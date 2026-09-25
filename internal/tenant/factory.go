// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

// Package tenant builds one bridge per caller credential, joining the registry
// to the bridge without either having to know about the other.
package tenant

import (
	"context"
	"net/http"

	"github.com/zmcp/odata-mcp/internal/bridge"
	"github.com/zmcp/odata-mcp/internal/client"
	"github.com/zmcp/odata-mcp/internal/config"
	"github.com/zmcp/odata-mcp/internal/registry"
	"github.com/zmcp/odata-mcp/internal/transport"
)

// Bridge adapts a bridge to the message handler the registry hands back.
type Bridge struct {
	*bridge.ODataMCPBridge
}

// HandleMessage dispatches to the bridge's own MCP server. The caller's HTTP
// headers have done their job by now, selecting the credential, so none of
// them are forwarded to the OData service.
func (b Bridge) HandleMessage(ctx context.Context, msg *transport.Message) (*transport.Message, error) {
	ctx = context.WithValue(ctx, client.HTTPHeadersContextKey, http.Header{})

	return b.GetServer().HandleMessage(ctx, msg)
}

// Config copies base and applies creds, so shared options survive while every
// credential from the previous tenant is replaced or cleared.
func Config(base *config.Config, creds registry.Credentials) *config.Config {
	tenantCfg := *base

	tenantCfg.ServiceURL = creds.ServiceURL
	tenantCfg.BearerToken = creds.BearerToken
	tenantCfg.OAuthClientID = creds.ClientID
	tenantCfg.OAuthClientSecret = creds.ClientSecret
	tenantCfg.OAuthTokenURL = creds.TokenURL
	tenantCfg.OAuthScope = creds.Scope

	tenantCfg.Username = ""
	tenantCfg.Password = ""
	tenantCfg.CookieFile = ""
	tenantCfg.CookieString = ""
	tenantCfg.Cookies = nil

	return &tenantCfg
}

// Factory returns a registry.Factory that builds bridges from base.
//
// The build gets the first caller's context for its values only. Every
// caller waiting on the same build shares its result, so the build must not
// die with whichever request happened to start it; but the log lines the
// build writes should still name that request.
func Factory(base *config.Config) registry.Factory {
	return func(ctx context.Context, creds registry.Credentials) (registry.Bridge, error) {
		built, err := bridge.NewODataMCPBridgeContext(context.WithoutCancel(ctx), Config(base, creds))
		if err != nil {
			return nil, err
		}

		return Bridge{built}, nil
	}
}
