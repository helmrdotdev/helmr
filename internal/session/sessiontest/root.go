package sessiontest

import (
	"bytes"
	"encoding/json"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/jackc/pgx/v5"
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

// ReplaceInitialRoot gives the fixture Computer a new certified initial root,
// leaving the previous root and its descriptor unreferenced.
func ReplaceInitialRoot(t *testing.T, f Fixture) {
	t.Helper()
	r := newInitialRoot(t, f.Environment)
	_, err := f.Pool.Exec(t.Context(), `
      INSERT INTO disk_encryption_keys(id,environment_id,wrapping_key_id,wrapped_key) VALUES($2,$1,'test',decode('01','hex'));
      INSERT INTO cas_blobs(digest,size_bytes) VALUES($3,$4);
      INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) SELECT org_id,$3,$4,'application/octet-stream' FROM environments WHERE id=$1;
      INSERT INTO disk_objects(environment_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection,certified_at) SELECT id,$3,org_id,project_id,$4,'application/octet-stream','root',$5,$6,clock_timestamp() FROM environments WHERE id=$1;
      INSERT INTO disk_object_keys(environment_id,digest,key_id,is_direct) VALUES($1,$3,$2,true);
      `+DiskRootInsert("$1", "$7", "$8")+`;
      UPDATE computers SET initial_root_id=$7,initial_root_digest=decode(substring($9::text from 8),'hex') WHERE environment_id=$1 AND id=$10;
    `, pgx.QueryExecModeSimpleProtocol, f.Environment, r.key, r.root.Pack.Digest, r.root.Pack.SizeBytes, r.root.Pack.Rank, string(r.inspection), r.id, string(r.locator), r.identity, f.Computer)
	if err != nil {
		t.Fatal(err)
	}
}
