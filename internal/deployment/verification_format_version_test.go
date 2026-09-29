package deployment

import (
	"encoding/json"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestVerificationResultZeroFormatVersionRequiresExplicitCanonicalField(t *testing.T) {
	cases := []struct {
		name  string
		value any
		parse func([]byte) error
	}{
		{"verification", testProgramVerificationResult(t), func(raw []byte) error { _, err := ParseVerificationResult(raw); return err }},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			raw, err := json.Marshal(item.value)
			if err != nil {
				t.Fatal(err)
			}
			canonical, err := jsoncanon.Transform(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := item.parse(canonical); err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{"missing", "null", "unsupported"} {
				var document map[string]json.RawMessage
				if err := json.Unmarshal(canonical, &document); err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "missing":
					delete(document, "formatVersion")
				case "null":
					document["formatVersion"] = json.RawMessage(`null`)
				case "unsupported":
					document["formatVersion"] = json.RawMessage(`1`)
				}
				encoded, err := json.Marshal(document)
				if err != nil {
					t.Fatal(err)
				}
				candidate, err := jsoncanon.Transform(encoded)
				if err != nil {
					t.Fatal(err)
				}
				if item.parse(candidate) == nil {
					t.Fatalf("accepted %s formatVersion", mode)
				}
			}
		})
	}
}
