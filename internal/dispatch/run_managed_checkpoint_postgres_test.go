package dispatch

import (
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func TestActorManagedCheckpointRequiresFullSourceProof(t *testing.T) {
	for _, mode := range []string{"valid timer", "wrong parent", "open source runtime"} {
		t.Run(mode, func(t *testing.T) {
			f := newRunPlacementFixture(t)
			_, _, checkpointID := prepareActorSuspendedRestore(t, f)
			switch mode {
			case "wrong parent":
				dbtest.MustExec(t, f.ctx, f.pool, `WITH other_head AS (INSERT INTO workspace_versions(id,environment_id,workspace_id,status,content_digest,size_bytes,entry_count,ownership_generation,writer_generation,published_at,parent_version_id,source_workspace_lease_id,artifact_id) SELECT gen_random_uuid(),v.environment_id,v.workspace_id,'committed',v.content_digest,v.size_bytes,v.entry_count,v.ownership_generation,v.writer_generation,now(),v.parent_version_id,v.source_workspace_lease_id,v.artifact_id FROM workspace_versions v JOIN run_checkpoints c ON c.private_workspace_version_id=v.id WHERE c.id=$1 RETURNING id) UPDATE workspace_versions SET parent_version_id=(SELECT id FROM other_head) WHERE id=(SELECT private_workspace_version_id FROM run_checkpoints WHERE id=$1)`, checkpointID)
			case "open source runtime":
				dbtest.MustExec(t, f.ctx, f.pool, `UPDATE runtime_instances SET observed_state='ready',observed_desired_version=desired_version-1,terminal_at=NULL,terminal_reason_code=NULL,reclaimed_at=NULL,reclaim_evidence=NULL WHERE id=(SELECT l.runtime_instance_id FROM run_checkpoints c JOIN run_leases l ON l.id=c.source_run_lease_id WHERE c.id=$1)`, checkpointID)
			}
			tx, err := f.pool.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(f.ctx)
			candidate := f.candidate()
			candidate.ExpectedRunRevision = 3
			_, err = lockRunPlacementAuthority(f.ctx, tx, candidate)
			if mode == "valid timer" {
				if err != nil {
					t.Fatalf("valid managed checkpoint rejected: %v", err)
				}
			} else if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("invalid managed checkpoint admitted: %v", err)
			}
		})
	}
}
