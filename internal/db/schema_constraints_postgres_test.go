package db

import (
	"encoding/json"
	"errors"
	"strings"
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
		 status, effective_from, next_fire_at)
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
		{"session failed", `UPDATE sessions SET status='failed', failed_at=now(), failure=$2 WHERE id=$1`, "run_failed", sessionID},
		{"schedule errored", `UPDATE schedules SET status='errored', last_failure=$2 WHERE id=$1`, "invalid_schedule", scheduleID},
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
	dbtest.MustExec(t, ctx, tx, `UPDATE schedules SET status='errored', last_failure='{"code":"invalid_schedule","message":"failed","details":{}}' WHERE id=$1`, scheduleID)
	dbtest.MustExec(t, ctx, tx, `UPDATE schedules SET status='active' WHERE id=$1`, scheduleID)
	rejectSchemaRow(t, tx, "23514", `UPDATE schedules SET last_failure='{"code":null,"message":"failed","details":{}}' WHERE id=$1`, scheduleID)
	dbtest.MustExec(t, ctx, tx, `UPDATE schedules SET status='archived', deployment_id=NULL, deployment_definition_id=NULL, next_fire_at=NULL WHERE id=$1`, scheduleID)
	dbtest.MustExec(t, ctx, tx, `UPDATE schedules SET last_failure=NULL WHERE id=$1`, scheduleID)
}

func TestSchemaComputerDiskVersionAndCheckpointAuthority(t *testing.T) {
	ctx := t.Context()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	versionID := uuid.NewV7()
	digest := dbtest.Digest("schema-computer-version")
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO computer_disk_versions (id,environment_id,computer_id,parent_version_id,root_pack_digest,source_computer_instance_id,writer_generation)
		SELECT $1,i.environment_id,i.computer_id,r.base_computer_disk_version_id,$2,i.id,i.writer_generation
		FROM run_leases l JOIN runs r ON r.id=l.run_id JOIN computer_instances i ON i.id=l.computer_instance_id WHERE l.id=$3
	`, versionID, digest, work.leaseID)
	for _, set := range []string{
		"parent_version_id=NULL",
		"source_computer_instance_id=NULL",
	} {
		t.Run(set, func(t *testing.T) {
			rejectSchemaRow(t, tx, "23514", "UPDATE computer_disk_versions SET "+set+" WHERE id=$1", versionID)
		})
	}
	rejectSchemaRow(t, tx, "23503", `UPDATE computer_disk_versions SET writer_generation=writer_generation+1 WHERE id=$1`, versionID)

	dbtest.MustExec(t, ctx, tx, "SAVEPOINT checkpoint_boundaries")
	assertCheckpointArtifactBoundaries(t, tx, work, versionID)
	dbtest.MustExec(t, ctx, tx, "ROLLBACK TO SAVEPOINT checkpoint_boundaries")

	var instanceID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT computer_instance_id FROM run_leases WHERE id=$1`, work.leaseID).Scan(&instanceID); err != nil {
		t.Fatal(err)
	}
	rejectSchemaRow(t, tx, "23514", `UPDATE computer_instances SET finalization_action='discard' WHERE id=$1`, instanceID)
	rejectSchemaRow(t, tx, "23514", `UPDATE computer_instances SET finalization_reason_code='closed' WHERE id=$1`, instanceID)
	dbtest.MustExec(t, ctx, tx, `SAVEPOINT private_version`)
	dbtest.MustExec(t, ctx, tx, `UPDATE computer_disk_versions SET status='discarded', discarded_at=now() WHERE id=$1`, versionID)
	// Restore the private row before checking its alternate publication path.
	dbtest.MustExec(t, ctx, tx, `ROLLBACK TO SAVEPOINT private_version`)
	dbtest.MustExec(t, ctx, tx, `UPDATE computer_disk_versions SET status='committed', discarded_at=NULL, published_at=now() WHERE id=$1`, versionID)
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
	claimID, turnID, waitID := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, tx, `INSERT INTO idempotency_claims (id,environment_id,operation,slot_hash,request_fingerprint,accepted_at,receipt_expires_at) VALUES ($1,$2,'run.create',$3,$4,now(),now()+interval '30 days')`, claimID, fixture.environmentID, dbtest.Hash("slot"), dbtest.Hash("request"))
	rejectSchemaRow(t, tx, "23514", `UPDATE idempotency_claims SET receipt_pruned_at=now() WHERE id=$1`, claimID)
	rejectSchemaRow(t, tx, "23514", `UPDATE idempotency_claims SET status='completed',completed_at=now() WHERE id=$1`, claimID)
	dbtest.MustExec(t, ctx, tx, `UPDATE idempotency_claims SET status='completed',completed_at=now(),receipt='{}' WHERE id=$1`, claimID)
	rejectSchemaRow(t, tx, "23514", `UPDATE idempotency_claims SET receipt=NULL,receipt_pruned_at=now() WHERE id=$1`, claimID)
	dbtest.MustExec(t, ctx, tx, `UPDATE idempotency_claims SET receipt=NULL,receipt_expires_at=now()-interval '1 day',receipt_pruned_at=now() WHERE id=$1`, claimID)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO session_turns (id,environment_id,session_id,sequence,data) VALUES ($1,$2,$3,10,'{}')`, turnID, fixture.environmentID, sessionID)
	rejectSchemaRow(t, tx, "23503", `UPDATE session_turns SET source_run_id=$2 WHERE id=$1`, turnID, uuid.NewV7())
	dbtest.MustExec(t, ctx, tx, `UPDATE session_turns SET source_run_id=$2 WHERE id=$1`, turnID, work.runID)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO run_waits (id,environment_id,run_id,computer_id,kind,session_id,after_input_sequence,
		 condition_status,condition_terminal_at,completed_turn_id,
		 expected_run_revision,attempt_number,current_run_lease_id)
		SELECT $1,environment_id,id,computer_id,'actor_input',session_id,0,'completed',now(),$2,revision,1,$3
		FROM runs WHERE id=$4
	`, waitID, turnID, work.leaseID, work.runID)
	rejectSchemaRow(t, tx, "23514", `UPDATE run_waits SET completed_turn_id=NULL WHERE id=$1`, waitID)
	rejectSchemaRow(t, tx, "23503", `UPDATE run_waits SET completed_turn_id=$2 WHERE id=$1`, waitID, uuid.NewV7())
	var outboxID int64
	if err := tx.QueryRow(ctx, `INSERT INTO telemetry_outbox(org_id,project_id,environment_id,stream_kind,source_kind,source_id,run_id,run_lease_id,attempt_number,stream_name,content,size_bytes,observed_seq) VALUES ($1,$2,$3,'run_log','run',$4,$4,$5,1,'stdout','\x00',1,1) RETURNING id`, fixture.orgID, fixture.projectID, fixture.environmentID, work.runID, work.leaseID).Scan(&outboxID); err != nil {
		t.Fatal(err)
	}
	for _, assignment := range []string{"run_id=NULL", "run_lease_id=NULL", "attempt_number=NULL", "attempt_number=0", "attempt_number=-1"} {
		rejectSchemaRow(t, tx, "23514", "UPDATE telemetry_outbox SET "+assignment+" WHERE id=$1", outboxID)
	}
}

func assertCheckpointArtifactBoundaries(t *testing.T, tx pgx.Tx, work runLeaseWork, privateVersionID uuid.UUID) {
	ctx := t.Context()
	checkpointID := uuid.NewV7()
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO computer_checkpoints (id,environment_id,computer_id,computer_spec_id,
		 source_computer_instance_id,writer_generation,membership_revision,base_computer_disk_version_id)
		SELECT $1,i.environment_id,i.computer_id,i.computer_spec_id,i.id,i.writer_generation,i.membership_revision,r.base_computer_disk_version_id
		FROM run_leases l JOIN runs r ON r.id=l.run_id JOIN computer_instances i ON i.id=l.computer_instance_id WHERE l.id=$2
	`, checkpointID, work.leaseID)
	artifacts := dbtest.InsertCheckpointArtifacts(t, ctx, tx, work.runID, "schema-checkpoint")
	rejectSchemaRow(t, tx, "23514", `UPDATE computer_checkpoints SET vm_config_artifact_id=$2 WHERE id=$1`, checkpointID, artifacts.RuntimeConfig)
	rejectSchemaRow(t, tx, "23514", `UPDATE computer_checkpoints SET status='ready', private_computer_disk_version_id=$2, ready_at=now(), ready_request_fingerprint='sha256:b24d6d33736ecd5604a4b17bc9c6481039fac362bb7df044ef1c10a2bfd21db6', manifest='{"version":0}' WHERE id=$1`, checkpointID, privateVersionID)
	dbtest.MustExec(t, ctx, tx, `UPDATE computer_checkpoints SET vm_config_artifact_id=$2, vm_state_artifact_id=$3, memory_artifact_id=$4, scratch_disk_artifact_id=$5 WHERE id=$1`, checkpointID, artifacts.RuntimeConfig, artifacts.VMState, artifacts.Memory, artifacts.ScratchDisk)
	for _, column := range []string{"vm_config_artifact_id", "vm_state_artifact_id", "memory_artifact_id", "scratch_disk_artifact_id"} {
		t.Run(column, func(t *testing.T) {
			rejectSchemaRow(t, tx, "23503", "UPDATE computer_checkpoints SET "+column+"=$2 WHERE id=$1", checkpointID, uuid.NewV7())
		})
	}
	dbtest.MustExec(t, ctx, tx, `UPDATE computer_checkpoints SET status='ready',private_computer_disk_version_id=$2,ready_at=now(),manifest='{"version":1}',ready_request_fingerprint=$3 WHERE id=$1`, checkpointID, privateVersionID, dbtest.Digest("checkpoint-ready"))
	rejectSchemaRow(t, tx, "23001", `DELETE FROM cas_objects WHERE (org_id,digest) IN (SELECT org_id,digest FROM artifacts WHERE id=$1)`, artifacts.RuntimeConfig)
	unattached := dbtest.InsertCheckpointArtifacts(t, ctx, tx, work.runID, "unattached-checkpoint")
	dbtest.MustExec(t, ctx, tx, `DELETE FROM cas_objects WHERE (org_id,digest) IN (SELECT org_id,digest FROM artifacts WHERE id=$1)`, unattached.RuntimeConfig)
	var remaining int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM artifacts WHERE id=$1`, unattached.RuntimeConfig).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("unreferenced artifact remaining=%d err=%v", remaining, err)
	}
	rejectSchemaRow(t, tx, "23514", "UPDATE computer_checkpoints SET manifest=NULL WHERE id=$1", checkpointID)
	rejectSchemaRow(t, tx, "23514", "UPDATE computer_checkpoints SET manifest='{}' WHERE id=$1", checkpointID)
	for _, column := range []string{"vm_config_artifact_id", "vm_state_artifact_id", "memory_artifact_id", "scratch_disk_artifact_id"} {
		rejectSchemaRow(t, tx, "23514", "UPDATE computer_checkpoints SET "+column+"=NULL WHERE id=$1", checkpointID)
	}
	for _, value := range []string{"fingerprint", "sha256:abc", "sha256:" + strings.Repeat("A", 64)} {
		rejectSchemaRow(t, tx, "23514", `UPDATE computer_checkpoints SET ready_request_fingerprint=$2 WHERE id=$1`, checkpointID, value)
		rejectSchemaRow(t, tx, "23514", `UPDATE computer_checkpoints SET status='invalid',invalidated_at=now(),invalidation_reason_code='checkpoint_failed',failed_request_fingerprint=$2 WHERE id=$1`, checkpointID, value)
	}
	dbtest.MustExec(t, ctx, tx, `UPDATE computer_checkpoints SET status='invalid',invalidated_at=now(),invalidation_reason_code='checkpoint_failed',failed_request_fingerprint=$2 WHERE id=$1`, checkpointID, dbtest.Digest("checkpoint-failure"))
}

func TestSchemaLeaseCreationBoundsAndExpiry(t *testing.T) {
	ctx := t.Context()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "assigned", time.Now().Add(-time.Minute))
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	rejectSchemaRow(t, tx, "23514", `UPDATE run_leases SET expires_at=created_at, start_deadline_at=created_at WHERE id=$1`, work.leaseID)
	rejectSchemaRow(t, tx, "23514", `UPDATE run_leases SET status='starting', claimed_at=created_at-interval '1 microsecond' WHERE id=$1`, work.leaseID)
	rejectSchemaRow(t, tx, "23514", `UPDATE run_leases SET renewed_at=created_at-interval '1 microsecond', previous_expires_at=expires_at, expires_at=expires_at+interval '1 minute' WHERE id=$1`, work.leaseID)
	dbtest.MustExec(t, ctx, tx, `UPDATE run_leases SET status='starting', claimed_at=created_at WHERE id=$1`, work.leaseID)
	dbtest.MustExec(t, ctx, tx, `UPDATE run_leases SET status='running', started_at=claimed_at WHERE id=$1`, work.leaseID)
	tokenID, accessID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, tx, `INSERT INTO tokens(id,org_id,project_id,environment_id,expires_at,callback_secret_fingerprint) VALUES ($1,$2,$3,$4,now()+interval '1 hour',$5)`, tokenID, fixture.orgID, fixture.projectID, fixture.environmentID, dbtest.Hash("callback"))
	dbtest.MustExec(t, ctx, tx, `INSERT INTO public_access_tokens(id,token_id,token_hash,expires_at) VALUES ($1,$2,$3,now()+interval '1 hour')`, accessID, tokenID, dbtest.Hash("access"))
	queries := New(tx)
	used, err := queries.MarkPublicAccessTokenUsed(ctx, pgvalue.UUID(accessID))
	if err != nil || used.UsedCount != 1 {
		t.Fatalf("use = %+v, %v", used, err)
	}
	dbtest.MustExec(t, ctx, tx, `UPDATE public_access_tokens SET created_at=now()-interval '2 hours', expires_at=now()-interval '1 hour' WHERE id=$1`, accessID)
	expired, err := queries.ExpireDuePublicAccessTokens(ctx, 10)
	if err != nil || len(expired) != 1 || expired[0].Status != PublicAccessTokenStatusExpired || !expired[0].ExpiredAt.Valid {
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
		{"tokens", "completed", "completed_at", "expired_at", `, completion_fingerprint=decode(repeat('ab',32),'hex'), result='null'::jsonb`, tokenID},
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
			transition := "UPDATE " + tc.table + " SET status='" + tc.state + "'" + tc.extra
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

func TestSchemaDiagnosticCodesAreStructural(t *testing.T) {
	ctx := t.Context()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	sessionID := fixture.convertToActor(t, ctx, work, `{"enabled":false}`)
	scheduleID := uuid.NewV7()
	dbtest.MustExec(t, ctx, fixture.pool, `INSERT INTO schedules (id,environment_id,task_declared_id,deployment_definition_id,deployment_id,cron_pattern,timezone,status,effective_from,next_fire_at)
 SELECT $1,environment_id,declared_id,id,deployment_id,'* * * * *','UTC','active',now(),now() FROM deployment_definitions WHERE id=$2`, scheduleID, fixture.taskDefinitionID)
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, tc := range []struct {
		name, statement string
		id              uuid.UUID
	}{
		{"schedule", `UPDATE schedules SET status='errored',last_failure=$2 WHERE id=$1`, scheduleID},
		{"session", `UPDATE sessions SET status='failed',failed_at=now(),failure=$2 WHERE id=$1`, sessionID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, code := range []string{"x", strings.Repeat("x", 128), "future_diagnostic"} {
				payload, _ := json.Marshal(map[string]any{"code": code, "message": "diagnosis", "details": map[string]any{}})
				dbtest.MustExec(t, ctx, tx, tc.statement, tc.id, payload)
			}
			for _, code := range []any{nil, 42, true, "", " ", "Uppercase", "bad-code", "é", "x\n", strings.Repeat("x", 129)} {
				payload, _ := json.Marshal(map[string]any{"code": code, "message": "diagnosis", "details": map[string]any{}})
				rejectSchemaRow(t, tx, "23514", tc.statement, tc.id, payload)
			}
		})
	}
	rejectSchemaRow(t, tx, "23514", `UPDATE sessions SET status='open',failed_at=NULL WHERE id=$1`, sessionID)
	dbtest.MustExec(t, ctx, tx, `UPDATE sessions SET status='open',failed_at=NULL,failure=NULL WHERE id=$1`, sessionID)
	dbtest.MustExec(t, ctx, tx, `UPDATE sessions SET status='failed',failed_at=now(),failure='{"code":"future_diagnostic","message":"diagnosis","details":{}}' WHERE id=$1`, sessionID)
	dbtest.MustExec(t, ctx, tx, `UPDATE schedules SET status='active' WHERE id=$1`, scheduleID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`SELECT last_failure->>'code' FROM schedules WHERE id=$1`, `SELECT failure->>'code' FROM sessions WHERE id=$1`} {
		id := scheduleID
		if strings.Contains(q, "FROM sessions") {
			id = sessionID
		}
		var code string
		if err := fixture.pool.QueryRow(ctx, q, id).Scan(&code); err != nil {
			t.Fatal(err)
		}
		if code != "future_diagnostic" {
			t.Fatalf("stored code=%q", code)
		}
	}
}
