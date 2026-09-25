package http

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zmcp/odata-mcp/internal/client"
	"github.com/zmcp/odata-mcp/internal/obs"
	"github.com/zmcp/odata-mcp/internal/transport"
)

// StreamableHTTPTransport implements the Transport interface for Streamable HTTP
// This is the modern MCP transport that combines HTTP POST with optional SSE streaming
type StreamableHTTPTransport struct {
	security       SecurityConfig
	server         *http.Server
	handler        transport.Handler
	mu             sync.RWMutex
	activeStreams  map[string]*streamContext
	forwardHeaders bool // Whether to forward HTTP headers to OData client
}

type streamContext struct {
	id       string
	writer   http.ResponseWriter
	flusher  http.Flusher
	done     chan struct{}
	lastSeen time.Time
}

// NewStreamableHTTP creates a new Streamable HTTP transport
func NewStreamableHTTP(security SecurityConfig, handler transport.Handler, forwardHeaders bool) *StreamableHTTPTransport {
	return &StreamableHTTPTransport{
		security:       security,
		handler:        handler,
		activeStreams:  make(map[string]*streamContext),
		forwardHeaders: forwardHeaders,
	}
}

// Start initializes the HTTP server and begins listening
func (t *StreamableHTTPTransport) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	// Main MCP endpoint - handles both regular POST and SSE upgrades
	mux.HandleFunc("/mcp", t.handleMCP)

	// Health check endpoint
	mux.HandleFunc(HealthPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(map[string]string{
			"status":    "ok",
			"transport": "streamable-http",
			"protocol":  "2024-11-05",
		}); err != nil {
			slog.Error("health check: failed to encode response", slog.String("error", err.Error()))
		}
	})

	// Legacy SSE endpoint for backward compatibility
	mux.HandleFunc("/sse", t.handleLegacySSE)

	t.server = newHTTPServer(t.security, mux)

	// Start cleanup routine for stale streams
	go t.cleanupStreams(ctx)

	// Start server
	go func() {
		if err := ListenAndServe(t.server, t.security); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", slog.String("error", err.Error()))
		}
	}()

	<-ctx.Done()
	return t.Close()
}

// handleMCP handles the main MCP endpoint with automatic SSE upgrade
func (t *StreamableHTTPTransport) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check if client wants SSE streaming
	acceptSSE := strings.Contains(r.Header.Get("Accept"), "text/event-stream")
	lastEventID := r.Header.Get("Last-Event-ID")

	// Parse the incoming message
	var msg transport.Message
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		obs.From(r.Context()).Add(slog.String("error", err.Error()))
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
	}

	record := obs.From(r.Context())
	record.Add(describeCall(&msg)...)

	// Enrich context with HTTP headers for forwarding to OData service (if enabled)
	ctx := r.Context()
	if t.forwardHeaders {
		// Clone headers to avoid any modification issues
		headers := r.Header.Clone()
		// The gate token authenticates this hop only, so it never reaches OData.
		if t.security.Token != "" {
			headers.Del("Authorization")
		}
		ctx = context.WithValue(ctx, client.HTTPHeadersContextKey, headers)
	}

	// Process the message with enriched context
	response, err := t.handler(ctx, &msg)
	if err != nil {
		response = &transport.Message{
			JSONRPC: "2.0",
			ID:      msg.ID,
			Error: &transport.Error{
				Code:    -32603,
				Message: err.Error(),
			},
		}
	}

	if response != nil && response.Error != nil {
		record.Add(
			slog.Int("rpc_error_code", response.Error.Code),
			slog.String("rpc_error", loggableRPCError(msg.Method, response.Error)),
		)
	}

	// Check if this is a method that might benefit from streaming
	needsStreaming := t.shouldUpgradeToStream(&msg, response)

	if acceptSSE && needsStreaming {
		// Upgrade to SSE for streaming responses
		t.upgradeToSSE(w, response, lastEventID)
	} else {
		// Regular JSON response
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			slog.Error("Error encoding response", slog.String("error", err.Error()))
		}
	}
}

// describeCall names what the request asked for, at the level of the tool
// and, for the universal tool, its action and target. Those are method and
// entity-set names, never row data: the arguments' filters and payloads are
// left out because they carry whatever the caller searched for or wrote.
func describeCall(msg *transport.Message) []slog.Attr {
	attrs := []slog.Attr{slog.String("rpc_method", msg.Method)}
	if msg.Method != "tools/call" || len(msg.Params) == 0 {
		return attrs
	}

	var params struct {
		Name      string `json:"name"`
		Arguments struct {
			Action string `json:"action"`
			Target string `json:"target"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return attrs
	}

	if params.Name != "" {
		attrs = append(attrs, slog.String("tool", params.Name))
	}
	if params.Arguments.Action != "" {
		attrs = append(attrs, slog.String("action", params.Arguments.Action))
	}
	if params.Arguments.Target != "" {
		attrs = append(attrs, slog.String("target", params.Arguments.Target))
	}

	return attrs
}

// credentialErrorCode is what the multi-tenant runner answers when a request's
// credential is refused or its bridge cannot be built.
const credentialErrorCode = -32001

// toolFailureMarker stands in for a tool failure's message on the request line.
const toolFailureMarker = "tool call failed"

// loggableRPCError decides how much of a JSON-RPC error goes on the request
// line. A failed tool call carries the OData service's own error text, and a
// validation error from an HR service can echo the value that was submitted,
// so those are reduced to a marker: the code says what class it was, and the
// upstream line for the same request id has the HTTP status. Everything else
// is the server's own wording, or a credential refusal naming at most a URL.
func loggableRPCError(method string, e *transport.Error) string {
	if method == "tools/call" && e.Code != credentialErrorCode {
		return toolFailureMarker
	}

	return e.Message
}

// shouldUpgradeToStream determines if a request should be upgraded to SSE
func (t *StreamableHTTPTransport) shouldUpgradeToStream(request, response *transport.Message) bool {
	// Check if the response indicates streaming would be beneficial
	// This could be based on response size, method type, or explicit flags

	// For now, check if it's a method that typically streams
	streamingMethods := []string{
		"tools/call",
		"resources/read",
		"prompts/get",
	}

	for _, method := range streamingMethods {
		if strings.Contains(request.Method, method) {
			return true
		}
	}

	// Check if response has pagination or continuation indicators
	if response != nil && response.Result != nil {
		var result map[string]interface{}
		if err := json.Unmarshal(response.Result, &result); err == nil {
			if _, ok := result["has_more"]; ok {
				return true
			}
			if _, ok := result["continuation_token"]; ok {
				return true
			}
		}
	}

	return false
}

// upgradeToSSE writes the response as an SSE stream and closes it, as the spec
// requires; holding it open parked a connection per tool call on every proxy hop.
func (t *StreamableHTTPTransport) upgradeToSSE(w http.ResponseWriter, initialResponse *transport.Message, lastEventID string) {
	// Ensure we can flush
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Fall back to regular response
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(initialResponse); err != nil {
			slog.Error("upgradeToSSE: failed to encode fallback response", slog.String("error", err.Error()))
		}
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // Disable Nginx buffering

	// Create stream context
	stream := &streamContext{
		id:       fmt.Sprintf("stream-%d", time.Now().UnixNano()),
		writer:   w,
		flusher:  flusher,
		done:     make(chan struct{}),
		lastSeen: time.Now(),
	}

	// Register stream
	t.mu.Lock()
	t.activeStreams[stream.id] = stream
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		delete(t.activeStreams, stream.id)
		t.mu.Unlock()
		close(stream.done)
	}()

	// Send initial response as first event
	if initialResponse != nil {
		if err := t.sendSSEMessage(stream, "message", initialResponse); err != nil {
			slog.Error("upgradeToSSE: failed to send initial response", slog.String("error", err.Error()))
		}
	}

	// Handle resume from last event if provided
	if lastEventID != "" {
		// In a real implementation, you'd replay missed events here
		if err := t.sendSSEMessage(stream, "resume", map[string]string{
			"last_event_id": lastEventID,
			"status":        "resumed",
		}); err != nil {
			slog.Error("upgradeToSSE: failed to send resume message", slog.String("error", err.Error()))
		}
	}
}

// sendSSEMessage sends a message in SSE format
func (t *StreamableHTTPTransport) sendSSEMessage(stream *streamContext, eventType string, data interface{}) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}

	// Generate event ID
	eventID := fmt.Sprintf("%s-%d", stream.id, time.Now().UnixNano())

	// Send SSE formatted message
	_, err = fmt.Fprintf(stream.writer, "id: %s\nevent: %s\ndata: %s\n\n",
		eventID, eventType, jsonData)
	if err != nil {
		return err
	}

	stream.flusher.Flush()
	stream.lastSeen = time.Now()
	return nil
}

// handleLegacySSE handles the legacy /sse endpoint for backward compatibility
func (t *StreamableHTTPTransport) handleLegacySSE(w http.ResponseWriter, r *http.Request) {
	// Check if the request accepts SSE
	if r.Header.Get("Accept") != "text/event-stream" {
		http.Error(w, "SSE not supported", http.StatusBadRequest)
		return
	}

	// Redirect to main MCP endpoint with SSE accept header
	r.Header.Set("Accept", "text/event-stream")
	t.handleMCP(w, r)
}

// cleanupStreams removes stale stream contexts
func (t *StreamableHTTPTransport) cleanupStreams(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.mu.Lock()
			now := time.Now()
			for id, stream := range t.activeStreams {
				if now.Sub(stream.lastSeen) > 5*time.Minute {
					close(stream.done)
					delete(t.activeStreams, id)
				}
			}
			t.mu.Unlock()
		}
	}
}

// BroadcastMessage sends a message to all active SSE streams
func (t *StreamableHTTPTransport) BroadcastMessage(msg *transport.Message) error {
	t.mu.RLock()
	defer t.mu.RUnlock()

	for _, stream := range t.activeStreams {
		go func(s *streamContext) {
			if err := t.sendSSEMessage(s, "broadcast", msg); err != nil {
				slog.Error("BroadcastMessage: failed to send to stream", slog.String("stream", s.id), slog.String("error", err.Error()))
			}
		}(stream)
	}

	return nil
}

// ReadMessage reads a message from stdin (for stdio compatibility during testing)
func (t *StreamableHTTPTransport) ReadMessage() (*transport.Message, error) {
	// For HTTP transport, we don't read from stdin
	// This is here for interface compatibility
	scanner := bufio.NewScanner(io.LimitReader(http.NoBody, 0))
	if scanner.Scan() {
		var msg transport.Message
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			return nil, err
		}
		return &msg, nil
	}
	return nil, io.EOF
}

// WriteMessage writes a message (used for broadcasting)
func (t *StreamableHTTPTransport) WriteMessage(msg *transport.Message) error {
	return t.BroadcastMessage(msg)
}

// Close gracefully shuts down the HTTP server
func (t *StreamableHTTPTransport) Close() error {
	if t.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return t.server.Shutdown(ctx)
	}
	return nil
}

// isLocalhost checks if an address is localhost
func isLocalhost(addr string) bool {
	return strings.HasPrefix(addr, "127.") ||
		strings.HasPrefix(addr, "localhost") ||
		strings.HasPrefix(addr, "[::1]") ||
		strings.HasPrefix(addr, "::1")
}
