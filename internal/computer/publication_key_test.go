package computer

import (
	"encoding/hex"
	"testing"
	"uuid"
)

// Pins persisted before this derivation moved must stay addressable: the
// derivation bytes of every publication kind are fixed.
func TestPublicationKeysAreStable(t *testing.T) {
	owner := uuid.MustParse("01997f91-0564-7000-a000-000000000001")
	operation := uuid.MustParse("01997f91-0564-7000-a000-000000000002")
	for _, test := range []struct {
		name string
		key  []byte
		want string
	}{
		{"initial derivation", derivePublicationKey("initial", owner, operation), "bfdaf942e3665ecff0daf51d6d5766c90f8721efef290d5ffb4cf85b81b50384"},
		{"checkpoint derivation", derivePublicationKey("checkpoint", owner, operation), "77c04ea0a6aae6bfb4f1294e60203b913e7e459169b0a6c3a6e4f13c08f904f4"},
		{"save", savePublicationKey(owner, 7, operation), "ccde7eee3146453f009cae98874c839348286516e693847d9ab5f5a8689272bb"},
		{"initial", initialPublicationKey(owner), hex.EncodeToString(derivePublicationKey("initial", owner, owner))},
		{"checkpoint", checkpointPublicationKey(owner), hex.EncodeToString(derivePublicationKey("checkpoint", owner, owner))},
		{"exported checkpoint", CheckpointPublicationKey(owner), hex.EncodeToString(derivePublicationKey("checkpoint", owner, owner))},
	} {
		if got := hex.EncodeToString(test.key); got != test.want {
			t.Errorf("%s key = %s, want %s", test.name, got, test.want)
		}
	}
}
