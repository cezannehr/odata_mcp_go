// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package registry

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
)

type Resolver struct {
	// Credential fields here are ignored, so a caller can never borrow the operator's.
	Service Credentials
}

func (rs Resolver) Resolve(headers http.Header) (Credentials, error) {
	creds := Credentials{
		ServiceURL: rs.Service.ServiceURL,
		TokenURL:   rs.Service.TokenURL,
		Scope:      rs.Service.Scope,
	}

	scheme, value, _ := strings.Cut(strings.TrimSpace(headers.Get("Authorization")), " ")
	value = strings.TrimSpace(value)

	switch {
	case strings.EqualFold(scheme, "bearer"):
		creds.BearerToken = value
	case strings.EqualFold(scheme, "basic"):
		clientID, clientSecret, err := parseBasic(value)
		if err != nil {
			return Credentials{}, err
		}
		creds.ClientID = clientID
		creds.ClientSecret = clientSecret
	}

	return creds, creds.Validate()
}

// parseBasic decodes an RFC 7617 credential. The client id ends at the first
// colon, so a secret may contain one.
func parseBasic(value string) (string, string, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", "", fmt.Errorf("registry: Basic credential is not valid base64")
	}

	clientID, clientSecret, found := strings.Cut(string(decoded), ":")
	if !found || clientID == "" || clientSecret == "" {
		return "", "", fmt.Errorf("registry: Basic credential must be base64 of client_id:client_secret")
	}

	return clientID, clientSecret, nil
}
