package controlplane

import (
	"encoding/json"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// retainedTestVersion uploads real authenticated root bytes for a
// Runtime's retained writer key and records them as a certified root of its
// Computer, returning the root and its inspection. It supplies certified
// database state for the checkpoint fixtures, which pin it through the
// owner's object reuse and exercise their own live commit fences; object
// recording and its authority are tested by the computer owner.
func retainedTestVersion(t *testing.T, pool *pgxpool.Pool, objects cas.Store, instanceID string) (disk.VersionRoot, blockformat.ObjectInspection) {
	t.Helper()
	ctx := t.Context()
	var orgID, projectID, environmentID, computerID, pinned pgtype.UUID
	var logicalBytes int64
	if err := pool.QueryRow(ctx, `SELECT org_id,project_id,environment_id,computer_id,reserved_guest_ephemeral_disk_bytes,write_key_id FROM computer_instances WHERE id=$1`, instanceID).Scan(&orgID, &projectID, &environmentID, &computerID, &logicalBytes, &pinned); err != nil {
		t.Fatal(err)
	}
	if !pinned.Valid {
		pinned = pgvalue.NewUUIDv7()
		dbtest.MustExec(t, ctx, pool, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'fixture',decode('01','hex'))`, pinned, environmentID, computerID)
		dbtest.MustExec(t, ctx, pool, `UPDATE computer_instances SET write_key_id=$2 WHERE id=$1`, instanceID, pinned)
	}
	key := make([]byte, 32)
	key[0] = 42
	local, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writer := blockformat.Writer{Source: local, Sink: local, Scope: "fixture", ActiveKey: pgvalue.UUIDString(pinned), Keys: map[string][]byte{pgvalue.UUIDString(pinned): key}, PackLimit: blockformat.MinPackLimit}
	locator, err := writer.Empty(ctx, logicalBytes, 64)
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := blockformat.InspectPack(ctx, local, "fixture", writer.Keys, locator.Pack)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspected.Pages) != 1 || len(inspected.Pages[0].Children) != 0 || len(inspected.Pages[0].Segments) != 0 {
		t.Fatal("empty root references other objects")
	}
	evidence := blockformat.ObjectInspection{Pack: &inspected}
	inspection, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	root, err := disk.NewVersionRoot(locator, logicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	body, err := local.Get(ctx, root.Pack.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	uploaded, err := objects.Put(ctx, "application/octet-stream", body)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,$2) ON CONFLICT DO NOTHING`, uploaded.Digest, uploaded.SizeBytes)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_objects(environment_id,computer_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection) VALUES($1,$2,$3,$4,$5,$6,'application/octet-stream','root',$7,$8)`, environmentID, computerID, uploaded.Digest, orgID, projectID, uploaded.SizeBytes, locator.Pack.Rank, inspection)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id,is_direct) VALUES($1,$2,$3,$4,true)`, environmentID, computerID, uploaded.Digest, pinned)
	q := db.New(tx)
	if _, err = q.UpsertCasObject(ctx, db.UpsertCasObjectParams{OrgID: orgID, Digest: uploaded.Digest, SizeBytes: uploaded.SizeBytes, MediaType: uploaded.MediaType}); err != nil {
		t.Fatal(err)
	}
	if n, err := q.CertifyComputerObject(ctx, db.CertifyComputerObjectParams{EnvironmentID: environmentID, ComputerID: computerID, Digest: uploaded.Digest}); err != nil || n != 1 {
		t.Fatalf("certify root n=%d err=%v", n, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return root, evidence
}
