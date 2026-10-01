package controlplane

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/run"
)

// A rejected set value is described with the public JSON diagnostic of its
// canonicalization error, as when the control plane canonicalized it.
func TestRunMetadataRejectionUsesPublicJSONDiagnostics(t *testing.T) {
	for _, raw := range []string{`{"a":`, `{"a":1,"a":2}`, `[1,]`, `"\ud800"`} {
		_, canonicalErr := jsoncanon.Transform(json.RawMessage(raw))
		if canonicalErr == nil {
			t.Fatalf("%s canonicalized", raw)
		}
		want := fmt.Errorf("set value is invalid: %w", publicJSONDecodeError(canonicalErr)).Error()
		_, err := run.NewMetadataMutation("set", "key", json.RawMessage(raw), nil, nil)
		if err == nil {
			t.Fatalf("%s was accepted", raw)
		}
		if got := publicJSONDecodeError(err).Error(); got != want {
			t.Fatalf("%s rejection = %q, want %q", raw, got, want)
		}
	}
}
