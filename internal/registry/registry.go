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
}

type entry struct {
	once     sync.Once
	bridge   Bridge
	err      error
	lastUsed time.Time
	built    time.Time
	inFlight bool
}

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
		delete(r.entries, key)
		closeBridge(e)
		cached = false
	}
	if !cached {
		if err := r.makeRoomLocked(); err != nil {
			r.mu.Unlock()
			return nil, err
		}
		e = &entry{inFlight: true, built: r.now()}
		r.entries[key] = e
	}
	e.lastUsed = r.now()
	r.mu.Unlock()

	e.once.Do(func() {
		e.bridge, e.err = r.factory(ctx, creds)

		r.mu.Lock()
		e.inFlight = false
		r.mu.Unlock()
	})

	if e.err != nil {
		// Not cached, so a transient failure does not poison the whole TTL.
		r.forget(key, e)
		return nil, e.err
	}

	return e.bridge, nil
}

// Len reports how many bridges are currently cached.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.entries)
}

// Evict drops the bridge for creds, closing it if it is closeable.
func (r *Registry) Evict(creds Credentials) {
	key := creds.Key()

	r.mu.Lock()
	e := r.entries[key]
	delete(r.entries, key)
	r.mu.Unlock()

	closeBridge(e)
}

func (r *Registry) forget(key string, want *entry) {
	r.mu.Lock()
	if r.entries[key] == want {
		delete(r.entries, key)
	}
	r.mu.Unlock()
}

// makeRoomLocked drops expired entries and then, if still full, the least
// recently used idle one. Entries still building are never dropped.
func (r *Registry) makeRoomLocked() error {
	cutoff := r.now().Add(-r.ttl)

	for key, e := range r.entries {
		if (!e.inFlight && e.lastUsed.Before(cutoff)) || r.agedOutLocked(e) {
			delete(r.entries, key)
			closeBridge(e)
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

	delete(r.entries, oldestKey)
	closeBridge(oldest)

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
