// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The registry logs through slog.Default unless told otherwise, and the older
// tests do not tell it otherwise. Keep their output out of the test log.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// logLines collects JSON lines from a logger that may be written from more
// than one goroutine.
type logLines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.Write(p)
}

func (l *logLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.String()
}

func (l *logLines) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(l, nil))
}

func (l *logLines) events(t *testing.T) []map[string]any {
	t.Helper()

	var out []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(l.String()), "\n") {
		if raw == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, raw)
		}
		out = append(out, line)
	}

	return out
}

func (l *logLines) find(t *testing.T, event, reason string) map[string]any {
	t.Helper()

	for _, line := range l.events(t) {
		if line["event"] == event && (reason == "" || line["reason"] == reason) {
			return line
		}
	}
	t.Fatalf("no %s line with reason %q in:\n%s", event, reason, l.String())

	return nil
}

func TestTenantIDIsShortStableAndNotTheSecret(t *testing.T) {
	a := testCreds("a")

	if got := a.TenantID(); len(got) != 12 || got != a.TenantID() {
		t.Errorf("TenantID() = %q, want a stable 12-character id", got)
	}
	if a.TenantID() == testCreds("b").TenantID() {
		t.Error("two credentials share a tenant id")
	}
	if strings.Contains(a.TenantID(), a.ClientSecret) {
		t.Error("tenant id contains the secret")
	}

	rotated := a
	rotated.ClientSecret = "rotated"
	if rotated.TenantID() == a.TenantID() {
		t.Error("a rotated secret kept the same tenant id, so its lines would be indistinguishable")
	}
}

func TestBuildIsLoggedWithHostAndNoCredential(t *testing.T) {
	var logs logLines
	var calls int64
	r := New(countingFactory(&calls), WithLogger(logs.logger()))

	creds := testCreds("a")
	if _, err := r.For(context.Background(), creds); err != nil {
		t.Fatalf("For() error = %v", err)
	}

	line := logs.find(t, "registry.build", "")
	if line["tenant"] != creds.TenantID() || line["host"] != "svc.example.com" || line["entries"] != float64(1) {
		t.Errorf("build line = %v", line)
	}
	if strings.Contains(logs.String(), creds.ClientSecret) {
		t.Errorf("secret appeared in the log: %s", logs.String())
	}
}

func TestFailedBuildIsLoggedAndDropped(t *testing.T) {
	var logs logLines
	boom := errors.New("metadata: 503")
	r := New(func(context.Context, Credentials) (Bridge, error) { return nil, boom }, WithLogger(logs.logger()))

	if _, err := r.For(context.Background(), testCreds("a")); !errors.Is(err, boom) {
		t.Fatalf("For() error = %v, want %v", err, boom)
	}

	if line := logs.find(t, "registry.build", ""); line["level"] != "WARN" || line["error"] != boom.Error() {
		t.Errorf("build line = %v, want WARN with the error", line)
	}
	logs.find(t, "registry.evict", evictFailed)
}

func TestEvictionReasonsAreLogged(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	var logs logLines
	var calls int64
	r := New(countingFactory(&calls),
		WithLogger(logs.logger()),
		WithClock(clock),
		WithMaxEntries(2),
		WithTTL(10*time.Minute),
		WithMaxAge(time.Hour),
	)
	ctx := context.Background()

	// a and b fill the cache; c forces the least recently used, a, out.
	for _, id := range []string{"a", "b", "c"} {
		if _, err := r.For(ctx, testCreds(id)); err != nil {
			t.Fatalf("For(%s) error = %v", id, err)
		}
		now = now.Add(time.Second)
	}
	lru := logs.find(t, "registry.evict", evictLRU)
	if lru["tenant"] != testCreds("a").TenantID() || lru["entries"] != float64(1) {
		t.Errorf("lru line = %v", lru)
	}

	// b sits idle past the TTL and goes when room is next needed.
	now = now.Add(11 * time.Minute)
	if _, err := r.For(ctx, testCreds("c")); err != nil {
		t.Fatalf("For(c) error = %v", err)
	}
	now = now.Add(time.Second)
	if _, err := r.For(ctx, testCreds("d")); err != nil {
		t.Fatalf("For(d) error = %v", err)
	}
	logs.find(t, "registry.evict", evictIdle)

	// c passes max age and is rebuilt on its next request.
	now = now.Add(2 * time.Hour)
	if _, err := r.For(ctx, testCreds("c")); err != nil {
		t.Fatalf("For(c) error = %v", err)
	}
	logs.find(t, "registry.evict", evictAged)

	r.Evict(testCreds("c"))
	logs.find(t, "registry.evict", evictAsked)
}

func TestFullRegistryIsLogged(t *testing.T) {
	var logs logLines
	release := make(chan struct{})
	r := New(func(context.Context, Credentials) (Bridge, error) {
		<-release
		return &fakeBridge{}, nil
	}, WithLogger(logs.logger()), WithMaxEntries(1))

	var building sync.WaitGroup
	building.Add(1)
	go func() {
		defer building.Done()
		_, _ = r.For(context.Background(), testCreds("a"))
	}()
	for r.Len() == 0 {
		time.Sleep(time.Millisecond)
	}

	if _, err := r.For(context.Background(), testCreds("b")); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("For(b) error = %v, want ErrNoCapacity", err)
	}
	close(release)
	building.Wait()

	if line := logs.find(t, "registry.full", ""); line["level"] != "WARN" || line["max"] != float64(1) {
		t.Errorf("full line = %v", line)
	}
}
