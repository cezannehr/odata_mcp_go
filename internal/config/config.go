// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package config

import (
	"strings"
	"unicode"
)

// Config holds all configuration options for the OData MCP bridge
type Config struct {
	// Service configuration
	ServiceURL string `mapstructure:"service_url"`

	// Authentication
	Username     string            `mapstructure:"username"`
	Password     string            `mapstructure:"password"`
	CookieFile   string            `mapstructure:"cookie_file"`
	CookieString string            `mapstructure:"cookie_string"`
	Cookies      map[string]string // Parsed cookies

	// OAuth 2.0 client credentials
	OAuthClientID     string `mapstructure:"oauth_client_id"`
	OAuthClientSecret string `mapstructure:"oauth_client_secret"`
	OAuthTokenURL     string `mapstructure:"oauth_token_url"`
	OAuthScope        string `mapstructure:"oauth_scope"`
	OAuthClientAuth   string `mapstructure:"oauth_client_auth"`

	// Bearer token presented as-is, for a token an administrator minted
	BearerToken string `mapstructure:"bearer_token"`

	// Multi-tenant serving
	MultiTenant        bool   `mapstructure:"multi_tenant"`
	AllowedServiceURLs string `mapstructure:"allowed_service_urls"`

	// Tool naming options
	ToolPrefix  string `mapstructure:"tool_prefix"`
	ToolPostfix string `mapstructure:"tool_postfix"`
	NoPostfix   bool   `mapstructure:"no_postfix"`
	ToolShrink  bool   `mapstructure:"tool_shrink"`

	// Entity and function filtering
	Entities         string   `mapstructure:"entities"`
	Functions        string   `mapstructure:"functions"`
	AllowedEntities  []string // Parsed from Entities
	AllowedFunctions []string // Parsed from Functions

	// Output and debugging
	LogFormat string `mapstructure:"log_format"` // "text" or "json"; empty picks by mode
	Verbose   bool   `mapstructure:"verbose"`
	Debug     bool   `mapstructure:"debug"`
	SortTools bool   `mapstructure:"sort_tools"`
	Trace     bool   `mapstructure:"trace"`

	// Response enhancement options
	PaginationHints  bool `mapstructure:"pagination_hints"`  // Add pagination support with hints
	LegacyDates      bool `mapstructure:"legacy_dates"`      // Support epoch timestamp format
	LegacyDatesSet   bool `mapstructure:"-"`                 // True when --legacy-dates was given explicitly
	NoLegacyDates    bool `mapstructure:"no_legacy_dates"`   // Disable legacy date format
	VerboseErrors    bool `mapstructure:"verbose_errors"`    // Detailed error context
	ResponseMetadata bool `mapstructure:"response_metadata"` // Include __metadata in responses

	// Response size limits
	MaxResponseSize int `mapstructure:"max_response_size"` // Maximum response size in bytes
	MaxItems        int `mapstructure:"max_items"`         // Maximum number of items in response

	// Read-only mode flags
	ReadOnly             bool `mapstructure:"read_only"`               // Read-only mode: hide all modifying operations
	ReadOnlyButFunctions bool `mapstructure:"read_only_but_functions"` // Read-only mode but allow function imports

	// Hint configuration
	HintsFile string `mapstructure:"hints_file"` // Path to hints JSON file
	Hint      string `mapstructure:"hint"`       // Direct hint JSON from CLI

	// Operation type filtering
	EnableOps  string `mapstructure:"enable_ops"`  // Operation types to enable (e.g., "csfg")
	DisableOps string `mapstructure:"disable_ops"` // Operation types to disable (e.g., "cud")

	// Claude Code compatibility
	ClaudeCodeFriendly bool `mapstructure:"claude_code_friendly"` // Remove $ prefix from OData parameters

	// Protocol version override (for AI Foundry compatibility)
	ProtocolVersion string `mapstructure:"protocol_version"` // Override MCP protocol version (default: 2024-11-05)

	// Header forwarding (HTTP transport only)
	ForwardMCPHeaders bool `mapstructure:"forward_mcp_headers"` // Forward HTTP headers from MCP connection to OData service

	// Universal tool mode (single tool instead of N tools per entity)
	UniversalTool bool `mapstructure:"universal"` // Use single universal OData tool instead of per-entity tools
}

// HasBasicAuth returns true if username and password are configured
func (c *Config) HasBasicAuth() bool {
	return c.Username != "" && c.Password != ""
}

// HasCookieAuth returns true if cookies are configured
func (c *Config) HasCookieAuth() bool {
	return len(c.Cookies) > 0
}

// HasOAuthAuth returns true if OAuth client credentials are configured
func (c *Config) HasOAuthAuth() bool {
	return c.OAuthClientID != "" && c.OAuthClientSecret != "" && c.OAuthTokenURL != ""
}

// HasBearerAuth returns true if a bearer token is configured
func (c *Config) HasBearerAuth() bool {
	return c.BearerToken != ""
}

// HasAnyOAuthSetting returns true if any OAuth field is set, complete or not
func (c *Config) HasAnyOAuthSetting() bool {
	return c.OAuthClientID != "" || c.OAuthClientSecret != "" ||
		c.OAuthTokenURL != "" || c.OAuthScope != "" || c.OAuthClientAuth != ""
}

// UsePostfix returns true if tool postfix should be used instead of prefix
func (c *Config) UsePostfix() bool {
	return !c.NoPostfix
}

// IsReadOnly returns true if read-only mode is enabled
func (c *Config) IsReadOnly() bool {
	return c.ReadOnly || c.ReadOnlyButFunctions
}

// AllowModifyingFunctions reports whether actions and other modifying function
// imports may run. Both read-only modes refuse them; --read-only-but-functions
// only keeps functions the metadata declares as GET.
func (c *Config) AllowModifyingFunctions() bool {
	return !c.IsReadOnly()
}

// IsOperationEnabled checks if a specific operation type is enabled based on --enable/--disable flags
func (c *Config) IsOperationEnabled(opType rune) bool {
	// Normalize operation type to uppercase
	opType = unicode.ToUpper(opType)

	// Expand 'R' to 'SFG'
	if opType == 'R' {
		// Check if any of S, F, or G are enabled
		return c.IsOperationEnabled('S') || c.IsOperationEnabled('F') || c.IsOperationEnabled('G')
	}

	// If --enable is specified, only those operations are allowed
	if c.EnableOps != "" {
		enableOps := strings.ToUpper(c.EnableOps)
		// Check if 'R' is in enable list and we're checking S, F, or G
		if strings.ContainsRune(enableOps, 'R') && (opType == 'S' || opType == 'F' || opType == 'G') {
			return true
		}
		return strings.ContainsRune(enableOps, opType)
	}

	// If --disable is specified, those operations are not allowed
	if c.DisableOps != "" {
		disableOps := strings.ToUpper(c.DisableOps)
		// Check if 'R' is in disable list and we're checking S, F, or G
		if strings.ContainsRune(disableOps, 'R') && (opType == 'S' || opType == 'F' || opType == 'G') {
			return false
		}
		return !strings.ContainsRune(disableOps, opType)
	}

	// If neither flag is specified, all operations are enabled by default
	return true
}
