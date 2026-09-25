// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package http

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zmcp/odata-mcp/internal/obs"
	"github.com/zmcp/odata-mcp/internal/transport"
)

// captureLogs points the default logger at a buffer for the test and returns
// a parser for the single line expected.
func captureLogs(t *testing.T) func() map[string]any {
	t.Helper()

	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return func() map[string]any {
		t.Helper()
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != 1 || lines[0] == "" {
			t.Fatalf("want exactly one log line, got %d:\n%s", len(lines), out.String())
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, lines[0])
		}
		return line
	}
}

func TestRequestLoggingEmitsOneLineWithWhatHandlersAdded(t *testing.T) {
	parse := captureLogs(t)

	handler := RequestLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record := obs.From(r.Context())
		if record == nil {
			t.Fatal("no request record on the context")
		}
		record.SetTenant("abc123")
		record.Add(slog.String("rpc_method", "tools/call"))
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("short and stout"))
	}))

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	req.Header.Set("User-Agent", "claude-code/1.0")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	id := rec.Header().Get(RequestIDHeader)
	if id == "" {
		t.Fatal("no X-Request-Id on the response")
	}

	line := parse()
	want := map[string]any{
		"event":      "http.request",
		"method":     "POST",
		"path":       "/mcp",
		"status":     float64(http.StatusTeapot),
		"bytes":      float64(len("short and stout")),
		"client_ip":  "203.0.113.9",
		"user_agent": "claude-code/1.0",
		"request_id": id,
		"tenant":     "abc123",
		"rpc_method": "tools/call",
		"level":      "INFO",
	}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("%s = %v, want %v", k, line[k], v)
		}
	}
	if _, ok := line["duration_ms"]; !ok {
		t.Error("no duration_ms on the line")
	}
}

func TestRequestLoggingHonoursACallerSuppliedID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		kept bool
	}{
		{"plain id is kept", "alb-1-abc", true},
		{"too long is replaced", strings.Repeat("x", maxRequestIDLength+1), false},
		{"control characters are replaced", "abc\x00def", false},
		{"spaces are replaced", "abc def", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parse := captureLogs(t)

			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			req.Header.Set(RequestIDHeader, tt.id)
			rec := httptest.NewRecorder()
			RequestLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(rec, req)

			got := rec.Header().Get(RequestIDHeader)
			if tt.kept && got != tt.id {
				t.Errorf("X-Request-Id = %q, want the caller's %q", got, tt.id)
			}
			if !tt.kept && (got == tt.id || got == "") {
				t.Errorf("X-Request-Id = %q, want a fresh id", got)
			}
			if line := parse(); line["request_id"] != got {
				t.Errorf("logged request_id = %v, want %q", line["request_id"], got)
			}
		})
	}
}

func TestRequestLoggingSkipsHealth(t *testing.T) {
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	req := httptest.NewRequest(http.MethodGet, HealthPath, nil)
	rec := httptest.NewRecorder()
	RequestLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)

	if out.Len() != 0 {
		t.Errorf("health check was logged: %s", out.String())
	}
}

func TestRequestLoggingLogsServerErrorsAtErrorLevel(t *testing.T) {
	parse := captureLogs(t)

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	rec := httptest.NewRecorder()
	RequestLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	})).ServeHTTP(rec, req)

	line := parse()
	if line["level"] != "ERROR" || line["status"] != float64(http.StatusBadGateway) {
		t.Errorf("level = %v status = %v, want ERROR 502", line["level"], line["status"])
	}
}

func TestRequestLoggingKeepsTheFlusher(t *testing.T) {
	captureLogs(t)

	flushed := false
	RequestLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("wrapped writer is not a Flusher; SSE would fall back to plain JSON")
		}
		f.Flush()
		flushed = true
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", nil))

	if !flushed {
		t.Error("handler did not run")
	}
}

func TestRequestLoggingIsWiredIntoTheServer(t *testing.T) {
	parse := captureLogs(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := newHTTPServer(SecurityConfig{Token: gateToken}, mux)

	// No token: the gate refuses it, and the refusal must still be logged.
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if line := parse(); line["status"] != float64(http.StatusUnauthorized) {
		t.Errorf("logged status = %v, want 401", line["status"])
	}
}

func TestHandleMCPRecordsTheCallAndItsError(t *testing.T) {
	parse := captureLogs(t)

	handler := func(ctx context.Context, msg *transport.Message) (*transport.Message, error) {
		return &transport.Message{
			JSONRPC: "2.0",
			ID:      msg.ID,
			Error:   &transport.Error{Code: -32001, Message: "registry: no OData service URL for this request"},
		}, nil
	}
	tr := NewStreamableHTTP(SecurityConfig{}, handler, true)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"odata","arguments":{"action":"list","target":"People","params":{"$filter":"Email eq 'x@y'"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	rec := httptest.NewRecorder()
	RequestLogging(http.HandlerFunc(tr.handleMCP)).ServeHTTP(rec, req)

	line := parse()
	want := map[string]any{
		"rpc_method":     "tools/call",
		"tool":           "odata",
		"action":         "list",
		"target":         "People",
		"rpc_error_code": float64(-32001),
		"rpc_error":      "registry: no OData service URL for this request",
	}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("%s = %v, want %v", k, line[k], v)
		}
	}
	for _, leaked := range []string{"$filter", "x@y"} {
		if strings.Contains(fmtLine(line), leaked) {
			t.Errorf("tool arguments leaked into the log line: %q", leaked)
		}
	}
}

func fmtLine(line map[string]any) string {
	b, _ := json.Marshal(line)
	return string(b)
}
