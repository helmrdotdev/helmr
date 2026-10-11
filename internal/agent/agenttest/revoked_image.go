package agenttest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/jackc/pgx/v5"
)

// PinRevokedImage bootstraps certified-image lineage for transport tests. Real
// preparation publication is exercised by the Agent storage tests. Revocation is
// derived from its actual Secret exposure, never inserted into the derived view.
func (f Fixture) PinRevokedImage(t *testing.T, computer uuid.UUID) {
	t.Helper()
	local, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, rootID, secretID, versionID := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	writer := blockformat.Writer{Source: local, Sink: local, Scope: f.Environment.String(), ActiveKey: key.String(), Keys: map[string][]byte{key.String(): bytes.Repeat([]byte{7}, 32)}, PackLimit: blockformat.MinPackLimit}
	locator, err := writer.Empty(t.Context(), 1<<20, 64)
	if err != nil {
		t.Fatal(err)
	}
	root, err := disk.NewVersionRoot(locator, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := root.Digest()
	if err != nil {
		t.Fatal(err)
	}
	var preparation, spec uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT preparation_id,preparation_spec_id FROM computers WHERE environment_id=$1 AND id=$2`, f.Environment, computer).Scan(&preparation, &spec); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `
 INSERT INTO computer_data_keys(id,environment_id,writer_preparation_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'test',decode('01','hex'));
 INSERT INTO cas_blobs(digest,size_bytes) VALUES($4,$5);
 INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) SELECT org_id,$4,$5,'application/octet-stream' FROM environments WHERE id=$2;
 INSERT INTO computer_objects(environment_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection,certified_at) SELECT id,$4,org_id,project_id,$5,'application/octet-stream','root',$6,'{}',clock_timestamp() FROM environments WHERE id=$2;
 INSERT INTO computer_object_keys(environment_id,digest,key_id,is_direct) VALUES($2,$4,$1,true);
 INSERT INTO computer_disk_roots(environment_id,id,locator) VALUES($2,$7,$8);
 UPDATE computer_preparations SET status='succeeded',write_key_id=$1,logical_bytes=1048576,capture_root=$9,capture_evidence='fixture publication' WHERE environment_id=$2 AND id=$3;
 INSERT INTO computer_images(environment_id,id,preparation_spec_id,preparation_id,seq,root_id,publication_evidence) VALUES($2,$3,$10,$3,1,$7,'fixture publication');
 UPDATE computers SET initial_root_id=$7,initial_root_digest=decode(substring($9::text from 8),'hex'),image_id=$3 WHERE environment_id=$2 AND id=$11;
 INSERT INTO secrets(environment_id,id,name,status,revoked_at) VALUES($2,$12,'image-secret','revoked',clock_timestamp());
 INSERT INTO secret_versions(id,secret_id,version,nonce,ciphertext) VALUES($13,$12,1,decode(repeat('00',12),'hex'),decode(repeat('00',16),'hex'));
 INSERT INTO secret_exposures(environment_id,preparation_id,secret_id,version_id,revocation_generation) VALUES($2,$3,$12,$13,0);
 `, pgx.QueryExecModeSimpleProtocol, key, f.Environment, preparation, fmt.Sprintf("sha256:%x", locator.Pack.Digest), locator.Pack.Size, locator.Pack.Rank, rootID, string(encoded), identity, spec, computer, secretID, versionID)
	var revoked bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM computer_secret_revocations WHERE environment_id=$1 AND computer_id=$2)`, f.Environment, computer).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("image lineage not revoked: %v %v", revoked, err)
	}
}
