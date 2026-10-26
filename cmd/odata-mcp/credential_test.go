// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"testing"

	"github.com/zmcp/odata-mcp/internal/transport"
)

func TestRefusedCredentialBecomesAChallengeOnlyWhenSignInIsAvailable(t *testing.T) {
	msg := &transport.Message{JSONRPC: "2.0", ID: []byte("1")}
	refusal := errors.New("failed to fetch metadata: HTTP 401: token expired")

	_, err := rejectCredential(msg, refusal, true)
	var rejected *transport.CredentialRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("err = %v, want a CredentialRejectedError the transport answers with 401", err)
	}

	response, err := rejectCredential(msg, refusal, false)
	if err != nil || response.Error == nil || response.Error.Code != -32001 {
		t.Fatalf("got (%v, %v), want the JSON-RPC -32001 error as before", response, err)
	}
}

func TestRejectingAFailedToolCallKeepsItsErrorWithoutSignIn(t *testing.T) {
	failed := &transport.Message{JSONRPC: "2.0", ID: []byte("1"), Error: &transport.Error{Code: -32603, Message: "OData error (HTTP 401): expired"}}

	response, err := rejectCredential(failed, errors.New(failed.Error.Message), false)
	if err != nil || response != failed {
		t.Errorf("got (%v, %v), want the tool call's own response untouched", response, err)
	}
}

func TestOnlyAnUpstream401CountsAsARefusedCredential(t *testing.T) {
	cases := map[string]bool{
		"failed to fetch metadata: HTTP 401: Unauthorized":     true,
		"OData error (HTTP 401): The access token is invalid.": true,
		"OData error (HTTP 403): forbidden":                    false,
		"dial tcp 10.0.0.1:443: connection refused":            false,
	}
	for message, want := range cases {
		if got := refusedByService(message); got != want {
			t.Errorf("refusedByService(%q) = %v, want %v", message, got, want)
		}
	}
}
