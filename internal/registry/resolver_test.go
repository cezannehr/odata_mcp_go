// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package registry

import (
	"encoding/base64"
	"net/http"
	"testing"
)

func defaultResolver() Resolver {
	return Resolver{
		Service: Credentials{
			ServiceURL:   "https://tenant.example.com/odata/service.svc/",
			ClientID:     "configured-id",
			ClientSecret: "configured-secret",
			BearerToken:  "configured-token",
			TokenURL:     "https://tenant.example.com/OAuth/Token",
			Scope:        "read",
		},
	}
}

func authorization(value string) http.Header {
	headers := http.Header{}
	headers.Set("Authorization", value)

	return headers
}

func basic(raw string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
}

func TestResolveTakesClientCredentialsFromBasic(t *testing.T) {
	creds, err := defaultResolver().Resolve(authorization(basic("caller-id:caller-secret")))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	want := Credentials{
		ServiceURL:   "https://tenant.example.com/odata/service.svc/",
		ClientID:     "caller-id",
		ClientSecret: "caller-secret",
		TokenURL:     "https://tenant.example.com/OAuth/Token",
		Scope:        "read",
	}
	if creds != want {
		t.Errorf("Resolve() = %+v, want %+v", creds.Redacted(), want.Redacted())
	}
}

func TestResolveKeepsAColonInTheSecret(t *testing.T) {
	creds, err := defaultResolver().Resolve(authorization(basic("caller-id:sec:ret")))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if creds.ClientID != "caller-id" || creds.ClientSecret != "sec:ret" {
		t.Errorf("Resolve() = %+v, want the id to end at the first colon", creds.Redacted())
	}
}

func TestResolveTreatsABearerAsTheWholeCredential(t *testing.T) {
	creds, err := defaultResolver().Resolve(authorization("Bearer caller-token"))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if creds.BearerToken != "caller-token" {
		t.Errorf("BearerToken = %q, want the header token", creds.BearerToken)
	}
	if creds.ClientID != "" || creds.ClientSecret != "" {
		t.Errorf("Resolve() = %+v, want no client credentials alongside a bearer", creds.Redacted())
	}
}

func TestResolveMatchesTheSchemeCaseInsensitively(t *testing.T) {
	for _, value := range []string{"bearer t", "BEARER t", "basic " + basic("id:secret")[len("Basic "):]} {
		t.Run(value, func(t *testing.T) {
			if _, err := defaultResolver().Resolve(authorization(value)); err != nil {
				t.Errorf("Resolve(%q) error = %v", value, err)
			}
		})
	}
}

func TestResolveNeverLendsTheConfiguredCredential(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-OData-Client-Id", "caller-id")
	headers.Set("X-OData-Client-Secret", "caller-secret")

	for name, h := range map[string]http.Header{
		"no header":              {},
		"the retired X- headers": headers,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := defaultResolver().Resolve(h); err == nil {
				t.Error("a request with no Authorization credential was let through")
			}
		})
	}
}

func TestResolveRefusesAMalformedCredential(t *testing.T) {
	tests := []string{
		"Basic not-base64!",
		basic("no-colon"),
		basic(":secret-without-id"),
		basic("id-without-secret:"),
		"Basic",
		"Bearer",
		"Bearer ",
		"Digest abc",
		"caller-token",
	}

	for _, value := range tests {
		t.Run(value, func(t *testing.T) {
			if _, err := defaultResolver().Resolve(authorization(value)); err == nil {
				t.Errorf("Resolve(%q) accepted a malformed credential", value)
			}
		})
	}
}

func TestResolveRefusesClientCredentialsWithoutATokenURL(t *testing.T) {
	resolver := defaultResolver()
	resolver.Service.TokenURL = ""

	if _, err := resolver.Resolve(authorization(basic("id:secret"))); err == nil {
		t.Error("Resolve() accepted client credentials with nowhere to exchange them")
	}
}
