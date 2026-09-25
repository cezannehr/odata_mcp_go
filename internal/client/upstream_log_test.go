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

// The rule has to hold when the service is down, which is when the line is
// read: http.Client wraps the failure in a url.Error whose text carries the
// full URL, query included.
func TestAFailedODataCallStillLogsWithoutItsQuery(t *testing.T) {
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	// Port 1 is never listening.
	c := NewODataClient("http://127.0.0.1:1/odata/", false)

	_, err := c.GetEntitySet(context.Background(), "People", map[string]string{"$filter": "Email eq 'someone@example.com'"})
	if err == nil {
		t.Fatal("GetEntitySet() error = nil, want a connection failure")
	}

	if strings.Contains(out.String(), "someone@example.com") || strings.Contains(out.String(), "filter") {
		t.Fatalf("the $filter leaked through the failure: %s", out.String())
	}

	var line map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &line); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, out.String())
	}
	if line["level"] != "ERROR" || line["path"] != "/odata/People" || !strings.Contains(line["error"].(string), "connection refused") {
		t.Errorf("line = %v, want ERROR on /odata/People with the bare cause", line)
	}
}

func TestAKeyedODataCallLogsTheSetNotTheRow(t *testing.T) {
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"d":{"PersonCode":"CEZ59"}}`))
	}))
	defer srv.Close()

	c := NewODataClient(srv.URL+"/odata/", false)
	if _, err := c.GetEntity(context.Background(), "People", map[string]interface{}{"PersonCode": "CEZ59"}, nil); err != nil {
		t.Fatalf("GetEntity() error = %v", err)
	}

	if strings.Contains(out.String(), "CEZ59") {
		t.Errorf("the row key leaked: %s", out.String())
	}
}
