package agenttest

import (
	"bytes"
	"encoding/json"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

type initialRootFixture struct {
	id, key             uuid.UUID
	root                disk.VersionRoot
	locator, inspection []byte
	identity            string
}

// This is certified fixture state, not an initial-image publisher. Specialized
// storage tests replace it with a graph whose wrapped keys and CAS they own.
func newInitialRoot(t *testing.T, environment uuid.UUID) initialRootFixture {
	t.Helper()
	local, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := initialRootFixture{id: uuid.NewV7(), key: uuid.NewV7()}
	writer := blockformat.Writer{Source: local, Sink: local, Scope: environment.String(), ActiveKey: r.key.String(), Keys: map[string][]byte{r.key.String(): bytes.Repeat([]byte{7}, 32)}, PackLimit: blockformat.MinPackLimit}
	locator, err := writer.Empty(t.Context(), 1<<20, 64)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := blockformat.InspectPack(t.Context(), local, writer.Scope, writer.Keys, locator.Pack)
	if err != nil {
		t.Fatal(err)
	}
	r.root, err = disk.NewVersionRoot(locator, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	r.locator, err = json.Marshal(r.root)
	if err != nil {
		t.Fatal(err)
	}
	r.inspection, err = json.Marshal(blockformat.ObjectInspection{Pack: &inspection})
	if err != nil {
		t.Fatal(err)
	}
	r.identity, err = r.root.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return r
}
