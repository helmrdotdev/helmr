package dbtest

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func MustExec(t *testing.T, ctx context.Context, executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, query string, args ...any) {
	t.Helper()
	if _, err := executor.Exec(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func Digest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return sha256sum.FormatDigest(sum[:])
}

func Hash(seed string) []byte {
	sum := sha256.Sum256([]byte(seed))
	return sum[:]
}

func ShortID(id uuid.UUID) string {
	return strings.ReplaceAll(id.String(), "-", "")[20:]
}

type CheckpointArtifactIDs struct {
	RuntimeConfig uuid.UUID
	VMState       uuid.UUID
	Memory        uuid.UUID
	ScratchDisk   uuid.UUID
}

func InsertCheckpointArtifacts(t *testing.T, ctx context.Context, executor interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, runID uuid.UUID, seed string) CheckpointArtifactIDs {
	t.Helper()
	ids := CheckpointArtifactIDs{
		RuntimeConfig: uuid.NewV7(),
		VMState:       uuid.NewV7(),
		Memory:        uuid.NewV7(),
		ScratchDisk:   uuid.NewV7(),
	}
	digests := []string{
		Digest(seed + "-runtime-config"),
		Digest(seed + "-vm-state"),
		Digest(seed + "-memory"),
		Digest(seed + "-scratch-disk"),
	}
	if err := executor.QueryRow(ctx, `
		WITH authority AS (
			SELECT org_id, project_id, environment_id FROM runs WHERE id = $1
		), descriptors(id, digest, kind, media_type) AS (
			VALUES
				($2::uuid, $6::text, 'run_checkpoint_config'::artifact_kind, 'application/vnd.helmr.checkpoint.runtime-config.v0+json'),
				($3::uuid, $7::text, 'run_checkpoint_vm_state'::artifact_kind, 'application/vnd.helmr.firecracker.vm-state.v0'),
				($4::uuid, $8::text, 'run_checkpoint_memory'::artifact_kind, 'application/vnd.helmr.firecracker.memory.v0+filepack'),
				($5::uuid, $9::text, 'run_checkpoint_scratch_disk'::artifact_kind, 'application/vnd.helmr.firecracker.scratch-disk.v0+filepack')
		), inserted_cas AS (
			INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
			SELECT authority.org_id, descriptors.digest, 1, descriptors.media_type
			  FROM authority CROSS JOIN descriptors
			ON CONFLICT (org_id, digest) DO NOTHING
		)
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		SELECT descriptors.id, authority.org_id, authority.project_id, authority.environment_id,
		       descriptors.digest, descriptors.kind, 1, descriptors.media_type
		  FROM authority CROSS JOIN descriptors
		RETURNING artifacts.id
	`, runID, ids.RuntimeConfig, ids.VMState, ids.Memory, ids.ScratchDisk,
		digests[0], digests[1], digests[2], digests[3]).Scan(new(uuid.UUID)); err != nil {
		t.Fatal(err)
	}
	return ids
}
