package artifact

import (
	"bytes"
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
	for _, value := range []string{`{}`, `{"dirs":null,"ignorePatterns":[]}`, `{"dirs":["agents"],"ignorePatterns":null}`, `{"assets":[],"dirs":["agents"],"external":[],"ignorePatterns":[],"unexpectedOption":[]}`, `{"dirs":["../agents"],"ignorePatterns":[]}`} {
		if _, err := ParseBuildConfig([]byte(value)); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
}
