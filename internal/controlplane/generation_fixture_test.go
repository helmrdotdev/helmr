package controlplane

import (
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Framing-only identity for tests that do not publish or read physical bytes.
func testGenerationRoot(capacity int64) computer.GenerationRoot {
	return computer.GenerationRoot{FormatVersion: 1, LogicalBytes: capacity,
		Pack: computer.GenerationPack{Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 1024, Rank: 2},
		Page: computer.GenerationPage{Digest: "sha256:" + strings.Repeat("b", 64), Salt: strings.Repeat("c", 64), KeyID: "01912345-6789-7abc-8def-0123456789ab", Kind: 3, Count: 1, SizeBytes: 128}, Offset: 8}
}

// Publish real authenticated bytes with a retained Runtime writer. Higher-level
// checkpoint/outcome fixtures exercise their own live commit fences separately.
func retainedTestGeneration(t *testing.T, pool *pgxpool.Pool, server *Server, runtimeID string, publicationKey []byte) computer.GenerationRoot {
	t.Helper()
	ctx := t.Context()
	var owner dispatch.ComputerPreparation
	var desired int64
	var pinned pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT org_id,project_id,environment_id,workspace_id,reserved_guest_ephemeral_disk_bytes,desired_version,computer_write_key_id FROM runtime_instances WHERE id=$1`, runtimeID).Scan(&owner.OrgID, &owner.ProjectID, &owner.EnvironmentID, &owner.ComputerID, &owner.LogicalBytes, &desired, &pinned); err != nil {
		t.Fatal(err)
	}
	if !pinned.Valid {
		pinned = pgvalue.NewUUIDv7()
		dbtest.MustExec(t, ctx, pool, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'fixture',decode('01','hex'))`, pinned, owner.EnvironmentID, owner.ComputerID)
		dbtest.MustExec(t, ctx, pool, `UPDATE runtime_instances SET computer_write_key_id=$2 WHERE id=$1`, runtimeID, pinned)
	}
	key := make([]byte, 32)
	key[0] = 42
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writer := blockformat.Writer{Source: store, Sink: store, Scope: "fixture", ActiveKey: pgvalue.UUIDString(pinned), Keys: map[string][]byte{pgvalue.UUIDString(pinned): key}, PackLimit: blockformat.MinPackLimit}
	locator, err := writer.Empty(ctx, owner.LogicalBytes, 64)
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := blockformat.InspectPack(ctx, store, "fixture", writer.Keys, locator.Pack)
	if err != nil {
		t.Fatal(err)
	}
	evidence := blockformat.ObjectInspection{Pack: &inspected}
	root, err := computer.NewGenerationRoot(locator, owner.LogicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	body, err := store.Get(ctx, root.Pack.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	uploaded, err := server.cas.Put(ctx, "application/octet-stream", body)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, object := range []*cas.Object{nil, &uploaded} {
		if err := recordComputerObjectLocked(ctx, tx, owner, pgvalue.UUID(uuid.MustParse(runtimeID)), publicationKey, desired, evidence, object, false, map[string]bool{writer.ActiveKey: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return root
}

// This supplies certified database state for placement tests, not proof of pack
// authentication or publication. Those boundaries are tested by the publisher.
