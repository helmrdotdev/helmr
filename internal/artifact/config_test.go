package artifact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func TestBuildConfigClosedCanonicalAuthority(t *testing.T) {
	raw := []byte(`{"assets":[],"dirs":["agents"],"external":[],"ignorePatterns":[]}`)
	config, err := ParseBuildConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := CanonicalBuildConfig(config)
	if err != nil || !bytes.Equal(raw, encoded) {
		t.Fatalf("%s %v", encoded, err)
	}
	cloned := cloneBuildConfig(config)
	before, _ := BuildConfigDigest(config)
	config.Dirs[0] = "custom-agents"
	after, _ := BuildConfigDigest(config)
	if before == after || cloned.Dirs[0] != "agents" {
		t.Fatal("config binding or clone failed")
	}
	for name, value := range map[string]string{
		"empty document": `{}`,
		"unknown field":  `{"assets":[],"dirs":["agents"],"external":[],"ignorePatterns":[],"unexpectedOption":[]}`,
		"traversal":      `{"assets":[],"dirs":["../agents"],"external":[],"ignorePatterns":[]}`,
		"absolute":       `{"assets":[],"dirs":["/agents"],"external":[],"ignorePatterns":[]}`,
		"dot prefix":     `{"assets":[],"dirs":["./agents"],"external":[],"ignorePatterns":[]}`,
		"empty segment":  `{"assets":[],"dirs":["agents//nested"],"external":[],"ignorePatterns":[]}`,
		"unsorted":       `{"assets":[],"dirs":["z","a"],"external":[],"ignorePatterns":[]}`,
		"duplicate":      `{"assets":[],"dirs":["agents","agents"],"external":[],"ignorePatterns":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseBuildConfig([]byte(value)); err == nil {
				t.Fatalf("accepted %s", value)
			}
		})
	}
	// Change one field of an otherwise complete canonical document. Encoding
	// this map does not invoke the production config validator.
	for _, field := range []string{"assets", "dirs", "external", "ignorePatterns"} {
		for _, mode := range []string{"null", "absent", "too many"} {
			t.Run(field+"/"+mode, func(t *testing.T) {
				value := map[string]any{"assets": []string{}, "dirs": []string{"agents"}, "external": []string{}, "ignorePatterns": []string{}}
				switch mode {
				case "null":
					value[field] = nil
				case "absent":
					delete(value, field)
				case "too many":
					entries := make([]string, maxBuildConfigEntries+1)
					for i := range entries {
						entries[i] = fmt.Sprintf("entry%05d", i)
					}
					value[field] = entries
				}
				encoded, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ParseBuildConfig(encoded); err == nil {
					t.Fatalf("accepted %s %s", field, mode)
				}
			})
		}
	}
}
