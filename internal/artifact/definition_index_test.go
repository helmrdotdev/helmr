package artifact

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestDefinitionIndexCompilerFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/definition-index.json")
	if err != nil {
		t.Fatal(err)
	}
	index, err := ParseDefinitionIndex(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Agents) != 1 || index.Agents[0].ID != "coder" || !index.Computers[0].ThroughAgent {
		t.Fatal(index)
	}
	recoded, err := CanonicalDefinitionIndex(index)
	if err != nil || !bytes.Equal(raw, recoded) {
		t.Fatalf("round trip: %s, %v", recoded, err)
	}
}
func TestDefinitionIndexRejectsInvalidReferences(t *testing.T) {
	for name, mutate := range map[string]func(*DefinitionIndex){
		"empty":               func(i *DefinitionIndex) { i.Agents = nil },
		"duplicate Agent":     func(i *DefinitionIndex) { i.Agents = append(i.Agents, i.Agents[0]) },
		"duplicate Computer":  func(i *DefinitionIndex) { i.Computers = append(i.Computers, i.Computers[0]) },
		"missing Computer":    func(i *DefinitionIndex) { i.Agents[0].ComputerDefinitionID = "missing" },
		"wrong inline export": func(i *DefinitionIndex) { i.Computers[0].ExportName = "missing" },
		"wrong inline module": func(i *DefinitionIndex) { i.Computers[0].ModulePath = testModulePath("b") },
		"oversized export":    func(i *DefinitionIndex) { i.Agents[0].ExportName = strings.Repeat("a", 257) },
		"control export":      func(i *DefinitionIndex) { i.Agents[0].ExportName = "bad\nname" },
		"invalid UTF8":        func(i *DefinitionIndex) { i.Agents[0].ExportName = string([]byte{255}) },
		"traversal":           func(i *DefinitionIndex) { i.Computers[0].ModulePath = "helmr/app/../entry-0.mjs" },
		"node_modules":        func(i *DefinitionIndex) { i.Agents[0].ModulePath = "node_modules/task.js" },
		"config":              func(i *DefinitionIndex) { i.Agents[0].ModulePath = "helmr.config.ts" },
	} {
		t.Run(name, func(t *testing.T) {
			i := testDefinitionIndex()
			mutate(&i)
			if ValidateDefinitionIndex(i) == nil {
				t.Fatal("accepted invalid index")
			}
		})
	}
}
func TestDefinitionIndexRejectsOpenMissingAndNoncanonicalShapes(t *testing.T) {
	raw, err := CanonicalDefinitionIndex(testDefinitionIndex())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"apiVersion", "agents", "computers"} {
		var doc map[string]any
		_ = json.Unmarshal(raw, &doc)
		delete(doc, key)
		candidate, _ := json.Marshal(doc)
		candidate, _ = jsoncanon.Transform(candidate)
		if _, err := ParseDefinitionIndex(candidate); err == nil {
			t.Fatalf("accepted missing %s", key)
		}
	}
	for _, mutate := range []func(map[string]any){
		func(d map[string]any) { d["unknown"] = true },
		func(d map[string]any) { d["agents"].([]any)[0].(map[string]any)["unknown"] = true },
		func(d map[string]any) { delete(d["computers"].([]any)[0].(map[string]any), "throughAgent") },
		func(d map[string]any) { d["computers"].([]any)[0].(map[string]any)["throughAgent"] = nil },
	} {
		var doc map[string]any
		_ = json.Unmarshal(raw, &doc)
		mutate(doc)
		candidate, _ := json.Marshal(doc)
		candidate, _ = jsoncanon.Transform(candidate)
		if _, err := ParseDefinitionIndex(candidate); err == nil {
			t.Fatal("accepted divergent shape")
		}
	}
	for _, candidate := range [][]byte{nil, append([]byte(" "), raw...), bytes.Repeat([]byte("x"), int(MaxProgramFileSizeBytes)+1)} {
		if _, err := ParseDefinitionIndex(candidate); err == nil {
			t.Fatal("accepted invalid bytes")
		}
	}
}
func testModulePath(digit string) string {
	if digit == "a" {
		return "helmr/app/entry-0.mjs"
	}
	return "helmr/app/entry-1.mjs"
}
