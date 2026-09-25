// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestInitRejectsUnknownFormat(t *testing.T) {
	if err := initTo(&bytes.Buffer{}, "yaml", slog.LevelInfo); err == nil {
		t.Fatal("initTo(yaml) error = nil, want an error")
	}
}

func TestInitJSONWritesOneObjectPerLine(t *testing.T) {
	var out bytes.Buffer
	if err := initTo(&out, FormatJSON, slog.LevelInfo); err != nil {
		t.Fatalf("initTo() error = %v", err)
	}

	slog.Info("hello", slog.String("k", "v"))

	var line map[string]any
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	if line["msg"] != "hello" || line["k"] != "v" {
		t.Errorf("line = %v, want msg=hello k=v", line)
	}
}

func TestInitHonoursTheLevel(t *testing.T) {
	var out bytes.Buffer
	if err := initTo(&out, FormatJSON, slog.LevelWarn); err != nil {
		t.Fatalf("initTo() error = %v", err)
	}

	slog.Info("quiet")
	slog.Warn("loud")

	if got := out.String(); strings.Contains(got, "quiet") || !strings.Contains(got, "loud") {
		t.Errorf("output = %q, want only the warning", got)
	}
}

func TestParseLevel(t *testing.T) {
	for name, want := range map[string]slog.Level{"debug": slog.LevelDebug, "INFO": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError} {
		got, err := ParseLevel(name)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", name, got, err, want)
		}
	}
	if _, err := ParseLevel("loud"); err == nil {
		t.Error("ParseLevel(loud) error = nil, want an error")
	}
}

func TestNewRequestMintsAnIDWhenNoneGiven(t *testing.T) {
	a, b := NewRequest(""), NewRequest("")
	if a.ID == "" || a.ID == b.ID {
		t.Errorf("ids = %q, %q; want two distinct non-empty ids", a.ID, b.ID)
	}

	if got := NewRequest("given").ID; got != "given" {
		t.Errorf("ID = %q, want the supplied id", got)
	}
}

func TestNilRequestIsSafe(t *testing.T) {
	var r *Request
	r.SetTenant("x")
	r.Add(slog.String("k", "v"))
	if r.Attrs() != nil || Correlation(r) != nil {
		t.Error("nil request produced attributes")
	}
	if From(context.Background()) != nil {
		t.Error("From(empty ctx) != nil")
	}
}

func TestAttrsLeadWithCorrelation(t *testing.T) {
	r := NewRequest("req-1")
	r.Add(slog.String("rpc_method", "tools/call"))
	r.SetTenant("abc")

	attrs := r.Attrs()
	if len(attrs) != 3 {
		t.Fatalf("len(attrs) = %d, want 3: %v", len(attrs), attrs)
	}
	if attrs[0].Key != "request_id" || attrs[0].Value.String() != "req-1" {
		t.Errorf("attrs[0] = %v, want request_id=req-1", attrs[0])
	}
	if attrs[1].Key != "tenant" || attrs[1].Value.String() != "abc" {
		t.Errorf("attrs[1] = %v, want tenant=abc", attrs[1])
	}
	if attrs[2].Key != "rpc_method" {
		t.Errorf("attrs[2] = %v, want rpc_method", attrs[2])
	}
}

func TestLogUpstreamDropsTheQueryAndCarriesCorrelation(t *testing.T) {
	var out bytes.Buffer
	if err := initTo(&out, FormatJSON, slog.LevelInfo); err != nil {
		t.Fatalf("initTo() error = %v", err)
	}

	r := NewRequest("req-9")
	r.SetTenant("t1")
	ctx := WithRequest(context.Background(), r)
	u, _ := url.Parse("https://svc.example.com/odata/People?$filter=Email%20eq%20'someone@example.com'")

	LogUpstream(ctx, "upstream.request", "GET", u, 200, nil, 42*time.Millisecond)

	var line map[string]any
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}

	if bytes.Contains(out.Bytes(), []byte("someone@example.com")) {
		t.Errorf("query string leaked into the log line: %s", out.String())
	}

	want := map[string]any{
		"event":      "upstream.request",
		"method":     "GET",
		"host":       "svc.example.com",
		"path":       "/odata/People",
		"status":     float64(200),
		"request_id": "req-9",
		"tenant":     "t1",
		"level":      "INFO",
	}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("%s = %v, want %v", k, line[k], v)
		}
	}
}

func TestLogUpstreamLevels(t *testing.T) {
	u, _ := url.Parse("https://svc.example.com/odata/")

	tests := []struct {
		name   string
		status int
		err    error
		want   string
	}{
		{"2xx is info", 200, nil, "INFO"},
		{"4xx is info: the caller's problem, reported to them", 404, nil, "INFO"},
		{"5xx is error", 503, nil, "ERROR"},
		{"transport failure is error", 0, errors.New("dial tcp: refused"), "ERROR"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := initTo(&out, FormatJSON, slog.LevelInfo); err != nil {
				t.Fatalf("initTo() error = %v", err)
			}

			LogUpstream(context.Background(), "upstream.request", "GET", u, tt.status, tt.err, time.Millisecond)

			var line map[string]any
			if err := json.Unmarshal(out.Bytes(), &line); err != nil {
				t.Fatalf("output is not JSON: %v", err)
			}
			if line["level"] != tt.want {
				t.Errorf("level = %v, want %s", line["level"], tt.want)
			}
			if tt.err != nil && line["error"] != tt.err.Error() {
				t.Errorf("error = %v, want %q", line["error"], tt.err)
			}
		})
	}
}
