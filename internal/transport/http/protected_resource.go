// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package http

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

const ProtectedResourcePath = "/.well-known/oauth-protected-resource/mcp"

// origin is taken from the request so it is right behind a TLS-terminating
// proxy without being configured.
func origin(r *http.Request) string {
	scheme := r.Header.Get("X-Forwarded-Proto")
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
	return fmt.Sprintf(`Bearer resource_metadata="%s%s"`, origin(r), ProtectedResourcePath)
}

func ProtectedResourceHandler(authorizationServer string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"resource":                 origin(r) + "/mcp",
			"authorization_servers":    []string{authorizationServer},
			"bearer_methods_supported": []string{"header"},
		}); err != nil {
			slog.Error("protected resource: failed to encode metadata", slog.String("error", err.Error()))
		}
	}
}
