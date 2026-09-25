// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package tenant

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zmcp/odata-mcp/internal/config"
	"github.com/zmcp/odata-mcp/internal/obs"
	"github.com/zmcp/odata-mcp/internal/registry"
)

// The build's metadata fetch must be findable from the request that caused
// it, and must not die with that request: every caller waiting on the same
// build shares its result.
func TestBuildNamesItsRequestAndOutlivesIt(t *testing.T) {
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	reached := false
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer svc.Close()

	record := obs.NewRequest("req-build")
	record.SetTenant("t-build")
	ctx, cancel := context.WithCancel(obs.WithRequest(context.Background(), record))
	cancel() // already gone by the time the build runs

	creds := registry.Credentials{ServiceURL: svc.URL + "/odata/", BearerToken: "token"}
	if _, err := Factory(&config.Config{})(ctx, creds); err == nil {
		t.Fatal("Factory() error = nil, want the 401 to fail the build")
	}

	if !reached {
		t.Fatal("the metadata fetch never reached the service: the caller's cancellation killed the build")
	}

	var upstream map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, raw)
		}
		if line["event"] == "upstream.request" {
			upstream = line
		}
	}
	if upstream == nil {
		t.Fatalf("no upstream.request line in:\n%s", out.String())
	}
	if upstream["request_id"] != "req-build" || upstream["tenant"] != "t-build" || upstream["path"] != "/odata/$metadata" {
		t.Errorf("upstream line = %v, want request_id=req-build tenant=t-build path=/odata/$metadata", upstream)
	}
}
