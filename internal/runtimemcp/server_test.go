package runtimemcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestCatalogPreservesTextInputAndRejectsOtherShapes(t *testing.T) {
	var admitted map[string]any
	handler := NewHandler(func(_ context.Context, _ string, body json.RawMessage) (json.RawMessage, error) {
		if err := json.Unmarshal(body, &admitted); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"sequence":1}`), nil
	})
	post := func(body string) map[string]any {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("MCP-Protocol-Version", "2025-11-25")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result["error"] != nil {
			return map[string]any{"isError": true, "error": result["error"]}
		}
		return result["result"].(map[string]any)
	}
	catalog := post(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	count := 0
	for _, entry := range catalog["tools"].([]any) {
		tool := entry.(map[string]any)
		if name := tool["name"]; name != "enqueue" && name != "spawn" && name != "start" {
			continue
		}
		schema := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)["input"]
		if fields, ok := schema.(map[string]any); !ok || fields["type"] != "array" {
			t.Fatalf("%s input schema is not an array: %v", tool["name"], schema)
		}
		count++
	}
	if count != 3 {
		t.Fatalf("mutation catalog count=%d", count)
	}
	for _, input := range []string{`[]`, `[{"type":"text","text":"hello"}]`, `[{"type":"text","text":"Hello"},{"type":"text","text":" world\n"}]`} {
		admitted = nil
		result := post(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"enqueue","arguments":{"sessionId":"peer","idempotencyKey":"stable","input":` + input + `}}}`)
		if result["isError"] == true {
			t.Fatalf("tool rejected %s: %v", input, result)
		}
		var expected any
		if err := json.Unmarshal([]byte(input), &expected); err != nil {
			t.Fatal(err)
		}
		if admitted == nil || !reflect.DeepEqual(admitted["input"], expected) {
			t.Fatalf("input changed: %v want %v", admitted, expected)
		}
	}
	for _, input := range []string{`null`, `true`, `7.5`, `"text"`, `{"task":"work"}`, `{"type":"message","content":[]}`, `[{"type":"json","value":null}]`, `[{"type":"text","text":"hello","actor":"someone"}]`} {
		admitted = nil
		result := post(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"enqueue","arguments":{"sessionId":"peer","idempotencyKey":"stable","input":` + input + `}}}`)
		if result["isError"] != true || admitted != nil {
			t.Fatalf("invalid input admitted %s: %v", input, result)
		}
	}
	names := map[string]bool{}
	for _, entry := range catalog["tools"].([]any) {
		names[entry.(map[string]any)["name"].(string)] = true
	}
	for _, name := range []string{"spawn", "start", "send_turn", "interrupt_session", "resume_session", "close_session", "cancel_session"} {
		if !names[name] {
			t.Fatalf("missing managed operation %s", name)
		}
	}
	if names["handoff"] {
		t.Fatal("unexpected third creation operation")
	}

}
