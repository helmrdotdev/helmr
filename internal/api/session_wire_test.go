package api

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestSessionWireFixturesPreserveAPIEncoding(t *testing.T) {
	raw, err := os.ReadFile("../../tests/fixtures/session-wire.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	for name, raw := range fixtures {
		t.Run(name, func(t *testing.T) {
			var value any = &AgentTurn{}
			if name == "session" {
				value = &AgentSession{}
			}
			if err := json.Unmarshal(raw, value); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var before, after any
			if err := json.Unmarshal(raw, &before); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &after); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("API encoding changed fixture: %s", encoded)
			}
		})
	}
}
