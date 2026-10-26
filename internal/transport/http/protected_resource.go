// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package http

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
)

// ProtectedResourcePrefix is where RFC 9728 puts metadata: this prefix
// followed by the path of the resource it describes.
const ProtectedResourcePrefix = "/.well-known/oauth-protected-resource"

var protectedResourcePaths = []string{"/mcp", "/sse"}

func isProtectedResource(path string) bool {
	return slices.Contains(protectedResourcePaths, path)
}

func isProtectedResourceMetadata(path string) bool {
	return strings.HasPrefix(path, ProtectedResourcePrefix+"/")
}

// origin is taken from the request so it is right behind a TLS-terminating
// proxy without being configured.
func origin(r *http.Request) string {
	// Each proxy in a chain may append its own value; the first is what the
	// client used.
	scheme, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	scheme = strings.TrimSpace(scheme)
	if scheme == "" && r.TLS != nil {
		scheme = "https"
	}
	if scheme == "" {
		scheme = "http"
	}

	return scheme + "://" + r.Host
}

// protectedResourceChallenge is what makes an MCP client start the OAuth
// flow; a JSON-RPC error with HTTP 200 never does.
func protectedResourceChallenge(r *http.Request) string {
	return fmt.Sprintf(`Bearer resource_metadata="%s%s%s"`, origin(r), ProtectedResourcePrefix, r.URL.Path)
}

// RFC 6750 names the error only when a token was sent, which is how a client
// tells a refused token from a missing one.
func invalidTokenChallenge(r *http.Request) string {
	return fmt.Sprintf(`Bearer error="invalid_token", resource_metadata="%s%s%s"`, origin(r), ProtectedResourcePrefix, r.URL.Path)
}

func ProtectedResourceHandler(authorizationServer string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Public and credential-free, so any browser-based client may read it.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"resource":                 origin(r) + strings.TrimPrefix(r.URL.Path, ProtectedResourcePrefix),
			"authorization_servers":    []string{authorizationServer},
			"bearer_methods_supported": []string{"header"},
		}); err != nil {
			slog.Error("protected resource: failed to encode metadata", slog.String("error", err.Error()))
		}
	}
}
