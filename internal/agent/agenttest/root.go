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

// DiskRootInsert returns a statement that records a disk root from its
// JSON-encoded disk.VersionRoot parameter, expanding it into the typed columns.
// The arguments are the parameter references, for example "$1".
func DiskRootInsert(environment, id, root string) string {
	return `INSERT INTO disk_roots(environment_id,id,format_version,logical_bytes,pack_digest,pack_size_bytes,pack_rank,page_digest,page_salt,page_key_id,page_kind,page_count,page_size_bytes,page_offset)
 SELECT ` + environment + `,` + id + `,(r->>'format_version')::int,(r->>'logical_bytes')::bigint,r->'pack'->>'digest',(r->'pack'->>'size_bytes')::bigint,(r->'pack'->>'rank')::int,
 r->'page'->>'digest',r->'page'->>'salt',(r->'page'->>'key_id')::uuid,(r->'page'->>'kind')::int,(r->'page'->>'count')::int,(r->'page'->>'size_bytes')::bigint,(r->>'offset')::bigint
 FROM (SELECT ` + root + `::jsonb AS r) root`
}
