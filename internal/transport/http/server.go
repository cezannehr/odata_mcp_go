package http

import (
	"net/http"
	"time"
)

const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 60 * time.Second
	// Longer than a load balancer's idle timeout, so the balancer closes an
	// idle keep-alive first rather than reusing one the server just dropped.
	idleTimeout = 180 * time.Second
	// A JSON-RPC request is a tool call, not a payload; 1 MiB is generous.
	maxRequestBody = 1 << 20
)

// newHTTPServer builds the server both HTTP transports listen on. No write
// timeout: a tool call runs as long as the OData client allows.
func newHTTPServer(security SecurityConfig, mux http.Handler) *http.Server {
	return &http.Server{
		Addr:              security.Addr,
		Handler:           SecurityMiddleware(security, mux),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
	}
}
