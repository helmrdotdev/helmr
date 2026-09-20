package db

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func TestSchemaTokenCompletionRequiresReplayIdentityAndResult(t *testing.T) {
	ctx := t.Context()
	f := newRunLeaseClaimFixture(t, ctx)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	id := uuid.NewV7()
	dbtest.MustExec(t, ctx, tx, `INSERT INTO tokens(id,org_id,project_id,environment_id,expires_at,callback_secret_fingerprint) VALUES($1,$2,$3,$4,now()+interval '1 hour',$5)`, id, f.orgID, f.projectID, f.environmentID, dbtest.Hash("callback"))
	rejectSchemaRow(t, tx, "23514", `UPDATE tokens SET status='completed',completed_at=now(),result='null' WHERE id=$1`, id)
	rejectSchemaRow(t, tx, "23514", `UPDATE tokens SET status='completed',completed_at=now(),completion_fingerprint=$2 WHERE id=$1`, id, dbtest.Hash("completion"))
	q := New(tx)
	params := CompleteTokenParams{ID: pgvalue.UUID(id), OrgID: pgvalue.UUID(f.orgID), ProjectID: pgvalue.UUID(f.projectID), EnvironmentID: pgvalue.UUID(f.environmentID), CompletionFingerprint: dbtest.Hash("completion"), Result: []byte("null"), ControlOutboxID: pgvalue.UUID(uuid.NewV7())}
	first, err := q.CompleteToken(ctx, params)
	if err != nil || first.Status != TokenStatusCompleted || first.AlreadyCompleted || first.CompletionConflict || !bytes.Equal(first.Result, []byte("null")) {
		t.Fatalf("completion = %+v, %v", first, err)
	}
	replay, err := q.CompleteToken(ctx, params)
	if err != nil || !replay.AlreadyCompleted || replay.CompletionConflict || replay.ReconciliationEnqueued {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	params.CompletionFingerprint = dbtest.Hash("different")
	conflict, err := q.CompleteToken(ctx, params)
	if err != nil || conflict.AlreadyCompleted || !conflict.CompletionConflict || conflict.ReconciliationEnqueued {
		t.Fatalf("conflict = %+v, %v", conflict, err)
	}
	rejectSchemaRow(t, tx, "23514", `UPDATE tokens SET completion_fingerprint=NULL WHERE id=$1`, id)
	rejectSchemaRow(t, tx, "23514", `UPDATE tokens SET result=NULL WHERE id=$1`, id)
}

func TestSchemaRunMetadataAndSessionDuration(t *testing.T) {
	ctx := t.Context()
	f := newRunLeaseClaimFixture(t, ctx)
	work := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	session := f.convertToActor(t, ctx, work, `{"enabled":false}`)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, value := range []string{`null`, `[]`, `1`, `"text"`, `true`} {
		rejectSchemaRow(t, tx, "23514", `UPDATE runs SET metadata=$2::jsonb WHERE id=$1`, work.runID, value)
	}
	dbtest.MustExec(t, ctx, tx, `UPDATE runs SET metadata='{"nested":[null,1]}' WHERE id=$1`, work.runID)
	for _, value := range []int64{0, 4999, 86400001} {
		rejectSchemaRow(t, tx, "23514", `UPDATE sessions SET run_max_active_duration_ms=$2 WHERE id=$1`, session, value)
	}
	for _, value := range []int64{5000, 86400000} {
		dbtest.MustExec(t, ctx, tx, `UPDATE sessions SET run_max_active_duration_ms=$2 WHERE id=$1`, session, value)
	}
	// Payload and output remain arbitrary JSON values, independent of metadata shape.
	task := f.addWork(t, ctx, "assigned", time.Now().Add(-time.Minute))
	for _, value := range []string{`null`, `[]`, `1`, `"text"`, `true`} {
		dbtest.MustExec(t, ctx, tx, `UPDATE runs SET payload=$2::jsonb,status='succeeded',terminal_at=now(),output=$2::jsonb WHERE id=$1`, task.runID, value)
	}
}

func TestSchemaCanonicalContentAndRequestDigests(t *testing.T) {
	ctx := t.Context()
	f := newRunLeaseClaimFixture(t, ctx)
	work := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	substrateID := uuid.NewV7()
	dbtest.MustExec(t, ctx, tx, `INSERT INTO runtime_substrates(id,org_id,project_id,environment_id,deployment_definition_id,substrate_digest,substrate_format,substrate_contract,substrate_size_bytes) VALUES($1,$2,$3,$4,$5,$6,'squashfs','test',1)`, substrateID, f.orgID, f.projectID, f.environmentID, f.workspaceDefinitionID, dbtest.Digest("substrate"))
	for _, value := range []string{"", "digest", "sha256:abc", "sha256:" + strings.Repeat("A", 64), "sha512:" + strings.Repeat("a", 64)} {
		rejectSchemaRow(t, tx, "23514", `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,1,'application/octet-stream')`, f.orgID, value)
		rejectSchemaRow(t, tx, "23514", `UPDATE runtime_substrates SET substrate_digest=$2 WHERE id=$1`, substrateID, value)
		rejectSchemaRow(t, tx, "23514", `UPDATE run_leases SET terminal_request_fingerprint=$2 WHERE id=$1`, work.leaseID, value)
		rejectSchemaRow(t, tx, "23514", `UPDATE run_leases SET status='finalizing',started_at=claimed_at,finalization_operation_id=$2,finalization_kind='capture',finalization_started_at=now(),finalization_request_fingerprint=$3 WHERE id=$1`, work.leaseID, uuid.NewV7(), value)
	}
	dbtest.MustExec(t, ctx, tx, `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,1,'application/octet-stream')`, f.orgID, dbtest.Digest("content"))
	dbtest.MustExec(t, ctx, tx, `UPDATE run_leases SET terminal_request_fingerprint=$2 WHERE id=$1`, work.leaseID, dbtest.Digest("terminal"))
}

func TestCreationSelectsDefinitionKindAndDeclaredID(t *testing.T) {
	ctx := t.Context()
	f := newRunLeaseClaimFixture(t, ctx)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.environmentID, f.deploymentID)
	actorID := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO deployment_definitions(id,environment_id,deployment_id,kind,declared_id,manifest_version,manifest,manifest_digest) VALUES($1,$2,$3,'actor','selection-actor',0,'{}',$4)`, actorID, f.environmentID, f.deploymentID, dbtest.Hash("actor-manifest"))
	var sandboxName string
	if err := f.pool.QueryRow(ctx, `SELECT declared_id FROM deployment_definitions WHERE id=$1`, f.workspaceDefinitionID).Scan(&sandboxName); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	q := New(tx)
	workspaceParams := CreateWorkspaceFromCurrentDeploymentParams{ID: pgvalue.UUID(uuid.NewV7()), InitialVersionID: pgvalue.UUID(uuid.NewV7()), OrgID: pgvalue.UUID(f.orgID), ProjectID: pgvalue.UUID(f.projectID), EnvironmentID: pgvalue.UUID(f.environmentID), DeploymentDefinitionID: pgvalue.UUID(f.workspaceDefinitionID), SandboxDeclaredID: sandboxName}
	for _, tc := range []struct {
		definition uuid.UUID
		name       string
	}{{f.taskDefinitionID, "test-task"}, {f.workspaceDefinitionID, "wrong-name"}, {actorID, "selection-actor"}} {
		p := workspaceParams
		p.DeploymentDefinitionID = pgvalue.UUID(tc.definition)
		p.SandboxDeclaredID = tc.name
		if _, err := q.CreateWorkspaceFromCurrentDeployment(ctx, p); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("workspace selection = %v", err)
		}
	}
	workspace, err := q.CreateWorkspaceFromCurrentDeployment(ctx, workspaceParams)
	if err != nil {
		t.Fatal(err)
	}
	params := CreateActorParams{ID: pgvalue.UUID(uuid.NewV7()), OrgID: workspaceParams.OrgID, ProjectID: workspaceParams.ProjectID, EnvironmentID: workspaceParams.EnvironmentID, WorkspaceID: workspace.ID, DeploymentDefinitionID: pgvalue.UUID(actorID), ActorDeclaredID: "selection-actor", RunQueueName: "default", RunMaxActiveDurationMs: 5000, RunRetryPolicy: []byte(`{"enabled":false}`)}
	for _, tc := range []struct {
		definition uuid.UUID
		name       string
	}{{f.taskDefinitionID, "test-task"}, {f.workspaceDefinitionID, sandboxName}, {actorID, "wrong-name"}} {
		p := params
		p.DeploymentDefinitionID = pgvalue.UUID(tc.definition)
		p.ActorDeclaredID = tc.name
		if _, err := q.CreateActor(ctx, p); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("actor selection = %v", err)
		}
	}
	session, err := q.CreateActor(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if session.ActorDeclaredID != "selection-actor" || session.WorkspaceID != workspace.ID {
		t.Fatalf("session = %+v", session)
	}
	dbtest.MustExec(t, ctx, tx, `SET CONSTRAINTS ALL IMMEDIATE`)
}

func TestSchemaTurnFingerprintAndMessageEventSubject(t *testing.T) {
	ctx := t.Context()
	f := newRunLeaseClaimFixture(t, ctx)
	work := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	sessionID := f.convertToActor(t, ctx, work, `{"enabled":false}`)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	turnID, otherTurnID, messageID := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	for i, id := range []uuid.UUID{turnID, otherTurnID} {
		dbtest.MustExec(t, ctx, tx, `INSERT INTO session_turns(id,environment_id,session_id,sequence,data,status,run_id,attempt_number,run_generation)
   VALUES($1,$2,$3,$4,'null','running',$5,1,1)`, id, f.environmentID, sessionID, i+1, work.runID)
	}
	for _, invalid := range []string{"", strings.Repeat("a", 64), "sha256:abc", "sha256:" + strings.Repeat("A", 64)} {
		rejectSchemaRow(t, tx, "23514", `UPDATE session_turns SET terminal_request_fingerprint=$2 WHERE id=$1`, turnID, invalid)
	}
	dbtest.MustExec(t, ctx, tx, `UPDATE session_turns SET terminal_request_fingerprint=$2 WHERE id=$1`, turnID, dbtest.Digest("settlement"))
	dbtest.MustExec(t, ctx, tx, `INSERT INTO session_messages(id,environment_id,session_id,turn_id,run_id,attempt_number,run_generation,data,accepted_sequence)
  VALUES($1,$2,$3,$4,$5,1,1,'null',1)`, messageID, f.environmentID, sessionID, turnID, work.runID)
	const eventSQL = `INSERT INTO session_events(id,environment_id,session_id,workspace_id,turn_id,message_id,sequence,kind,data)
  SELECT $1,environment_id,id,workspace_id,$3,$4,1,'message.accepted','{}' FROM sessions WHERE id=$2`
	// Each subject exists within this Session, but only the message's own Turn is valid.
	rejectSchemaRow(t, tx, "23503", eventSQL, uuid.NewV7(), sessionID, otherTurnID, messageID)
	dbtest.MustExec(t, ctx, tx, eventSQL, uuid.NewV7(), sessionID, turnID, messageID)
}
