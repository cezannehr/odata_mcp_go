// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package http

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// SecurityConfig holds security settings for HTTP transport
type SecurityConfig struct {
	Addr               string // HTTP server address (e.g., "localhost:8080")
	Token              string // MCP token for authentication
	TLSEnabled         bool   // Whether TLS is enabled
	TLSCert            string // Path to TLS certificate
	TLSKey             string // Path to TLS key
	AllowAllInterfaces bool   // Explicit flag to allow 0.0.0.0/::
	PerRequestAuth     bool   // Callers authenticate per request, so no shared token
	AllowPlainHTTP     bool   // Serve plain HTTP off loopback, for a private network or behind a TLS proxy
	AuthServer         string // Issuer MCP clients sign in with; empty publishes no OAuth metadata
}

// ValidateHTTPSecurity validates security configuration for HTTP transport.
// Returns an error if the configuration is insecure.
//
// Security model (strict by default):
// - Token is always required unless --no-token-localhost is set for loopback addresses
// - Non-localhost requires token + TLS, unless --allow-plain-http accepts the risk
// - Binding to all interfaces (0.0.0.0/::) requires explicit flag + token + TLS
func ValidateHTTPSecurity(cfg SecurityConfig) error {
	// Check for unspecified addresses (0.0.0.0, ::, or empty host)
	if IsUnspecifiedAddr(cfg.Addr) {
		if !cfg.AllowAllInterfaces {
			return fmt.Errorf("binding to all interfaces (0.0.0.0/::) requires --allow-all-interfaces flag")
		}
		if cfg.Token == "" && !cfg.PerRequestAuth {
			return fmt.Errorf("--mcp-token required when binding to all interfaces")
		}
		if !cfg.TLSEnabled && !cfg.AllowPlainHTTP {
			return fmt.Errorf("--tls required when binding to all interfaces (or --allow-plain-http behind a TLS-terminating proxy)")
		}
		return nil
	}

	// Localhost bindings: token always required
	if IsLoopbackAddr(cfg.Addr) {
		if cfg.Token == "" && !cfg.PerRequestAuth {
			return fmt.Errorf("--mcp-token required for HTTP transport")
		}
		return nil
	}

	// Non-localhost bindings: token + TLS required, no exceptions
	if cfg.Token == "" && !cfg.PerRequestAuth {
		return fmt.Errorf("--mcp-token required for non-localhost binding")
	}
	if !cfg.TLSEnabled && !cfg.AllowPlainHTTP {
		return fmt.Errorf("--tls required for non-localhost binding (or --allow-plain-http behind a TLS-terminating proxy)")
	}

	return nil
}

// IsLoopbackAddr checks if an address is a loopback address (localhost/127.x.x.x/::1)
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	host = strings.Trim(host, "[]")

	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)
	if ip != nil {
		return ip.IsLoopback()
	}

	// Check for 127.x shorthand (e.g., "127.1" = "127.0.0.1")
	if strings.HasPrefix(host, "127.") {
		return true
	}

	return false
}

// IsUnspecifiedAddr checks if an address binds to all interfaces (0.0.0.0, ::, or empty)
func IsUnspecifiedAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		if strings.HasPrefix(addr, ":") {
			return true
		}
		host = addr
	}

	if host == "" {
		return true
	}

	host = strings.Trim(host, "[]")

	ip := net.ParseIP(host)
	if ip != nil {
		return ip.IsUnspecified()
	}

	return false
}

// ValidateToken performs constant-time comparison of tokens to prevent timing attacks
func ValidateToken(provided, expected string) bool {
	if provided == "" && expected == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

// HealthPath is the unauthenticated health endpoint both HTTP transports serve.
const HealthPath = "/health"

// bearerScheme is matched case-insensitively, as RFC 7235 requires.
const bearerScheme = "bearer "

// AuthenticateRequest reports whether r presents the expected MCP token as a
// bearer credential. A blank expected token leaves the endpoint ungated.
func AuthenticateRequest(r *http.Request, expected string) bool {
	if expected == "" {
		return true
	}

	header := r.Header.Get("Authorization")
	if len(header) <= len(bearerScheme) || !strings.EqualFold(header[:len(bearerScheme)], bearerScheme) {
		return false
	}

	return ValidateToken(strings.TrimSpace(header[len(bearerScheme):]), expected)
}

// SecurityMiddleware gates next with cfg. The health endpoint and CORS
// preflight stay open; every other request must present the token.
func SecurityMiddleware(cfg SecurityConfig, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")

		if isLocalhost(r.Host) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept, Last-Event-ID, Authorization")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.URL.Path == HealthPath || (cfg.AuthServer != "" && r.URL.Path == ProtectedResourcePath) {
			next.ServeHTTP(w, r)
			return
		}

		// An ungated endpoint stays loopback-only, mirroring the configuration
		// ValidateHTTPSecurity enforces at startup.
		if cfg.Token == "" && !cfg.PerRequestAuth && !isLocalhost(r.RemoteAddr) && !isLocalhost(r.Host) {
			http.Error(w, "Remote connections require --mcp-token with --tls and --allow-all-interfaces", http.StatusForbidden)
			return
		}

		if !AuthenticateRequest(r, cfg.Token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="odata-mcp"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		if cfg.AuthServer != "" && r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", protectedResourceChallenge(r))
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// ListenAndServe starts srv, using TLS when cfg supplies a certificate.
func ListenAndServe(srv *http.Server, cfg SecurityConfig) error {
	if cfg.TLSEnabled {
		return srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	}

	return srv.ListenAndServe()
}
