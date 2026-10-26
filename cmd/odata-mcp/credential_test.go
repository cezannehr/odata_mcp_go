// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	nethttp "net/http"
	"testing"

	"github.com/zmcp/odata-mcp/internal/registry"
	"github.com/zmcp/odata-mcp/internal/transport"
)

func TestRefusedCredentialBecomesAChallengeOnlyWhenSignInIsAvailable(t *testing.T) {
	msg := &transport.Message{JSONRPC: "2.0", ID: []byte("1")}
	refusal := &transport.CredentialRejectedError{Err: errors.New("failed to fetch metadata: HTTP 401: token expired"), TokenRefused: true}

	_, err := rejectCredential(msg, refusal, true)
	var rejected *transport.CredentialRejectedError
	if !errors.As(err, &rejected) || !rejected.TokenRefused {
		t.Fatalf("err = %v, want the refusal handed to the transport to answer with 401", err)
	}

	response, err := rejectCredential(msg, refusal, false)
	if err != nil || response.Error == nil || response.Error.Code != -32001 {
		t.Fatalf("got (%v, %v), want the JSON-RPC -32001 error as before", response, err)
	}
}

func TestOnlyAnUpstream401CountsAsARefusedCredential(t *testing.T) {
	cases := map[string]bool{
		"failed to fetch metadata: HTTP 401: Unauthorized":                       true,
		"OData error (HTTP 401): The access token is invalid.":                   true,
		"OData error (HTTP 403): forbidden":                                      false,
		"dial tcp 10.0.0.1:443: connection refused":                              false,
		"failed to fetch metadata: HTTP 500: <p>upstream said HTTP 401 once</p>": false,
	}
	for message, want := range cases {
		if got := refusedByService(message); got != want {
			t.Errorf("refusedByService(%q) = %v, want %v", message, got, want)
		}
	}
}

func TestOnlyBasicCallersAreKeptOffTheOAuthChallenge(t *testing.T) {
	cases := map[string]bool{
		"":                    true,
		"Bearer some-token":   true,
		"Basic aWQ6c2VjcmV0":  false,
		"basic aWQ6c2VjcmV0":  false,
		"Token something-odd": true,
	}
	for header, want := range cases {
		headers := nethttp.Header{}
		if header != "" {
			headers.Set("Authorization", header)
		}
		if got := signsInWithOAuth(headers); got != want {
			t.Errorf("signsInWithOAuth(%q) = %v, want %v", header, got, want)
		}
	}
}

func TestA401OnOneCallIsCheckedAgainstTheMetadata(t *testing.T) {
	creds := registry.Credentials{ServiceURL: "https://odata.example.com/svc/", BearerToken: "token"}

	tests := []struct {
		name     string
		rebuild  error
		wantDead bool
	}{
		{"metadata refuses the token too", errors.New("failed to fetch metadata: HTTP 401: expired"), true},
		{"metadata still reads, so only that resource was refused", nil, false},
		{"metadata fails for another reason", errors.New("failed to fetch metadata: HTTP 503: down"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builds := 0
			bridges := registry.New(func(ctx context.Context, creds registry.Credentials) (registry.Bridge, error) {
				builds++
				if builds == 1 {
					return fakeBridge{}, nil
				}
				return fakeBridge{}, tt.rebuild
			})
			if _, err := bridges.For(context.Background(), creds); err != nil {
				t.Fatalf("first build: %v", err)
			}

			if got := tokenRefused(context.Background(), bridges, creds); got != tt.wantDead {
				t.Errorf("tokenRefused = %v, want %v", got, tt.wantDead)
			}
			if builds != 2 {
				t.Errorf("builds = %d, want the cached bridge rebuilt once", builds)
			}
		})
	}
}

type fakeBridge struct{}

func (fakeBridge) HandleMessage(context.Context, *transport.Message) (*transport.Message, error) {
	return nil, nil
}
