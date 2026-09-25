// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

// Package obs is the server's observability seam: it configures the process
// logger and carries a per-request record that every layer can add to, so
// each HTTP request ends in one log line holding everything known about it.
package obs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Formats accepted by Init.
const (
	FormatText = "text"
	FormatJSON = "json"
)

// Init installs the process logger. Everything logs through slog.Default, and
// the standard log package is routed there too, so a plain log.Printf lands
// in the same stream with the same shape.
//
// Output is stderr: on the stdio transport stdout is the MCP channel, and a
// log line there would corrupt it.
func Init(format string, level slog.Level) error {
	return initTo(os.Stderr, format, level)
}

// ParseLevel reads a level name as given on the command line.
func ParseLevel(name string) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.TrimSpace(name))); err != nil {
		return 0, fmt.Errorf("unknown log level %q: use debug, info, warn or error", name)
	}

	return level, nil
}

func initTo(w io.Writer, format string, level slog.Level) error {
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch strings.ToLower(strings.TrimSpace(format)) {
	case FormatJSON:
		handler = slog.NewJSONHandler(w, opts)
	case FormatText, "":
		handler = slog.NewTextHandler(w, opts)
	default:
		return fmt.Errorf("unknown log format %q: use %s or %s", format, FormatText, FormatJSON)
	}

	slog.SetDefault(slog.New(handler))

	return nil
}

// Request is what one HTTP request accumulates on its way through the server.
// The transport creates it, handlers add to it, and the transport emits it
// once when the response has been written.
type Request struct {
	// ID is echoed to the caller and stamped on every line the request
	// produces, so a caller's report can be matched to the server's view.
	ID string

	mu     sync.Mutex
	tenant string
	attrs  []slog.Attr
}

type ctxKey struct{}

// NewRequest returns a record with the given id, or a fresh random one.
func NewRequest(id string) *Request {
	if id == "" {
		id = newID()
	}

	return &Request{ID: id}
}

// WithRequest attaches r to ctx.
func WithRequest(ctx context.Context, r *Request) context.Context {
	return context.WithValue(ctx, ctxKey{}, r)
}

// From returns the request record on ctx, or nil. Every method is safe on a
// nil receiver, so callers need not check.
func From(ctx context.Context) *Request {
	if ctx == nil {
		return nil
	}

	r, _ := ctx.Value(ctxKey{}).(*Request)

	return r
}

// SetTenant records which cached bridge served the request. It is an opaque
// digest, never a credential, and it is the join key between this request's
// line and the registry's build and eviction lines.
func (r *Request) SetTenant(id string) {
	if r == nil {
		return
	}

	r.mu.Lock()
	r.tenant = id
	r.mu.Unlock()
}

// Add appends attributes to the final line. Later values for the same key
// win when the handler renders, which is what a retry or a fallback wants.
func (r *Request) Add(attrs ...slog.Attr) {
	if r == nil {
		return
	}

	r.mu.Lock()
	r.attrs = append(r.attrs, attrs...)
	r.mu.Unlock()
}

// Attrs returns everything accumulated so far, correlation first.
func (r *Request) Attrs() []slog.Attr {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]slog.Attr, 0, len(r.attrs)+2)
	out = append(out, r.correlationLocked()...)
	out = append(out, r.attrs...)

	return out
}

// Correlation returns just the ids, for lines logged mid-request by lower
// layers that want to be findable from the request line.
func Correlation(r *Request) []slog.Attr {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.correlationLocked()
}

func (r *Request) correlationLocked() []slog.Attr {
	attrs := []slog.Attr{slog.String("request_id", r.ID)}
	if r.tenant != "" {
		attrs = append(attrs, slog.String("tenant", r.tenant))
	}

	return attrs
}

// LogUpstream records one call the bridge made to an OData service or its
// token endpoint. Only the path is logged, never the query: a $filter carries
// whatever the caller searched for, which for an HR service is personal data.
func LogUpstream(ctx context.Context, event, method string, u *url.URL, status int, err error, elapsed time.Duration) {
	attrs := []slog.Attr{
		slog.String("event", event),
		slog.String("method", method),
		slog.String("host", u.Host),
		slog.String("path", u.Path),
		slog.Int64("duration_ms", elapsed.Milliseconds()),
	}
	attrs = append(attrs, Correlation(From(ctx))...)

	level := slog.LevelInfo
	if err != nil {
		level = slog.LevelError
		attrs = append(attrs, slog.String("error", err.Error()))
	} else {
		if status >= 500 {
			level = slog.LevelError
		}
		attrs = append(attrs, slog.Int("status", status))
	}

	slog.Default().LogAttrs(ctx, level, "upstream", attrs...)
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}

	return hex.EncodeToString(b[:])
}
