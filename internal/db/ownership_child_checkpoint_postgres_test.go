package db

import (
	"bytes"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestOwnershipDelayedChildBindingUsesLastEligibleWait(t *testing.T) {
	ctx := t.Context()
	f := newRunLeaseClaimFixture(t, ctx)
	work := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	startTaskCompletionWork(t, ctx, f, work)
	v := ownershipVersionParams(t, f, work)
	if _, err := f.queries.CreatePrivateCheckpointWorkspaceVersion(ctx, v); err != nil {
		t.Fatal(err)
	}
	claimID, childID, checkpointID := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, "INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'task.child.invoke',decode(repeat('a1',32),'hex'),decode(repeat('a2',32),'hex'),now())", claimID, f.environmentID)
	var version int64
	if err := f.pool.QueryRow(ctx, "SELECT state_version FROM runs WHERE id=$1", work.runID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	w, err := f.queries.RegisterSameWorkspaceChildCall(ctx, RegisterSameWorkspaceChildCallParams{ID: pgvalue.UUID(uuid.NewV7()), ChildTargetDeclaredID: pgvalue.Text("test-task"), ChildClaimID: pgvalue.UUID(claimID), ChildRequest: []byte("{}"), RegistrationRequestFingerprint: pgvalue.Text(dbtest.Digest("delayed-child")), AttemptNumber: 1, CurrentRunLeaseID: pgvalue.UUID(work.leaseID), ResumeAttachID: pgvalue.UUID(uuid.NewV7()), RunID: pgvalue.UUID(work.runID), EnvironmentID: pgvalue.UUID(f.environmentID), WorkspaceID: v.WorkspaceID, ExpectedRunningStateVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO run_checkpoints(id,run_id,attempt_number,run_wait_id,source_run_lease_id,source_workspace_lease_id,workspace_id,base_workspace_version_id) SELECT $1,$2,1,$3,$4,id,workspace_id,base_version_id FROM workspace_leases WHERE owner_run_lease_id=$4`, checkpointID, work.runID, w.ID, work.leaseID)
	a := dbtest.InsertCheckpointArtifacts(t, ctx, f.pool, work.runID, "delayed-child")
	if _, err := f.queries.MarkRunCheckpointReady(ctx, MarkRunCheckpointReadyParams{ID: pgvalue.UUID(checkpointID), RunID: w.RunID, AttemptNumber: 1, PrivateWorkspaceVersionID: v.ID, RuntimeConfigArtifactID: pgvalue.UUID(a.RuntimeConfig), VMStateArtifactID: pgvalue.UUID(a.VMState), MemoryArtifactID: pgvalue.UUID(a.Memory), ScratchDiskArtifactID: pgvalue.UUID(a.ScratchDisk), RestoreManifest: []byte(`{"version":0}`), ReadyRequestFingerprint: pgvalue.Text("ready")}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, f.pool, "UPDATE run_waits SET suspension_state='checkpointing',suspend_checkpoint_id=$2,checkpoint_request_version=1 WHERE id=$1", w.ID, checkpointID)
	dbtest.MustExec(t, ctx, f.pool, "UPDATE runs SET active_started_at=NULL WHERE id=$1", w.RunID)
	dbtest.MustExec(t, ctx, f.pool, `WITH child AS (INSERT INTO runs(id,org_id,project_id,environment_id,deployment_id,deployment_definition_id,entrypoint_kind,entrypoint_declared_id,cause_kind,parent_run_id,parent_owns_lifecycle,claim_id,workspace_id,base_workspace_version_id,payload,queue_name,queue_origin_at,queue_score_at,max_active_duration_ms,retry_policy,root_span_id)
 SELECT $1,org_id,project_id,environment_id,deployment_id,deployment_definition_id,'task',entrypoint_declared_id,'child',id,true,$2,workspace_id,$3,'{}',queue_name,now(),now(),max_active_duration_ms,retry_policy,root_span_id FROM runs WHERE id=$4 RETURNING id,workspace_id,base_workspace_version_id) INSERT INTO run_attempts(run_id,number,entrypoint_kind,workspace_id,base_workspace_version_id) SELECT id,1,'task',workspace_id,base_workspace_version_id FROM child`, childID, claimID, v.ID, w.RunID)
	p := CommitSameWorkspaceChildCheckpointReadyParams{CheckpointRequestVersion: 1, BaseWorkspaceVersionID: v.ID, BaseWorkspaceContentDigest: pgvalue.Text(v.ContentDigest), OwnershipGeneration: pgtype.Int8{Int64: v.OwnershipGeneration, Valid: true}, ParentWriterGeneration: pgtype.Int8{Int64: v.WriterGeneration, Valid: true}, CheckpointedAt: pgvalue.Timestamptz(time.Now()), RunWaitID: w.ID, EnvironmentID: pgvalue.UUID(f.environmentID), ParentRunID: w.RunID, WorkspaceID: v.WorkspaceID, ParentAttemptNumber: 1, ChildClaimID: pgvalue.UUID(claimID), ParentRunLeaseID: w.CurrentRunLeaseID, SuspendCheckpointID: pgvalue.UUID(checkpointID), ExpectedRunStateVersion: w.ExpectedRunStateVersion, ChildRunID: pgvalue.UUID(childID)}
	for _, test := range []struct {
		name   string
		mutate func(*CommitSameWorkspaceChildCheckpointReadyParams)
	}{
		{"missing wait", func(p *CommitSameWorkspaceChildCheckpointReadyParams) { p.RunWaitID = pgvalue.UUID(uuid.NewV7()) }},
		{"wrong request", func(p *CommitSameWorkspaceChildCheckpointReadyParams) { p.CheckpointRequestVersion++ }},
		{"wrong child", func(p *CommitSameWorkspaceChildCheckpointReadyParams) { p.ChildRunID = pgvalue.UUID(uuid.NewV7()) }},
		{"wrong claim", func(p *CommitSameWorkspaceChildCheckpointReadyParams) { p.ChildClaimID = pgvalue.UUID(uuid.NewV7()) }},
		{"wrong base", func(p *CommitSameWorkspaceChildCheckpointReadyParams) { p.BaseWorkspaceVersionID = v.ParentVersionID }},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := ownershipRunWaitSnapshot(t, f, w)
			bad := p
			test.mutate(&bad)
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := New(tx).CommitSameWorkspaceChildCheckpointReady(ctx, bad); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("rejection=%v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, ownershipRunWaitSnapshot(t, f, w)) {
				t.Fatal("rejected binding moved parent or Wait")
			}
		})
	}
	for _, sql := range []string{"UPDATE runs SET parent_owns_lifecycle=false WHERE id=$1", "UPDATE run_waits SET expected_run_state_version=expected_run_state_version+1 WHERE id=$1"} {
		id := p.ChildRunID
		if sql[7:16] == "run_waits" {
			id = w.ID
		}
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		dbtest.MustExec(t, ctx, tx, sql, id)
		if _, err := New(tx).CommitSameWorkspaceChildCheckpointReady(ctx, p); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("invalid authority %s: %v", sql, err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.queries.CommitSameWorkspaceChildCheckpointReady(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.ChildRunID != p.ChildRunID || got.SuspensionState != RunWaitStateParked {
		t.Fatalf("binding=%+v", got)
	}
}
