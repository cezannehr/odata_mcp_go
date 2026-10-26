// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zmcp/odata-mcp/internal/transport"
)

const issuer = "https://auth.example.com"

func TestProtectedResourceMetadataIsOpenAndNamesTheIssuer(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle(ProtectedResourcePath, ProtectedResourceHandler(issuer))

	req := httptest.NewRequest(http.MethodGet, "http://mcp.example.com"+ProtectedResourcePath, nil)
	req.Header.Set("X-Forwarded-Proto", "https")

	rec := httptest.NewRecorder()
	cfg := SecurityConfig{PerRequestAuth: true, AuthServer: issuer}
	SecurityMiddleware(cfg, mux).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 without any credential", rec.Code)
	}

	var doc struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("metadata is not JSON: %v", err)
	}
	if doc.Resource != "https://mcp.example.com/mcp" {
		t.Errorf("resource = %q, want the /mcp URL as the caller reached it", doc.Resource)
	}
	if len(doc.AuthorizationServers) != 1 || doc.AuthorizationServers[0] != issuer {
		t.Errorf("authorization_servers = %v, want [%s]", doc.AuthorizationServers, issuer)
	}
}

func TestSecurityMiddlewareChallengesMissingCredential(t *testing.T) {
	tests := []struct {
		name          string
		authServer    string
		authHeader    string
		wantStatus    int
		wantChallenge string
	}{
		{"no credential", issuer, "", http.StatusUnauthorized, `Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/mcp"`},
		{"bearer token passes", issuer, "Bearer user-token", http.StatusOK, ""},
		{"basic credential still passes", issuer, "Basic aWQ6c2VjcmV0", http.StatusOK, ""},
		{"unconfigured leaves it to the handler", "", "", http.StatusOK, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://mcp.example.com/mcp", strings.NewReader("{}"))
			req.Header.Set("X-Forwarded-Proto", "https")
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}

			reached := false
			rec := httptest.NewRecorder()
			cfg := SecurityConfig{PerRequestAuth: true, AuthServer: tt.authServer}
			SecurityMiddleware(cfg, reachedHandler(&reached)).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != tt.wantChallenge {
				t.Errorf("WWW-Authenticate = %q, want %q", got, tt.wantChallenge)
			}
			if reached != (tt.wantStatus == http.StatusOK) {
				t.Errorf("handler reached = %v, want %v", reached, tt.wantStatus == http.StatusOK)
			}
		})
	}
}

func TestRejectedCredentialIsAnsweredWithAChallenge(t *testing.T) {
	handler := func(ctx context.Context, msg *transport.Message) (*transport.Message, error) {
		return nil, &transport.CredentialRejectedError{Err: errors.New("HTTP 401: token expired")}
	}
	tr := NewStreamableHTTP(SecurityConfig{PerRequestAuth: true, AuthServer: issuer}, handler, true)

	req := httptest.NewRequest(http.MethodPost, "http://mcp.example.com/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Authorization", "Bearer expired")
	rec := httptest.NewRecorder()
	tr.handleMCP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 so the client signs in again", rec.Code)
	}
	want := `Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/mcp"`
	if got := rec.Header().Get("WWW-Authenticate"); got != want {
		t.Errorf("WWW-Authenticate = %q, want %q", got, want)
	}
}
