// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

// Package auth provides authentication token sources for OData services.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zmcp/odata-mcp/internal/obs"
)

// Client authentication methods for the token endpoint.
const (
	// ClientAuthBasic uses an HTTP Basic header, which RFC 6749 section 2.3.1
	// requires every authorization server to support.
	ClientAuthBasic = "basic"

	// ClientAuthBody uses form fields, which that section makes optional.
	// Some servers accept only this.
	ClientAuthBody = "body"
)

const (
	grantClientCredentials = "client_credentials"
	bearerTokenType        = "Bearer"

	// defaultExpiresIn applies when the response omits expires_in, which
	// RFC 6749 section 5.1 permits.
	defaultExpiresIn = 3600 * time.Second

	minRefreshMargin = 5 * time.Second
	maxRefreshMargin = 60 * time.Second

	// errorBodyLimit caps how much of a failed token response is quoted back
	// in the error message. It does not cap what is read: tokens can be long.
	errorBodyLimit = 512

	// responseBodyLimit bounds the read so a hostile endpoint cannot stream
	// unbounded data into memory.
	responseBodyLimit = 1 << 20
)

// OAuthConfig describes an OAuth 2.0 client credentials configuration.
type OAuthConfig struct {
	ClientID     string
	ClientSecret string
	TokenURL     string
	Scope        string

	// ClientAuth is ClientAuthBasic or ClientAuthBody. Empty means basic.
	ClientAuth string

	Verbose bool

	// HTTPClient is optional; the default carries a timeout so a hung token
	// endpoint cannot wedge every OData request behind it.
	HTTPClient *http.Client
}

// Validate reports whether the configuration can be used to fetch a token.
func (c *OAuthConfig) Validate() error {
	var missing []string
	if c.ClientID == "" {
		missing = append(missing, "client ID")
	}
	if c.ClientSecret == "" {
		missing = append(missing, "client secret")
	}
	if c.TokenURL == "" {
		missing = append(missing, "token URL")
	}
	if len(missing) > 0 {
		return fmt.Errorf("OAuth requires %s", strings.Join(missing, ", "))
	}

	parsed, err := url.Parse(c.TokenURL)
	if err != nil {
		return fmt.Errorf("OAuth token URL is not a valid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("OAuth token URL must be http or https, got %q", c.TokenURL)
	}
	if parsed.Host == "" {
		return fmt.Errorf("OAuth token URL has no host: %q", c.TokenURL)
	}

	switch c.ClientAuth {
	case "", ClientAuthBasic, ClientAuthBody:
	default:
		return fmt.Errorf("OAuth client auth must be %q or %q, got %q",
			ClientAuthBasic, ClientAuthBody, c.ClientAuth)
	}

	return nil
}

// OAuthTokenSource fetches and caches client credentials access tokens.
type OAuthTokenSource struct {
	config     OAuthConfig
	httpClient *http.Client

	// mu is held across the token request, so a burst of OData calls on an
	// expired token makes one request to the authorization server, not one each.
	mu        sync.Mutex
	token     string
	tokenType string
	expiresAt time.Time

	// now is overridable so expiry can be tested without sleeping.
	now func() time.Time
}

// NewOAuthTokenSource validates the configuration and returns a token source.
func NewOAuthTokenSource(config OAuthConfig) (*OAuthTokenSource, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	return &OAuthTokenSource{
		config:     config,
		httpClient: httpClient,
		now:        time.Now,
	}, nil
}

// AuthorizationHeader returns the Authorization header value for the current
// token, fetching or refreshing it when necessary.
func (s *OAuthTokenSource) AuthorizationHeader(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.token != "" && s.now().Before(s.expiresAt) {
		return s.tokenType + " " + s.token, nil
	}

	if err := s.fetch(ctx); err != nil {
		return "", err
	}

	return s.tokenType + " " + s.token, nil
}

// tokenResponse is RFC 6749 section 5.1. ExpiresIn stays raw because servers
// disagree on whether it is a JSON number or a string.
type tokenResponse struct {
	AccessToken string          `json:"access_token"`
	TokenType   string          `json:"token_type"`
	ExpiresIn   json.RawMessage `json:"expires_in"`
}

// fetch requests a new token. The caller must hold s.mu.
func (s *OAuthTokenSource) fetch(ctx context.Context) error {
	form := url.Values{"grant_type": {grantClientCredentials}}
	if s.config.Scope != "" {
		form.Set("scope", s.config.Scope)
	}
	if s.config.ClientAuth == ClientAuthBody {
		form.Set("client_id", s.config.ClientID)
		form.Set("client_secret", s.config.ClientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.TokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("failed to build OAuth token request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if s.config.ClientAuth != ClientAuthBody {
		req.SetBasicAuth(s.config.ClientID, s.config.ClientSecret)
	}

	s.logVerbose("Fetching OAuth token from %s", s.config.TokenURL)

	start := time.Now()
	resp, err := s.httpClient.Do(req)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	obs.LogUpstream(ctx, "upstream.token", req.Method, req.URL, status, err, time.Since(start))
	if err != nil {
		return fmt.Errorf("OAuth token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, responseBodyLimit))
	if err != nil {
		return fmt.Errorf("failed to read OAuth token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("OAuth token endpoint returned %d: %s",
			resp.StatusCode, truncate(string(body), errorBodyLimit))
	}

	var parsed tokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("OAuth token response is not valid JSON: %w", err)
	}
	if parsed.AccessToken == "" {
		return fmt.Errorf("OAuth token response contains no access_token")
	}

	lifetime, err := tokenLifetime(parsed.ExpiresIn)
	if err != nil {
		return err
	}

	s.token = parsed.AccessToken
	s.tokenType = normalizeTokenType(parsed.TokenType)
	s.expiresAt = s.now().Add(lifetime - refreshMargin(lifetime))

	s.logVerbose("OAuth token obtained, expires in %d seconds", int(lifetime.Seconds()))

	return nil
}

func (s *OAuthTokenSource) logVerbose(format string, args ...interface{}) {
	if s.config.Verbose {
		fmt.Fprintf(os.Stderr, "[VERBOSE] "+format+"\n", args...)
	}
}

func tokenLifetime(expiresIn json.RawMessage) (time.Duration, error) {
	raw := strings.TrimSpace(string(expiresIn))
	if raw == "" || raw == "null" {
		return defaultExpiresIn, nil
	}

	if unquoted, err := strconv.Unquote(raw); err == nil {
		raw = unquoted
	}

	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("OAuth token response has a non-numeric expires_in %q", raw)
	}
	if seconds <= 0 {
		return defaultExpiresIn, nil
	}

	return time.Duration(seconds) * time.Second, nil
}

// refreshMargin is how early a token counts as expired, so a slow request
// cannot start on one that dies mid-flight. It scales with the lifetime.
func refreshMargin(lifetime time.Duration) time.Duration {
	margin := lifetime / 5
	if margin < minRefreshMargin {
		margin = minRefreshMargin
	}
	if margin > maxRefreshMargin {
		margin = maxRefreshMargin
	}
	if margin > lifetime/2 {
		margin = lifetime / 2
	}
	return margin
}

// normalizeTokenType title-cases the bearer type, which RFC 6749 section 7.1
// defines case-insensitively but many servers send lowercase.
func normalizeTokenType(tokenType string) string {
	if tokenType == "" || strings.EqualFold(tokenType, bearerTokenType) {
		return bearerTokenType
	}
	return tokenType
}

func truncate(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
