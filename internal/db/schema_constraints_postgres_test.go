package db

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Each rejected statement runs against real rows and recovers the same transaction.
// This catches CHECK's SQL-NULL acceptance and verifies the FK is actually reached.
func rejectSchemaRow(t *testing.T, tx pgx.Tx, code, statement string, args ...any) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), tx, "SAVEPOINT invalid_row")
	_, err := tx.Exec(t.Context(), statement, args...)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != code || pgerr.ConstraintName == "" {
		t.Errorf("invalid row error = %v, want SQLSTATE %s with constraint name", err, code)
	}
	dbtest.MustExec(t, t.Context(), tx, "ROLLBACK TO SAVEPOINT invalid_row")
	dbtest.MustExec(t, t.Context(), tx, "RELEASE SAVEPOINT invalid_row")
}

func TestSchemaFailurePayloadsRejectNullAndPreserveLifecycle(t *testing.T) {
	ctx := t.Context()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	sessionID := fixture.convertToActor(t, ctx, work, `{"enabled":false}`)
	scheduleID := uuid.NewV7()
	dbtest.MustExec(t, ctx, fixture.pool, `
		INSERT INTO schedules (id, environment_id, task_declared_id,
		 deployment_definition_id, deployment_id, cron_pattern, timezone,
		 state, effective_from, next_fire_at)
		SELECT $1, environment_id, declared_id, id, deployment_id,
		 '* * * * *', 'UTC', 'active', now(), now()
		FROM deployment_definitions WHERE id = $2
	`, scheduleID, fixture.taskDefinitionID)

	for _, tc := range []struct {
		name, statement, validCode string
		id                         uuid.UUID
	}{
		{"run failed", `UPDATE runs SET status='failed', terminal_at=now(), failure=$2 WHERE id=$1`, "task_failed", work.runID},
		{"run cancelled", `UPDATE runs SET status='cancelled', terminal_at=now(), failure=$2 WHERE id=$1`, "cancelled", work.runID},
		{"run expired", `UPDATE runs SET status='expired', terminal_at=now(), failure=$2 WHERE id=$1`, "expired", work.runID},
		{"run system failed", `UPDATE runs SET status='system_failed', terminal_at=now(), failure=$2 WHERE id=$1`, "system_failed", work.runID},
		{"session failed", `UPDATE sessions SET state='failed', failed_at=now(), failure=$2 WHERE id=$1`, "run_failed", sessionID},
		{"session cancelled", `UPDATE sessions SET state='cancelled', failure=$2 WHERE id=$1`, "cancelled", sessionID},
		{"schedule errored", `UPDATE schedules SET state='errored', last_failure=$2 WHERE id=$1`, "input_invalid", scheduleID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := fixture.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			for _, bad := range []struct {
				name  string
				value any
			}{
				{"SQL null", nil}, {"JSON null", `null`}, {"array", `[]`},
				{"missing code", `{"message":"failed","details":{}}`},
				{"null code", `{"code":null,"message":"failed","details":{}}`},
				{"boolean code", `{"code":true,"message":"failed","details":{}}`},
				{"null message", `{"code":"` + tc.validCode + `","message":null,"details":{}}`},
				{"numeric message", `{"code":"` + tc.validCode + `","message":42,"details":{}}`},
				{"null details", `{"code":"` + tc.validCode + `","message":"failed","details":null}`},
				{"extra field", `{"code":"` + tc.validCode + `","message":"failed","details":{},"extra":1}`},
			} {
				t.Run(bad.name, func(t *testing.T) { rejectSchemaRow(t, tx, "23514", tc.statement, tc.id, bad.value) })
			}
			dbtest.MustExec(t, ctx, tx, tc.statement, tc.id, `{"code":"`+tc.validCode+`","message":"failed","details":{}}`)
		})
	}
	// Optional prior schedule failures remain valid while active or archived.
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	dbtest.MustExec(t, ctx, tx, `UPDATE schedules SET state='errored', last_failure='{"code":"input_invalid","message":"failed","details":{}}' WHERE id=$1`, scheduleID)
	dbtest.MustExec(t, ctx, tx, `UPDATE schedules SET state='active' WHERE id=$1`, scheduleID)
	rejectSchemaRow(t, tx, "23514", `UPDATE schedules SET last_failure='{"code":null,"message":"failed","details":{}}' WHERE id=$1`, scheduleID)
	dbtest.MustExec(t, ctx, tx, `UPDATE schedules SET state='archived', deployment_id=NULL, deployment_definition_id=NULL, next_fire_at=NULL WHERE id=$1`, scheduleID)
	dbtest.MustExec(t, ctx, tx, `UPDATE schedules SET last_failure=NULL WHERE id=$1`, scheduleID)
}

func TestSchemaWorkspaceVersionArtifactAndFinalizationAuthority(t *testing.T) {
	ctx := t.Context()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	artifactID, versionID := uuid.NewV7(), uuid.NewV7()
	digest := dbtest.Digest("schema-workspace-version")
	dbtest.MustExec(t, ctx, tx, `INSERT INTO cas_objects (org_id,digest,size_bytes,media_type) VALUES ($1,$2,1,'application/octet-stream')`, fixture.orgID, digest)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO artifacts (id,org_id,project_id,environment_id,digest,kind,size_bytes,media_type) VALUES ($1,$2,$3,$4,$5,'workspace_version',1,'application/octet-stream')`, artifactID, fixture.orgID, fixture.projectID, fixture.environmentID, digest)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO workspace_versions (id,environment_id,workspace_id,parent_version_id,
		 artifact_id,content_digest,source_workspace_lease_id,ownership_generation,writer_generation)
		SELECT $1,environment_id,workspace_id,base_version_id,$2,$3,id,ownership_generation,writer_generation
		FROM workspace_leases WHERE owner_run_lease_id=$4
	`, versionID, artifactID, digest, work.leaseID)
	for _, set := range []string{
		"parent_version_id=NULL", "artifact_id=NULL",
		"source_workspace_lease_id=NULL",
	} {
		t.Run(set, func(t *testing.T) {
			rejectSchemaRow(t, tx, "23514", "UPDATE workspace_versions SET "+set+" WHERE id=$1", versionID)
		})
	}
	rejectSchemaRow(t, tx, "23503", `UPDATE workspace_versions SET artifact_id=$2 WHERE id=$1`, versionID, uuid.NewV7())
	rejectSchemaRow(t, tx, "23503", `UPDATE workspace_versions SET writer_generation=writer_generation+1 WHERE id=$1`, versionID)

	// A same-kind artifact in another environment must not cross the composite FK.
	otherEnvironment, crossScopeArtifact := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, tx, `INSERT INTO environments (id,org_id,project_id,slug,name,color_hex) VALUES ($1,$2,$3,'other','Other','#123456')`, otherEnvironment, fixture.orgID, fixture.projectID)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO artifacts (id,org_id,project_id,environment_id,digest,kind,size_bytes,media_type) VALUES ($1,$2,$3,$4,$5,'workspace_version',1,'application/octet-stream')`, crossScopeArtifact, fixture.orgID, fixture.projectID, otherEnvironment, digest)
	rejectSchemaRow(t, tx, "23503", `UPDATE workspace_versions SET artifact_id=$2 WHERE id=$1`, versionID, crossScopeArtifact)
	dbtest.MustExec(t, ctx, tx, "SAVEPOINT checkpoint_boundaries")
	assertCheckpointArtifactBoundaries(t, tx, fixture, work, versionID, otherEnvironment)
	dbtest.MustExec(t, ctx, tx, "ROLLBACK TO SAVEPOINT checkpoint_boundaries")

	var mountID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT workspace_mount_id FROM workspace_leases WHERE owner_run_lease_id=$1`, work.leaseID).Scan(&mountID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, tx, `UPDATE workspace_mounts SET state='unmounting', finalization_kind='capture', finalization_reason_code='workspace_exec_completed', staged_version_id=$2 WHERE id=$1`, mountID, versionID)
	rejectSchemaRow(t, tx, "23514", `UPDATE workspace_mounts SET finalization_reason_code=NULL WHERE id=$1`, mountID)
	rejectSchemaRow(t, tx, "23514", `UPDATE workspace_mounts SET finalization_kind=NULL WHERE id=$1`, mountID)
	rejectSchemaRow(t, tx, "23514", `UPDATE workspace_mounts SET finalization_kind=NULL, finalization_reason_code=NULL WHERE id=$1`, mountID)
	dbtest.MustExec(t, ctx, tx, `UPDATE workspace_mounts SET staged_version_id=NULL, finalization_kind='discard', finalization_reason_code='exec_failed' WHERE id=$1`, mountID)
	rejectSchemaRow(t, tx, "23514", `UPDATE workspace_mounts SET finalization_kind=NULL, finalization_reason_code=NULL, finalization_error='{}' WHERE id=$1`, mountID)
	dbtest.MustExec(t, ctx, tx, `UPDATE workspace_mounts SET state='unmounted', unmounted_at=now(), terminal_at=now(), terminal_reason_code='exec_failed' WHERE id=$1`, mountID)
	dbtest.MustExec(t, ctx, tx, `SAVEPOINT private_version`)
	dbtest.MustExec(t, ctx, tx, `UPDATE workspace_versions SET state='discarded', discarded_at=now() WHERE id=$1`, versionID)
	// Restore the private row before checking its alternate publication path.
	dbtest.MustExec(t, ctx, tx, `ROLLBACK TO SAVEPOINT private_version`)
	dbtest.MustExec(t, ctx, tx, `UPDATE workspace_versions SET state='committed', discarded_at=NULL, published_at=now() WHERE id=$1`, versionID)
}

func TestSchemaProvenanceAndExpiryRejectPartialTuples(t *testing.T) {
	ctx := t.Context()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	sessionID := fixture.convertToActor(t, ctx, work, `{"enabled":false}`)
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	claimID, recordID, waitID := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, tx, `INSERT INTO idempotency_claims (id,environment_id,operation,slot_hash,request_fingerprint,accepted_at,expires_at) VALUES ($1,$2,'run.create',$3,$4,now(),now()+interval '30 days')`, claimID, fixture.environmentID, dbtest.Hash("slot"), dbtest.Hash("request"))
	rejectSchemaRow(t, tx, "23514", `UPDATE idempotency_claims SET expires_at=NULL WHERE id=$1`, claimID)
	rejectSchemaRow(t, tx, "23514", `UPDATE idempotency_claims SET expires_at=accepted_at+interval '29 days' WHERE id=$1`, claimID)
	dbtest.MustExec(t, ctx, tx, `UPDATE idempotency_claims SET operation='task.child.invoke', expires_at=NULL WHERE id=$1`, claimID)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO session_records (id,environment_id,session_id,direction,sequence,data) VALUES ($1,$2,$3,'input',10,'{}')`, recordID, fixture.environmentID, sessionID)
	rejectSchemaRow(t, tx, "23503", `UPDATE session_records SET source_run_id=$2 WHERE id=$1`, recordID, uuid.NewV7())
	dbtest.MustExec(t, ctx, tx, `UPDATE session_records SET source_run_id=$2 WHERE id=$1`, recordID, work.runID)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO run_waits (id,environment_id,run_id,workspace_id,kind,session_id,after_input_sequence,
		 condition_state,condition_terminal_at,completed_actor_record_id,
		 expected_run_state_version,attempt_number,current_run_lease_id,resume_attach_id)
		SELECT $1,environment_id,id,workspace_id,'actor_input',session_id,0,'completed',now(),$2,state_version,1,$3,$4
		FROM runs WHERE id=$5
	`, waitID, recordID, work.leaseID, uuid.NewV7(), work.runID)
	rejectSchemaRow(t, tx, "23514", `UPDATE run_waits SET completed_actor_record_id=NULL WHERE id=$1`, waitID)
	rejectSchemaRow(t, tx, "23503", `UPDATE run_waits SET completed_actor_record_id=$2 WHERE id=$1`, waitID, uuid.NewV7())
	var outboxID int64
	if err := tx.QueryRow(ctx, `INSERT INTO telemetry_outbox(org_id,project_id,environment_id,stream_kind,source_kind,source_id,run_id,stream_name,content,size_bytes,observed_seq) VALUES ($1,$2,$3,'run_log','run',$4,$4,'stdout','\x00',1,1) RETURNING id`, fixture.orgID, fixture.projectID, fixture.environmentID, work.runID).Scan(&outboxID); err != nil {
		t.Fatal(err)
	}
	rejectSchemaRow(t, tx, "23514", `UPDATE telemetry_outbox SET run_id=NULL WHERE id=$1`, outboxID)
}

func assertCheckpointArtifactBoundaries(t *testing.T, tx pgx.Tx, fixture runLeaseClaimFixture, work runLeaseWork, privateVersionID, otherEnvironment uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	waitID, checkpointID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO run_waits (id,environment_id,run_id,workspace_id,kind,due_at,expected_run_state_version,attempt_number,current_run_lease_id,resume_attach_id)
		SELECT $1,environment_id,id,workspace_id,'timer',now()+interval '1 minute',state_version,1,$2,$3 FROM runs WHERE id=$4
	`, waitID, work.leaseID, uuid.NewV7(), work.runID)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO run_checkpoints (id,run_id,attempt_number,run_wait_id,source_run_lease_id,source_workspace_lease_id,workspace_id,base_workspace_version_id)
		SELECT $1,$2,1,$3,$4,id,workspace_id,base_version_id FROM workspace_leases WHERE owner_run_lease_id=$4
	`, checkpointID, work.runID, waitID, work.leaseID)
	artifacts := dbtest.InsertCheckpointArtifacts(t, ctx, tx, work.runID, "schema-checkpoint")
	rejectSchemaRow(t, tx, "23514", `UPDATE run_checkpoints SET runtime_config_artifact_id=$2 WHERE id=$1`, checkpointID, artifacts.RuntimeConfig)
	rejectSchemaRow(t, tx, "23514", `UPDATE run_checkpoints SET state='ready', private_workspace_version_id=$2, ready_at=now(), ready_request_fingerprint='ready', restore_manifest='{"version":0}' WHERE id=$1`, checkpointID, privateVersionID)
	params := MarkRunCheckpointReadyParams{
		ID: pgvalue.UUID(checkpointID), RunID: pgvalue.UUID(work.runID), AttemptNumber: 1,
		PrivateWorkspaceVersionID: pgvalue.UUID(privateVersionID), RuntimeConfigArtifactID: pgvalue.UUID(artifacts.RuntimeConfig),
		VMStateArtifactID: pgvalue.UUID(artifacts.VMState), MemoryArtifactID: pgvalue.UUID(artifacts.Memory), ScratchDiskArtifactID: pgvalue.UUID(artifacts.ScratchDisk),
		RestoreManifest: []byte(`{"version":0}`), ReadyRequestFingerprint: pgvalue.Text("ready"),
	}
	queries := New(tx)
	dbtest.MustExec(t, ctx, tx, `UPDATE run_checkpoints SET runtime_config_artifact_id=$2, vm_state_artifact_id=$3, memory_artifact_id=$4, scratch_disk_artifact_id=$5 WHERE id=$1`, checkpointID, artifacts.RuntimeConfig, artifacts.VMState, artifacts.Memory, artifacts.ScratchDisk)
	for _, column := range []string{"runtime_config_artifact_id", "vm_state_artifact_id", "memory_artifact_id", "scratch_disk_artifact_id"} {
		t.Run(column, func(t *testing.T) {
			rejectSchemaRow(t, tx, "23503", "UPDATE run_checkpoints SET "+column+"=$2 WHERE id=$1", checkpointID, uuid.NewV7())
		})
	}
	// Admission must check each artifact's kind and environment; the DB FKs intentionally
	// guarantee referential presence only. Restore the artifact between each attempt.
	for _, artifactID := range []uuid.UUID{artifacts.RuntimeConfig, artifacts.VMState, artifacts.Memory, artifacts.ScratchDisk} {
		for _, set := range []string{"kind='workspace_image'", "environment_id='" + otherEnvironment.String() + "'"} {
			dbtest.MustExec(t, ctx, tx, "SAVEPOINT artifact_authority")
			dbtest.MustExec(t, ctx, tx, "UPDATE artifacts SET "+set+" WHERE id=$1", artifactID)
			if _, err := queries.MarkRunCheckpointReady(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
				t.Errorf("checkpoint admission with %s: %v, want no rows", set, err)
			}
			dbtest.MustExec(t, ctx, tx, "ROLLBACK TO SAVEPOINT artifact_authority")
			dbtest.MustExec(t, ctx, tx, "RELEASE SAVEPOINT artifact_authority")
		}
	}
	if _, err := queries.MarkRunCheckpointReady(ctx, params); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"runtime_config_artifact_id", "vm_state_artifact_id", "memory_artifact_id", "scratch_disk_artifact_id"} {
		rejectSchemaRow(t, tx, "23514", "UPDATE run_checkpoints SET "+column+"=NULL WHERE id=$1", checkpointID)
	}
}

func TestSchemaRemovedStatesAndLeaseCreationBounds(t *testing.T) {
	ctx := t.Context()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "assigned", time.Now().Add(-time.Minute))
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	rejectSchemaRow(t, tx, "23514", `UPDATE run_leases SET expires_at=created_at, start_deadline_at=created_at WHERE id=$1`, work.leaseID)
	rejectSchemaRow(t, tx, "23514", `UPDATE run_leases SET state='starting', claimed_at=created_at-interval '1 microsecond' WHERE id=$1`, work.leaseID)
	rejectSchemaRow(t, tx, "23514", `UPDATE run_leases SET renewed_at=created_at-interval '1 microsecond', previous_expires_at=expires_at, expires_at=expires_at+interval '1 minute' WHERE id=$1`, work.leaseID)
	dbtest.MustExec(t, ctx, tx, `UPDATE run_leases SET state='starting', claimed_at=created_at WHERE id=$1`, work.leaseID)
	dbtest.MustExec(t, ctx, tx, `UPDATE run_leases SET state='running', started_at=claimed_at WHERE id=$1`, work.leaseID)
	rejectSchemaRow(t, tx, "23514", `UPDATE workspace_leases SET state='lost', terminal_at=now(), terminal_reason_code='worker_lost' WHERE owner_run_lease_id=$1`, work.leaseID)
	dbtest.MustExec(t, ctx, tx, `UPDATE workspace_leases SET state='expired', terminal_at=now(), terminal_reason_code='worker_lost' WHERE owner_run_lease_id=$1`, work.leaseID)
	var preservedReason string
	if err := tx.QueryRow(ctx, `SELECT terminal_reason_code FROM workspace_leases WHERE owner_run_lease_id=$1`, work.leaseID).Scan(&preservedReason); err != nil || preservedReason != "worker_lost" {
		t.Fatalf("loss reason = %q, %v", preservedReason, err)
	}
	tokenID, accessID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, tx, `INSERT INTO tokens(id,org_id,project_id,environment_id,expires_at,callback_secret_fingerprint) VALUES ($1,$2,$3,$4,now()+interval '1 hour',$5)`, tokenID, fixture.orgID, fixture.projectID, fixture.environmentID, dbtest.Hash("callback"))
	dbtest.MustExec(t, ctx, tx, `INSERT INTO public_access_tokens(id,token_id,token_hash,expires_at) VALUES ($1,$2,$3,now()+interval '1 hour')`, accessID, tokenID, dbtest.Hash("access"))
	rejectSchemaRow(t, tx, "23514", `UPDATE public_access_tokens SET state='revoked' WHERE id=$1`, accessID)
	queries := New(tx)
	used, err := queries.MarkPublicAccessTokenUsed(ctx, pgvalue.UUID(accessID))
	if err != nil || used.UsedCount != 1 {
		t.Fatalf("use = %+v, %v", used, err)
	}
	dbtest.MustExec(t, ctx, tx, `UPDATE public_access_tokens SET created_at=now()-interval '2 hours', expires_at=now()-interval '1 hour' WHERE id=$1`, accessID)
	expired, err := queries.ExpireDuePublicAccessTokens(ctx, 10)
	if err != nil || len(expired) != 1 || expired[0].State != PublicAccessTokenStateExpired || !expired[0].ExpiredAt.Valid {
		t.Fatalf("expiry = %+v, %v", expired, err)
	}
}

func TestSchemaCurrentTerminalStatesRequireTheirEventTime(t *testing.T) {
	ctx := t.Context()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	sessionID := fixture.convertToActor(t, ctx, work, `{"enabled":false}`)
	tokenID, accessID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, fixture.pool, `INSERT INTO tokens(id,org_id,project_id,environment_id,expires_at,callback_secret_fingerprint) VALUES ($1,$2,$3,$4,now()+interval '1 hour',$5)`, tokenID, fixture.orgID, fixture.projectID, fixture.environmentID, dbtest.Hash("terminal-callback"))
	dbtest.MustExec(t, ctx, fixture.pool, `INSERT INTO public_access_tokens(id,token_id,token_hash,expires_at) VALUES ($1,$2,$3,now()+interval '1 hour')`, accessID, tokenID, dbtest.Hash("terminal-access"))
	for _, tc := range []struct {
		table, state, column, other, extra string
		id                                 uuid.UUID
	}{
		{"sessions", "closed", "closed_at", "failed_at", "", sessionID},
		{"sessions", "failed", "failed_at", "closed_at", `, failure='{"code":"run_failed","message":"failed","details":{}}'`, sessionID},
		{"tokens", "completed", "completed_at", "expired_at", "", tokenID},
		{"tokens", "expired", "expired_at", "cancelled_at", "", tokenID},
		{"tokens", "cancelled", "cancelled_at", "completed_at", "", tokenID},
		{"public_access_tokens", "expired", "expired_at", "last_used_at", "", accessID},
	} {
		t.Run(tc.table+" "+tc.state, func(t *testing.T) {
			tx, err := fixture.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			transition := "UPDATE " + tc.table + " SET state='" + tc.state + "'" + tc.extra
			rejectSchemaRow(t, tx, "23514", transition+" WHERE id=$1", tc.id)
			// A different event cannot supply the missing terminal fact.
			rejectSchemaRow(t, tx, "23514", transition+", "+tc.other+"=now() WHERE id=$1", tc.id)
			dbtest.MustExec(t, ctx, tx, transition+", "+tc.column+"=now() WHERE id=$1", tc.id)
			rejectSchemaRow(t, tx, "23514", "UPDATE "+tc.table+" SET "+tc.column+"=NULL WHERE id=$1", tc.id)
			// Constraint failure recovery preserves a usable transaction and terminal row.
			dbtest.MustExec(t, ctx, tx, "UPDATE "+tc.table+" SET updated_at=updated_at WHERE id=$1", tc.id)
		})
	}
}
