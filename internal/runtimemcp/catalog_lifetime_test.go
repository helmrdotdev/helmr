package runtimemcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStaticCatalogDoesNotRetainNotificationRequests(t *testing.T) {
	server := httptest.NewServer(NewHandler(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		t.Error("catalog subscription invoked a Session operation")
		return nil, nil
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{"_meta":{"io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"catalog-test","version":"1"},"io.modelcontextprotocol/protocolVersion":"2026-07-28"},"notifications":{"toolsListChanged":true,"promptsListChanged":true,"resourcesListChanged":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Mcp-Method", "subscriptions/listen")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", "2026-07-28")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("static catalog retained the notification stream: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d: %s", response.StatusCode, body)
	}
	acknowledged, completed := false, false
	for line := range strings.SplitSeq(string(body), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var event struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Notifications map[string]any `json:"notifications"`
			} `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatal(err)
		}
		if event.Method == "notifications/subscriptions/acknowledged" {
			acknowledged = true
			if len(event.Params.Notifications) != 0 {
				t.Fatalf("static catalog advertised notifications: %s", data)
			}
		}
		completed = completed || event.ID == 1 && len(event.Result) != 0
	}
	if !acknowledged || !completed {
		t.Fatalf("subscription did not acknowledge and finish: %s", body)
	}
}
