// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

// Package registry keeps one bridge per set of caller credentials, so a single
// process can serve several OData services without knowing them at startup.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/zmcp/odata-mcp/internal/transport"
)

const (
	// DefaultTTL stops a rotated credential being held forever and lets stale
	// metadata be refetched.
	DefaultTTL = 30 * time.Minute

	// DefaultMaxEntries caps memory, since each bridge holds parsed metadata.
	DefaultMaxEntries = 64

	// DefaultMaxAge bounds how long a busy tenant keeps its parsed metadata, so
	// a schema change on the service shows up without a restart.
	DefaultMaxAge = 2 * time.Hour
)

// ErrNoCapacity is returned when every cached bridge is still building and the
// registry is at its limit.
var ErrNoCapacity = errors.New("registry: no capacity for another tenant")

// Credentials identify one OData service and how to authenticate to it. They
// are the cache key, so callers with different access never share a bridge.
type Credentials struct {
	ServiceURL   string
	BearerToken  string
	ClientID     string
	ClientSecret string
	TokenURL     string
	Scope        string
}

// Bridge is the part of an initialised bridge the registry hands back.
type Bridge interface {
	HandleMessage(ctx context.Context, msg *transport.Message) (*transport.Message, error)
}

// Factory builds and initialises a bridge for one set of credentials. It is
// expected to reach the service, so it may be slow and may fail.
type Factory func(ctx context.Context, creds Credentials) (Bridge, error)

// Registry caches bridges by credential.
type Registry struct {
	mu      sync.Mutex
	entries map[string]*entry
	factory Factory
	ttl     time.Duration
	maxAge  time.Duration
	max     int
	now     func() time.Time
	log     *slog.Logger
}

type entry struct {
	once     sync.Once
	bridge   Bridge
	err      error
	lastUsed time.Time
	built    time.Time
	inFlight bool

	// For the log lines: which tenant this is and where it points, with no
	// credential in either.
	tenant string
	host   string
}

// Why an entry left the cache, as logged.
const (
	evictIdle   = "idle"
	evictAged   = "aged"
	evictLRU    = "lru"
	evictFailed = "build-failed"
	evictAsked  = "requested"
)

// Option adjusts a Registry at construction.
type Option func(*Registry)

// WithTTL sets how long an unused bridge is kept.
func WithTTL(ttl time.Duration) Option {
	return func(r *Registry) { r.ttl = ttl }
}

// WithMaxAge sets how long a bridge is used before being rebuilt, however
// busy it is.
func WithMaxAge(maxAge time.Duration) Option {
	return func(r *Registry) { r.maxAge = maxAge }
}

// WithMaxEntries caps how many bridges are cached at once.
func WithMaxEntries(max int) Option {
	return func(r *Registry) { r.max = max }
}

// WithClock replaces the clock, for tests.
func WithClock(now func() time.Time) Option {
	return func(r *Registry) { r.now = now }
}

// WithLogger directs the registry's build and eviction lines somewhere other
// than the process default.
func WithLogger(log *slog.Logger) Option {
	return func(r *Registry) { r.log = log }
}

// New returns a Registry that builds bridges with factory.
func New(factory Factory, opts ...Option) *Registry {
	r := &Registry{
		entries: make(map[string]*entry),
		factory: factory,
		ttl:     DefaultTTL,
		maxAge:  DefaultMaxAge,
		max:     DefaultMaxEntries,
		now:     time.Now,
	}

	for _, opt := range opts {
		opt(r)
	}

	if r.log == nil {
		r.log = slog.Default()
	}

	return r
}

// For returns the bridge for creds, building it on first use. Concurrent first
// calls for the same credentials share one build rather than racing.
func (r *Registry) For(ctx context.Context, creds Credentials) (Bridge, error) {
	if err := creds.Validate(); err != nil {
		return nil, err
	}

	key := creds.Key()

	r.mu.Lock()
	e, cached := r.entries[key]
	if cached && r.agedOutLocked(e) {
		r.dropLocked(key, e, evictAged)
		cached = false
	}
	if !cached {
		if err := r.makeRoomLocked(); err != nil {
			r.log.Warn("registry",
				slog.String("event", "registry.full"),
				slog.String("tenant", creds.TenantID()),
				slog.String("host", creds.host()),
				slog.Int("entries", len(r.entries)),
				slog.Int("max", r.max))
			r.mu.Unlock()
			return nil, err
		}
		e = &entry{inFlight: true, built: r.now(), tenant: creds.TenantID(), host: creds.host()}
		r.entries[key] = e
	}
	e.lastUsed = r.now()
	r.mu.Unlock()

	e.once.Do(func() {
		start := r.now()
		e.bridge, e.err = r.factory(ctx, creds)

		r.mu.Lock()
		e.inFlight = false
		entries := len(r.entries)
		r.mu.Unlock()

		r.logBuild(e, r.now().Sub(start), entries)
	})

	if e.err != nil {
		// Not cached, so a transient failure does not poison the whole TTL.
		r.forget(key, e)
		return nil, e.err
	}

	return e.bridge, nil
}

// logBuild records that a bridge was built, or failed to be. Building is the
// expensive step, a metadata fetch and parse against the service, so how
// often it happens and how long it takes is the number to watch.
func (r *Registry) logBuild(e *entry, elapsed time.Duration, entries int) {
	attrs := []slog.Attr{
		slog.String("event", "registry.build"),
		slog.String("tenant", e.tenant),
		slog.String("host", e.host),
		slog.Int64("duration_ms", elapsed.Milliseconds()),
		slog.Int("entries", entries),
	}

	if e.err != nil {
		attrs = append(attrs, slog.String("error", e.err.Error()))
		r.log.LogAttrs(context.Background(), slog.LevelWarn, "registry", attrs...)
		return
	}

	r.log.LogAttrs(context.Background(), slog.LevelInfo, "registry", attrs...)
}

// dropLocked removes an entry and says why. The caller holds r.mu.
func (r *Registry) dropLocked(key string, e *entry, reason string) {
	delete(r.entries, key)
	closeBridge(e)

	if e == nil {
		return
	}

	r.log.Info("registry",
		slog.String("event", "registry.evict"),
		slog.String("reason", reason),
		slog.String("tenant", e.tenant),
		slog.String("host", e.host),
		slog.Int64("age_s", int64(r.now().Sub(e.built).Seconds())),
		slog.Int64("idle_s", int64(r.now().Sub(e.lastUsed).Seconds())),
		slog.Int("entries", len(r.entries)))
}

// Len reports how many bridges are currently cached.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.entries)
}

// Max reports the cap Len is measured against.
func (r *Registry) Max() int {
	return r.max
}

// Evict drops the bridge for creds, closing it if it is closeable.
func (r *Registry) Evict(creds Credentials) {
	key := creds.Key()

	r.mu.Lock()
	e := r.entries[key]
	r.dropLocked(key, e, evictAsked)
	r.mu.Unlock()
}

func (r *Registry) forget(key string, want *entry) {
	r.mu.Lock()
	if r.entries[key] == want {
		r.dropLocked(key, want, evictFailed)
	}
	r.mu.Unlock()
}

// makeRoomLocked drops expired entries and then, if still full, the least
// recently used idle one. Entries still building are never dropped.
func (r *Registry) makeRoomLocked() error {
	cutoff := r.now().Add(-r.ttl)

	for key, e := range r.entries {
		switch {
		case r.agedOutLocked(e):
			r.dropLocked(key, e, evictAged)
		case !e.inFlight && e.lastUsed.Before(cutoff):
			r.dropLocked(key, e, evictIdle)
		}
	}

	if len(r.entries) < r.max {
		return nil
	}

	var oldestKey string
	var oldest *entry

	for key, e := range r.entries {
		if e.inFlight {
			continue
		}
		if oldest == nil || e.lastUsed.Before(oldest.lastUsed) {
			oldestKey, oldest = key, e
		}
	}

	if oldest == nil {
		return ErrNoCapacity
	}

	r.dropLocked(oldestKey, oldest, evictLRU)

	return nil
}

// agedOutLocked reports whether a finished bridge has passed the max age.
// Idle expiry is measured from last use; this one is measured from build,
// so steady traffic cannot keep a stale schema alive.
func (r *Registry) agedOutLocked(e *entry) bool {
	return !e.inFlight && r.now().Sub(e.built) > r.maxAge
}

func closeBridge(e *entry) {
	if e == nil || e.bridge == nil {
		return
	}

	if closer, ok := e.bridge.(io.Closer); ok {
		_ = closer.Close()
	}
}

// Key digests every field, NUL separated so that shifting a character across a
// field boundary cannot collide.
func (c Credentials) Key() string {
	digest := sha256.New()

	for _, field := range []string{c.ServiceURL, c.BearerToken, c.ClientID, c.ClientSecret, c.TokenURL, c.Scope} {
		digest.Write([]byte(field))
		digest.Write([]byte{0})
	}

	return hex.EncodeToString(digest.Sum(nil))
}

// TenantID is a short, stable, non-reversible name for these credentials, for
// log lines. It is a prefix of the cache key, which is a digest of every
// field including the secret, so it identifies a tenant without naming one.
func (c Credentials) TenantID() string {
	return c.Key()[:12]
}

// host is the service host, the one part of the credentials that is safe to
// log as is.
func (c Credentials) host() string {
	u, err := url.Parse(c.ServiceURL)
	if err != nil {
		return ""
	}

	return u.Host
}

// Validate reports whether the credentials are usable.
func (c Credentials) Validate() error {
	if c.ServiceURL == "" {
		return fmt.Errorf("registry: no OData service URL for this request")
	}

	if c.BearerToken != "" {
		return nil
	}

	if c.ClientID == "" || c.ClientSecret == "" {
		return fmt.Errorf("registry: request carries neither a bearer token nor a client id and secret")
	}

	if c.TokenURL == "" {
		return fmt.Errorf("registry: client credentials given without a token endpoint")
	}

	return nil
}

// Redacted returns the credentials with secrets replaced, for logging.
func (c Credentials) Redacted() Credentials {
	if c.BearerToken != "" {
		c.BearerToken = "[redacted]"
	}
	if c.ClientSecret != "" {
		c.ClientSecret = "[redacted]"
	}

	return c
}
