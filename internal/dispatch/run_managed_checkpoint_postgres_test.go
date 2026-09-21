package dispatch

import (
	"errors"
	"fmt"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func TestManagedCheckpointRequiresFullSourceProof(t *testing.T) {
	for _, actor := range []bool{false, true} {
		t.Run(fmt.Sprintf("actor_%t", actor), func(t *testing.T) {
			for _, mode := range []string{"valid timer", "wrong parent", "open source runtime", "failed unreclaimed", "failed reclaimed", "lost reclaimed", "not materialized"} {
				t.Run(mode, func(t *testing.T) {
					f := newRunPlacementFixture(t)
					_, _, checkpointID := prepareSuspendedRestore(t, f, actor)
					switch mode {
					case "wrong parent":
						dbtest.MustExec(t, f.ctx, f.pool, `WITH other_head AS (INSERT INTO workspace_versions(id,environment_id,workspace_id,status,content_digest,size_bytes,entry_count,ownership_generation,writer_generation,published_at,parent_version_id,source_workspace_lease_id,artifact_id) SELECT gen_random_uuid(),v.environment_id,v.workspace_id,'committed',v.content_digest,v.size_bytes,v.entry_count,v.ownership_generation,v.writer_generation,now(),v.parent_version_id,v.source_workspace_lease_id,v.artifact_id FROM workspace_versions v JOIN run_checkpoints c ON c.private_workspace_version_id=v.id WHERE c.id=$1 RETURNING id) UPDATE workspace_versions SET parent_version_id=(SELECT id FROM other_head) WHERE id=(SELECT private_workspace_version_id FROM run_checkpoints WHERE id=$1)`, checkpointID)
					case "failed unreclaimed", "failed reclaimed", "lost reclaimed", "not materialized":
						state := "failed"
						if mode == "lost reclaimed" {
							state = "lost"
						}
						dbtest.MustExec(t, f.ctx, f.pool, `UPDATE runtime_instances SET observed_state=$2,terminal_reason_code='source_cleanup' WHERE id=(SELECT l.runtime_instance_id FROM run_checkpoints c JOIN run_leases l ON l.id=c.source_run_lease_id WHERE c.id=$1)`, checkpointID, state)
						if mode == "failed reclaimed" || mode == "lost reclaimed" {
							method := "host_reconciled"
							if mode == "lost reclaimed" {
								method = "provider_absent"
							}
							dbtest.MustExec(t, f.ctx, f.pool, `UPDATE runtime_instances SET reclaim_evidence=jsonb_set(reclaim_evidence,'{method}',to_jsonb($2::text)) WHERE id=(SELECT l.runtime_instance_id FROM run_checkpoints c JOIN run_leases l ON l.id=c.source_run_lease_id WHERE c.id=$1)`, checkpointID, method)
						}
						if mode == "failed unreclaimed" {
							dbtest.MustExec(t, f.ctx, f.pool, `UPDATE runtime_instances SET reclaimed_at=NULL,reclaim_evidence=NULL WHERE id=(SELECT l.runtime_instance_id FROM run_checkpoints c JOIN run_leases l ON l.id=c.source_run_lease_id WHERE c.id=$1)`, checkpointID)
						}
						if mode == "not materialized" {
							dbtest.MustExec(t, f.ctx, f.pool, `UPDATE runtime_instances SET reclaim_evidence=jsonb_set(reclaim_evidence,'{method}','"not_materialized"') WHERE id=(SELECT l.runtime_instance_id FROM run_checkpoints c JOIN run_leases l ON l.id=c.source_run_lease_id WHERE c.id=$1)`, checkpointID)
						}
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
					_, err = lockRunPlacementAuthority(f.ctx, tx, candidate, false)
					if mode == "valid timer" || mode == "failed reclaimed" || mode == "lost reclaimed" {
						if err != nil {
							t.Fatalf("valid managed checkpoint rejected: %v", err)
						}
					} else if !errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("invalid managed checkpoint admitted: %v", err)
					}
				})
			}
		})
	}
}
