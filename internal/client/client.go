// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/zmcp/odata-mcp/internal/constants"
)

// Context key for HTTP headers passed from MCP server
type contextKey string

const HTTPHeadersContextKey contextKey = "mcp-http-headers"

// TokenSource supplies an Authorization header value, refreshing the
// underlying credential as needed.
type TokenSource interface {
	AuthorizationHeader(ctx context.Context) (string, error)
}

// ODataClient handles HTTP communication with OData services
type ODataClient struct {
	baseURL        string
	httpClient     *http.Client
	cookies        map[string]string
	username       string
	password       string
	tokenSource    TokenSource
	csrfToken      string
	verbose        bool
	sessionCookies []*http.Cookie // Track session cookies from server
	isV4           bool           // Whether the service is OData v4
}

// NewODataClient creates a new OData client
func NewODataClient(baseURL string, verbose bool) *ODataClient {
	// Ensure base URL ends with /
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}

	return &ODataClient{
		baseURL:    baseURL,
		httpClient: NewHTTPClient(),
		verbose:    verbose,
		isV4:       false, // Will be determined when fetching metadata
	}
}

// SetBasicAuth configures basic authentication
func (c *ODataClient) SetBasicAuth(username, password string) {
	c.username = username
	c.password = password
}

// SetCookies configures cookie authentication
func (c *ODataClient) SetCookies(cookies map[string]string) {
	c.cookies = cookies
}

// SetTokenSource configures bearer token authentication
func (c *ODataClient) SetTokenSource(source TokenSource) {
	c.tokenSource = source
}

// Helper function for min
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// shouldForwardHeader determines if a header should be forwarded from MCP to OData service
func shouldForwardHeader(headerName string) bool {
	// Normalize to lowercase for comparison
	lower := strings.ToLower(headerName)

	// Allow authentication headers
	if lower == "authorization" || lower == "cookie" {
		return true
	}

	// Allow custom headers (X- prefix)
	if strings.HasPrefix(lower, "x-") {
		return true
	}

	// Block hop-by-hop headers and other problematic headers
	blockedHeaders := []string{
		"host",
		"connection",
		"keep-alive",
		"transfer-encoding",
		"upgrade",
		"proxy-authenticate",
		"proxy-authorization",
		"te",
		"trailer",
		"content-length", // Will be set by http.Client
		"content-type",   // Set by specific methods
		"accept",         // Set by buildRequest
		"user-agent",     // Set by buildRequest
	}

	for _, blocked := range blockedHeaders {
		if lower == blocked {
			return false
		}
	}

	// Allow other headers by default
	return true
}

// RedactHeaders copies h with every credential value masked, for logging.
func RedactHeaders(h http.Header) http.Header {
	safe := h.Clone()

	for name := range safe {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "cookie" || lower == "set-cookie" {
			safe.Set(name, "[redacted]")
		}
	}

	return safe
}

// NewHTTPClient returns the client every outbound call should use: bounded in
// time and unable to follow a redirect off the host it was pointed at.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout:       time.Duration(constants.DefaultTimeout) * time.Second,
		CheckRedirect: checkRedirect,
	}
}

// checkRedirect refuses a redirect that leaves the host the caller asked for.
// An allow list checked before the request says nothing about where that host
// then points, so following freely would make this a fetch-anything proxy.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}

	if len(via) >= 10 {
		return fmt.Errorf("stopped after %d redirects", len(via))
	}

	origin := via[0].URL

	if !strings.EqualFold(req.URL.Host, origin.Host) {
		return fmt.Errorf("refusing redirect from %s to a different host %s", origin.Host, req.URL.Host)
	}

	// An http to https upgrade is fine; a downgrade is not.
	if !strings.EqualFold(req.URL.Scheme, origin.Scheme) && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect from %s to %s on %s", origin.Scheme, req.URL.Scheme, req.URL.Host)
	}

	return nil
}
