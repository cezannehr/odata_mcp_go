// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zmcp/odata-mcp/internal/obs"
)

func TestEveryODataCallIsLoggedWithoutItsQuery(t *testing.T) {
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"d":{"results":[]}}`))
	}))
	defer srv.Close()

	c := NewODataClient(srv.URL+"/odata/", false)
	record := obs.NewRequest("req-77")
	record.SetTenant("t-1")
	ctx := obs.WithRequest(context.Background(), record)

	if _, err := c.GetEntitySet(ctx, "People", map[string]string{"$filter": "Email eq 'someone@example.com'"}); err != nil {
		t.Fatalf("GetEntitySet() error = %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want one upstream line, got %d:\n%s", len(lines), out.String())
	}

	var line map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}

	want := map[string]any{
		"event":      "upstream.request",
		"method":     "GET",
		"path":       "/odata/People",
		"status":     float64(200),
		"request_id": "req-77",
		"tenant":     "t-1",
	}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("%s = %v, want %v", k, line[k], v)
		}
	}
	if strings.Contains(out.String(), "someone@example.com") {
		t.Errorf("the $filter leaked into the log: %s", out.String())
	}
}
