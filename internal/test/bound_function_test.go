// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zmcp/odata-mcp/internal/bridge"
)

const boundFunctionMetadataXML = `<?xml version="1.0" encoding="utf-8"?>
<edmx:Edmx Version="1.0" xmlns:edmx="http://schemas.microsoft.com/ado/2007/06/edmx">
  <edmx:DataServices m:DataServiceVersion="3.0" xmlns:m="http://schemas.microsoft.com/ado/2007/08/dataservices/metadata">
    <Schema Namespace="Test" xmlns="http://schemas.microsoft.com/ado/2009/11/edm">
      <EntityType Name="Order">
        <Key><PropertyRef Name="OrderGUID"/></Key>
        <Property Name="OrderGUID" Type="Edm.Guid" Nullable="false"/>
      </EntityType>
      <EntityContainer Name="TestContainer" m:IsDefaultEntityContainer="true">
        <EntitySet Name="Orders" EntityType="Test.Order"/>
        <FunctionImport Name="Orders_Cancel" ReturnType="Edm.Boolean" IsBindable="true" m:IsAlwaysBindable="true">
          <Parameter Name="targetEntity" Type="Test.Order"/>
          <Parameter Name="reason" Type="Edm.String"/>
        </FunctionImport>
      </EntityContainer>
    </Schema>
  </edmx:DataServices>
</edmx:Edmx>`

type recordedPost struct {
	path string
	body map[string]any
}

func newBoundFunctionService(t *testing.T) (*httptest.Server, func() []recordedPost) {
	t.Helper()
	return newBoundFunctionServiceWith(t, boundFunctionMetadataXML)
}

func newBoundFunctionServiceWith(t *testing.T, metadataXML string) (*httptest.Server, func() []recordedPost) {
	t.Helper()

	var mu sync.Mutex
	var posts []recordedPost

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/$metadata") {
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, metadataXML)
			return
		}

		if r.Method == http.MethodPost {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			mu.Lock()
			posts = append(posts, recordedPost{path: r.URL.EscapedPath(), body: body})
			mu.Unlock()
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"value":true}`)
	}))
	t.Cleanup(server.Close)

	return server, func() []recordedPost {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedPost(nil), posts...)
	}
}

func TestBoundFunctionIsCalledThroughItsEntity(t *testing.T) {
	service, recorded := newBoundFunctionService(t)
	odataBridge, _ := universalTool(t, universalConfig(service.URL+"/odata/"))

	_, rpcErr := callUniversalTool(t, odataBridge, map[string]any{
		"action": "call",
		"target": "Orders_Cancel",
		"params": map[string]any{
			"key":    map[string]any{"OrderGUID": "7d603233-be31-4c4f-90e7-26d7618b6b9e"},
			"reason": "duplicate",
		},
	})
	require.Nil(t, rpcErr)

	posts := recorded()
	require.Len(t, posts, 1)
	assert.Equal(t, "/odata/Orders(guid'7d603233-be31-4c4f-90e7-26d7618b6b9e')/Orders_Cancel", posts[0].path)
	assert.Equal(t, map[string]any{"reason": "duplicate"}, posts[0].body,
		"the binding parameter is addressed in the URL, so it must not be sent in the body")
}

func TestBoundFunctionWithoutAKeyIsRefusedBeforeAnyRequest(t *testing.T) {
	service, recorded := newBoundFunctionService(t)
	odataBridge, _ := universalTool(t, universalConfig(service.URL+"/odata/"))

	_, rpcErr := callUniversalTool(t, odataBridge, map[string]any{
		"action": "call",
		"target": "Orders_Cancel",
		"params": map[string]any{"reason": "duplicate"},
	})
	require.NotNil(t, rpcErr)
	assert.Contains(t, string(rpcErr.Data), "missing required parameter: key")
	assert.Empty(t, recorded(), "the service rejects a bound call made at the service root, so none should be sent")
}

func TestBoundFunctionToolAsksForTheEntityKey(t *testing.T) {
	service, _ := newBoundFunctionService(t)

	cfg := universalConfig(service.URL + "/odata/")
	cfg.UniversalTool = false
	odataBridge, err := bridge.NewODataMCPBridge(cfg)
	require.NoError(t, err)

	var schema map[string]any
	for _, tool := range odataBridge.GetServer().GetTools() {
		if strings.Contains(tool.Name, "Orders_Cancel") {
			schema = tool.InputSchema
		}
	}
	require.NotNil(t, schema, "Orders_Cancel should be registered as a tool")

	properties := schema["properties"].(map[string]any)
	assert.Contains(t, properties, "key")
	assert.Contains(t, properties, "reason")
	assert.NotContains(t, properties, "targetEntity")
	assert.Contains(t, schema["required"], "key")
}

func TestBoundFunctionOnAnAmbiguousTypeFailsBeforeAnyRequest(t *testing.T) {
	twoSets := strings.Replace(boundFunctionMetadataXML,
		`<EntitySet Name="Orders" EntityType="Test.Order"/>`,
		`<EntitySet Name="Orders" EntityType="Test.Order"/><EntitySet Name="ArchivedOrders" EntityType="Test.Order"/>`, 1)
	service, recorded := newBoundFunctionServiceWith(t, twoSets)
	odataBridge, _ := universalTool(t, universalConfig(service.URL+"/odata/"))

	call := map[string]any{
		"action": "call",
		"target": "Orders_Cancel",
		"params": map[string]any{"key": map[string]any{"OrderGUID": "7d603233-be31-4c4f-90e7-26d7618b6b9e"}, "reason": "duplicate"},
	}
	_, rpcErr := callUniversalTool(t, odataBridge, call)
	require.NotNil(t, rpcErr)
	assert.Contains(t, string(rpcErr.Data), "must belong to exactly one exposed entity set")
	assert.Empty(t, recorded())

	narrowed := universalConfig(service.URL + "/odata/")
	narrowed.AllowedEntities = []string{"ArchivedOrders"}
	odataBridge, _ = universalTool(t, narrowed)

	_, rpcErr = callUniversalTool(t, odataBridge, call)
	require.Nil(t, rpcErr, "--entities leaves one set, so the binding resolves")
	posts := recorded()
	require.Len(t, posts, 1)
	assert.Equal(t, "/odata/ArchivedOrders(guid'7d603233-be31-4c4f-90e7-26d7618b6b9e')/Orders_Cancel", posts[0].path)
}
