// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package client

import (
	"net/http"
	"strings"
	"testing"
)

const probeSecret = "probe-secret-must-not-appear"

func credentialHeaders() http.Header {
	h := http.Header{}
	h.Set("Authorization", "Basic "+probeSecret)
	h.Set("Cookie", "session=abc")
	h.Set("X-Custom-Passthrough", "keep-me")
	return h
}

func TestRedactHeadersHidesEveryCredential(t *testing.T) {
	redacted := RedactHeaders(credentialHeaders())

	rendered := ""
	for name, values := range redacted {
		rendered += name + ": " + strings.Join(values, ",") + "\n"
	}

	for _, secret := range []string{probeSecret, "session=abc"} {
		if strings.Contains(rendered, secret) {
			t.Errorf("RedactHeaders() leaked %q:\n%s", secret, rendered)
		}
	}

	if got := redacted.Get("X-Custom-Passthrough"); got != "keep-me" {
		t.Errorf("X-Custom-Passthrough = %q, want non-credential headers left readable", got)
	}
}

func TestRedactHeadersDoesNotMutateTheOriginal(t *testing.T) {
	original := credentialHeaders()
	_ = RedactHeaders(original)

	if got := original.Get("Authorization"); got != "Basic "+probeSecret {
		t.Errorf("RedactHeaders() mutated its argument: Authorization is now %q", got)
	}
}
