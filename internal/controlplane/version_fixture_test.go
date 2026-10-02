package controlplane

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// uploadedTestVersion uploads authenticated root bytes for the Instance's
// retained writer key. The caller registers and certifies them through HTTP.
func uploadedTestVersion(t *testing.T, pool *pgxpool.Pool, objects cas.Store, instanceID string) (disk.VersionRoot, blockformat.ObjectInspection) {
	t.Helper()
	ctx := t.Context()
	var orgID, projectID, environmentID, computerID, pinned pgtype.UUID
	var logicalBytes int64
	if err := pool.QueryRow(ctx, `SELECT org_id,project_id,environment_id,computer_id,reserved_guest_ephemeral_disk_bytes,write_key_id FROM computer_instances WHERE id=$1`, instanceID).Scan(&orgID, &projectID, &environmentID, &computerID, &logicalBytes, &pinned); err != nil {
		t.Fatal(err)
	}
	if !pinned.Valid {
		pinned = pgvalue.NewUUIDv7()
		dbtest.MustExec(t, ctx, pool, `INSERT INTO computer_data_keys(id,environment_id,writer_computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'fixture',decode('01','hex'))`, pinned, environmentID, computerID)
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
	root, err := disk.NewVersionRoot(locator, logicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	body, err := local.Get(ctx, root.Pack.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	_, err = objects.Put(ctx, "application/octet-stream", body)
	if err != nil {
		t.Fatal(err)
	}
	return root, evidence
}
