// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package http

import (
	"bufio"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/zmcp/odata-mcp/internal/obs"
)

// RequestIDHeader is read from the caller when present and always set on the
// response, so a load balancer's id or a client's own can be carried through.
const RequestIDHeader = "X-Request-Id"

const maxRequestIDLength = 64

// RequestLogging emits one line per request once the response is written,
// carrying whatever the handlers added to the request record on the way.
//
// It sits outside the security middleware so that a rejected request is
// logged too; an attacker probing the endpoint shows up as a run of 401s.
// The health endpoint is not logged at all: a load balancer polls it every
// few seconds per task and the lines would drown everything else.
func RequestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == HealthPath {
			next.ServeHTTP(w, r)
			return
		}

		record := obs.NewRequest(incomingRequestID(r))
		w.Header().Set(RequestIDHeader, record.ID)

		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()

		next.ServeHTTP(sw, r.WithContext(obs.WithRequest(r.Context(), record)))

		attrs := []slog.Attr{
			slog.String("event", "http.request"),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", sw.status()),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.Int64("bytes", sw.bytes),
			slog.String("client_ip", clientIP(r)),
			slog.String("user_agent", r.UserAgent()),
		}
		attrs = append(attrs, record.Attrs()...)

		level := slog.LevelInfo
		if sw.status() >= http.StatusInternalServerError {
			level = slog.LevelError
		}

		slog.Default().LogAttrs(r.Context(), level, "request", attrs...)
	})
}

// incomingRequestID accepts a caller-supplied id when it is short and
// printable, and otherwise leaves the record to mint one.
func incomingRequestID(r *http.Request) string {
	id := strings.TrimSpace(r.Header.Get(RequestIDHeader))
	if id == "" || len(id) > maxRequestIDLength {
		return ""
	}

	for _, c := range id {
		if c < 0x21 || c > 0x7e {
			return ""
		}
	}

	return id
}

// clientIP is the first address in X-Forwarded-For when a proxy set it,
// otherwise the peer. Behind a load balancer the peer is the balancer.
func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		first, _, _ := strings.Cut(forwarded, ",")
		if ip := strings.TrimSpace(first); ip != "" {
			return ip
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}

// statusWriter records what was written. It forwards Flush and Hijack so the
// SSE path, which needs a Flusher, behaves exactly as it did unwrapped.
type statusWriter struct {
	http.ResponseWriter
	code  int
	bytes int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)

	return n, err
}

func (w *statusWriter) status() int {
	if w.code == 0 {
		return http.StatusOK
	}

	return w.code
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}

	return nil, nil, errors.New("response writer does not support hijacking")
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
