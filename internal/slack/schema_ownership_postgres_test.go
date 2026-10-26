package slack

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func requireSlackForeignKey(t *testing.T, err error) {
	t.Helper()
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "23503" {
		t.Fatalf("expected foreign key rejection, got %v", err)
	}
}

func slackPeerSession(t *testing.T, f statusFixture) uuid.UUID {
	t.Helper()
	peer := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO sessions(environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth,origin_user_id)
 SELECT environment_id,$2,agent_id,deployment_id,computer_id,$2,0,origin_user_id FROM sessions WHERE id=$1`, f.Session, peer)
	return peer
}

func TestSlackAnswerReceiptRequiresAskSession(t *testing.T) {
	f := newStatusFixture(t)
	_, ask := f.pendingQuestion(t)
	peer, source := slackPeerSession(t, f), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id) VALUES($1,$2,$3,$4)`, source, f.thread, f.Environment, peer)
	const insert = `INSERT INTO slack_requests(id,installation_id,request_key,request_digest,source_occurred_at,slack_user_id,user_id,thread_id,environment_id,session_id,thread_source_id,operation,payload,status,expires_at,ask_id,finished_at)
 VALUES($1::uuid,$2,$1::uuid::text,decode(repeat('11',32),'hex'),clock_timestamp(),'human',$3,$4,$5,$6,$7,'answer',decode('01','hex'),'accepted',clock_timestamp()+interval '1 hour',$8,clock_timestamp())`
	_, err := f.Pool.Exec(t.Context(), insert, uuid.NewV7(), f.installation, f.User, f.thread, f.Environment, peer, source, ask)
	requireSlackForeignKey(t, err)
	receipt := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, insert, receipt, f.installation, f.User, f.thread, f.Environment, f.Session, f.participant, ask)
	var nullTurn bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT turn_id IS NULL AND ask_id=$2 FROM slack_requests WHERE id=$1`, receipt, ask).Scan(&nullTurn); err != nil || !nullTurn {
		t.Fatalf("answer receipt requires a NULL Turn: %v %v", nullTurn, err)
	}
	_, err = f.Pool.Exec(t.Context(), `DELETE FROM turn_asks WHERE environment_id=$1 AND id=$2`, f.Environment, ask)
	requireSlackForeignKey(t, err)
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM slack_requests WHERE id=$1; DELETE FROM turn_asks WHERE environment_id=$2 AND id=$3`, pgx.QueryExecModeSimpleProtocol, receipt, f.Environment, ask)
}

func TestSlackWorkItemRequiresPostThreadAndEnvironment(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	f.outputWriter(t)
	child, turn := activityChild(t, f, false)
	post := f.post(t, 1, "group", "lifecycle", nil)
	// The child owns the work item while the front Session authors the post.
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_work_items(thread_id,environment_id,session_id,turn_id,post_id) VALUES($1,$2,$3,$4,$5)`, f.thread, f.Environment, child.Session, turn.TurnID, post)
	var differentSession bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT w.session_id<>p.session_id FROM slack_work_items w JOIN slack_posts p ON p.id=w.post_id WHERE w.turn_id=$1`, turn.TurnID).Scan(&differentSession); err != nil || !differentSession {
		t.Fatalf("child work grouping lost: %v %v", differentSession, err)
	}
	peer, otherThread, source := slackPeerSession(t, f), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_threads(id,environment_id,front_session_id,slack_channel_binding_id,org_id,team_id,slack_channel_id,thread_ts)
 SELECT $2,environment_id,$3,slack_channel_binding_id,org_id,team_id,slack_channel_id,'123.999' FROM slack_threads WHERE id=$1;
 INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id) VALUES($4,$2,$5,$3)`, pgx.QueryExecModeSimpleProtocol, f.thread, otherThread, peer, source, f.Environment)
	other := f
	other.Session, other.thread, other.participant = peer, otherThread, source
	otherPost := other.post(t, 1, "other-group", "lifecycle", nil)
	_, err := f.Pool.Exec(t.Context(), `UPDATE slack_work_items SET thread_id=$2 WHERE turn_id=$1`, turn.TurnID, otherThread)
	requireSlackForeignKey(t, err)
	_, err = f.Pool.Exec(t.Context(), `UPDATE slack_work_items SET post_id=$2 WHERE turn_id=$1`, turn.TurnID, otherPost)
	requireSlackForeignKey(t, err)
	otherEnvironment := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO environments(id,org_id,project_id,slug,name,color_hex,max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at)
 SELECT $2,org_id,project_id,'other','Other',color_hex,max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at FROM environments WHERE id=$1;
 INSERT INTO deployments(environment_id,id,bundle_digest) SELECT $2,id,bundle_digest FROM deployments WHERE environment_id=$1;
 INSERT INTO image_specs(environment_id,id,spec_digest,spec,seed) SELECT $2,id,spec_digest,spec,seed FROM image_specs WHERE environment_id=$1;
 INSERT INTO templates(environment_id,id,key) SELECT $2,id,key FROM templates WHERE environment_id=$1;
 INSERT INTO template_definitions(environment_id,template_id,deployment_id,image_spec_id,cpu_millis,memory_bytes,network_policy,prepare_network_policy) SELECT $2,template_id,deployment_id,image_spec_id,cpu_millis,memory_bytes,network_policy,prepare_network_policy FROM template_definitions WHERE environment_id=$1;
 INSERT INTO agents(environment_id,id,key) SELECT $2,id,key FROM agents WHERE environment_id=$1;
 INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,runtime_mcp_policy) SELECT $2,agent_id,deployment_id,template_id,has_init,runtime_mcp_policy FROM agent_definitions WHERE environment_id=$1;
 INSERT INTO computers(environment_id,id,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,initial_root_digest,initial_payload_retired_at,deleted_at)
 SELECT $2,id,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,initial_root_digest,clock_timestamp(),clock_timestamp() FROM computers WHERE environment_id=$1;
 INSERT INTO sessions(environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth,origin_user_id)
 SELECT $2,id,agent_id,deployment_id,computer_id,id,0,origin_user_id FROM sessions WHERE environment_id=$1 AND id=$3;
 INSERT INTO turns(environment_id,id,session_id,computer_id,seq,caller_kind,caller_id,admission_method,target_id,request_digest,input)
 SELECT $2,id,session_id,computer_id,seq,caller_kind,caller_id,admission_method,target_id,request_digest,input FROM turns WHERE environment_id=$1 AND id=$4`, pgx.QueryExecModeSimpleProtocol, f.Environment, otherEnvironment, child.Session, turn.TurnID)
	_, err = f.Pool.Exec(t.Context(), `UPDATE slack_work_items SET environment_id=$2 WHERE turn_id=$1`, turn.TurnID, otherEnvironment)
	requireSlackForeignKey(t, err)
	_, err = f.Pool.Exec(t.Context(), `DELETE FROM slack_posts WHERE id=$1`, post)
	requireSlackForeignKey(t, err)
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM slack_work_items WHERE turn_id=$1; DELETE FROM slack_posts WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, turn.TurnID, post)
}

func TestSlackSocketRequiresInstallationRegistration(t *testing.T) {
	f := newStatusFixture(t)
	otherRegistration, otherInstallation := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_app_registrations(id,org_id,transport,created_by_user_id)
 SELECT $2,org_id,'socket',created_by_user_id FROM slack_app_registrations WHERE id=$1;
 INSERT INTO slack_installations(id,app_registration_id,org_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id)
 SELECT $4,$2,org_id,'other-app','other-team','other-bot',credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id FROM slack_installations WHERE id=$3`, pgx.QueryExecModeSimpleProtocol, f.registration, otherRegistration, f.installation, otherInstallation)
	_, err := f.Pool.Exec(t.Context(), `INSERT INTO slack_socket_connections(app_registration_id,installation_id,credential_revision,status) VALUES($1,$2,1,'connecting')`, f.registration, otherInstallation)
	requireSlackForeignKey(t, err)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_socket_connections(app_registration_id,installation_id,credential_revision,status) VALUES($1,$2,1,'connecting')`, otherRegistration, otherInstallation)
	_, err = f.Pool.Exec(t.Context(), `UPDATE slack_socket_connections SET installation_id=$2 WHERE app_registration_id=$1`, otherRegistration, f.installation)
	requireSlackForeignKey(t, err)
	_, err = f.Pool.Exec(t.Context(), `DELETE FROM slack_installations WHERE id=$1`, otherInstallation)
	requireSlackForeignKey(t, err)
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM slack_socket_connections WHERE app_registration_id=$1; DELETE FROM slack_installations WHERE id=$2; DELETE FROM slack_app_registrations WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, otherRegistration, otherInstallation)
}

func TestSlackConnectionRequiresSharedOrganizationBeforeInstallation(t *testing.T) {
	f := newStatusFixture(t)
	var org uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id FROM environments WHERE id=$1`, f.Environment).Scan(&org); err != nil {
		t.Fatal(err)
	}
	otherOrg, otherRegistration := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO organizations(id,name,slug) VALUES($1,'Other','other');
 INSERT INTO slack_app_registrations(id,org_id,transport,created_by_user_id) VALUES($2,$1,'http',$3)`, pgx.QueryExecModeSimpleProtocol, otherOrg, otherRegistration, f.User)
	const insert = `INSERT INTO slack_connections(id,org_id,environment_id,agent_id,slack_app_registration_id,created_by_user_id,revoked_at) VALUES($1,$2,$3,$4,$5,$6,clock_timestamp())`
	for _, witness := range []uuid.UUID{org, otherOrg} {
		_, err := f.Pool.Exec(t.Context(), insert, uuid.NewV7(), witness, f.Environment, f.Agent, otherRegistration, f.User)
		requireSlackForeignKey(t, err)
	}
	registration, connection := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_app_registrations(id,org_id,transport,created_by_user_id) VALUES($1,$2,'http',$3)`, registration, org, f.User)
	dbtest.MustExec(t, t.Context(), f.Pool, insert, connection, org, f.Environment, f.Agent, registration, f.User)
	_, err := f.Pool.Exec(t.Context(), `UPDATE slack_connections SET slack_app_registration_id=$2 WHERE id=$1`, connection, otherRegistration)
	requireSlackForeignKey(t, err)
	_, err = f.Pool.Exec(t.Context(), `UPDATE slack_connections SET org_id=$2 WHERE id=$1`, connection, otherOrg)
	requireSlackForeignKey(t, err)
	_, err = f.Pool.Exec(t.Context(), `DELETE FROM slack_app_registrations WHERE id=$1`, registration)
	requireSlackForeignKey(t, err)
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM slack_connections WHERE id=$1; DELETE FROM slack_app_registrations WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, connection, registration)
}
