package deployment

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func (f deploymentFinalizePostgresFixture) principal() auth.Principal {
	return f.request.principal
}
func (f deploymentFinalizePostgresFixture) scope() auth.Scope {
	p := f.principal()
	return auth.Scope{OrgID: p.OrgID, ProjectID: p.ProjectID, EnvironmentID: p.EnvironmentID}
}

func TestPromoteCreatesHalfOpenIntervalsAndRecomputesRefresh(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	every := int64(1101)
	f.request.bundle.bundle.Program.Metadata.Definitions[2].Template.Refresh = &definition.ComputerRefresh{EveryMs: every}
	first, err := register(t.Context(), f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.principal(), f.scope(), first.ID, nil); err != nil {
		t.Fatal(err)
	}
	var previous uuid.UUID
	var activeFrom time.Time
	var nextRefresh time.Time
	var cadence int64
	if err = f.pool.QueryRow(t.Context(), `SELECT id,active_from FROM trigger_activations WHERE active_until IS NULL LIMIT 1`).Scan(&previous, &activeFrom); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT refresh_every_ms,next_refresh_at FROM image_specs`).Scan(&cadence, &nextRefresh); err != nil || cadence != every {
		t.Fatalf("refresh=%d: %v", cadence, err)
	}
	if _, err = Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.principal(), f.scope(), first.ID, nil); err != nil {
		t.Fatal(err)
	}
	var until, started time.Time
	var current uuid.UUID
	var preserved time.Time
	if err = f.pool.QueryRow(t.Context(), `SELECT active_until FROM trigger_activations WHERE id=$1`, previous).Scan(&until); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT id,active_from FROM trigger_activations WHERE active_until IS NULL LIMIT 1`).Scan(&current, &started); err != nil {
		t.Fatal(err)
	}
	if current == previous || !until.Equal(started) || until.Before(activeFrom) {
		t.Fatal("re-promotion rewrote interval or left cutover gap")
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT next_refresh_at FROM image_specs`).Scan(&preserved); err != nil || !preserved.Equal(nextRefresh) {
		t.Fatalf("unchanged cadence reset: %v", err)
	}
	// A new Program with no refresh stops background work for its older inputs.
	f.request.idempotencyKey = "second"
	f.request.bundle.root.Digest = artifacttest.Digest("second bundle")
	f.request.bundle.bundle.Program.Artifact.Digest = artifacttest.Digest("second program")
	f.request.bundle.bundle.Program.Metadata.Definitions[2].Template.Refresh = nil
	f.request.bundle.bundle.Program.Metadata.Definitions[0].Agent.Triggers = map[string]definition.CronTrigger{}
	second, err := register(t.Context(), f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.principal(), f.scope(), second.ID, nil); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM image_specs WHERE next_refresh_at IS NOT NULL`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("abandoned refresh %d: %v", count, err)
	}
	// Re-promoting without refresh must not rewrite inactive historical specs.
	var beforeCTID, afterCTID string
	if err = f.pool.QueryRow(t.Context(), `SELECT s.ctid::text FROM image_specs s JOIN template_definitions d ON (d.environment_id,d.image_spec_id)=(s.environment_id,s.id) WHERE d.deployment_id=$1`, first.ID).Scan(&beforeCTID); err != nil {
		t.Fatal(err)
	}
	if _, err = Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.principal(), f.scope(), second.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT s.ctid::text FROM image_specs s JOIN template_definitions d ON (d.environment_id,d.image_spec_id)=(s.environment_id,s.id) WHERE d.deployment_id=$1`, first.ID).Scan(&afterCTID); err != nil || afterCTID != beforeCTID {
		t.Fatalf("historical spec rewritten %s -> %s: %v", beforeCTID, afterCTID, err)
	}
	// Old definitions remain executable pins for existing Sessions.
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_definitions WHERE deployment_id=$1`, first.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("old definitions lost: %d %v", count, err)
	}
}

func TestTriggerCutoverOwnsExactBoundary(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	f.request.bundle.bundle.Program.Metadata.Definitions[0].Agent.Triggers = map[string]definition.CronTrigger{"minute": {Cron: "* * * * *", Timezone: "UTC", Input: []byte(`[]`)}}
	r, err := register(t.Context(), f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	cutover := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	for range 2 {
		if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error { return reconcileTriggers(t.Context(), tx, r.EnvironmentID, r.ID, cutover, nil) }); err != nil {
			t.Fatal(err)
		}
	}
	var next time.Time
	if err := f.pool.QueryRow(t.Context(), `SELECT next_fire_at FROM trigger_activations WHERE active_until IS NULL AND trigger_key='minute'`).Scan(&next); err != nil || !next.Equal(cutover) {
		t.Fatalf("boundary next=%s: %v", next, err)
	}
	var bad int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM trigger_activations WHERE active_until IS NOT NULL AND active_until<>$1`, cutover).Scan(&bad); err != nil || bad != 0 {
		t.Fatalf("bad cutover %d: %v", bad, err)
	}
}

func TestPromoteRollsBackInvalidTrigger(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	r, err := register(t.Context(), f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(t.Context(), `INSERT INTO agent_triggers(environment_id,agent_id,deployment_id,key,cron,timezone,input) SELECT environment_id,agent_id,deployment_id,'bad','invalid','UTC','null' FROM agent_definitions WHERE deployment_id=$1 LIMIT 1`, pgx.QueryExecModeSimpleProtocol, r.ID); err != nil {
		t.Fatal(err)
	}
	_, err = Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.principal(), f.scope(), r.ID, nil)
	var input InputError
	if !errors.As(err, &input) {
		t.Fatalf("invalid trigger should be actionable input: %v", err)
	}
	var current *uuid.UUID
	if err = f.pool.QueryRow(t.Context(), `SELECT current_deployment_id FROM environments WHERE id=$1`, r.EnvironmentID).Scan(&current); err != nil || current != nil {
		t.Fatalf("partial promotion %v: %v", current, err)
	}
}

func TestDeploymentReadsUseNewDefinitionsAndScope(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	r, err := register(t.Context(), f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GetCurrent(t.Context(), f.pool, f.principal(), f.scope()); !errors.Is(err, ErrNoCurrentDeployment) {
		t.Fatal(err)
	}
	if _, err = Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.principal(), f.scope(), r.ID, nil); err != nil {
		t.Fatal(err)
	}
	current, err := GetCurrent(t.Context(), f.pool, f.principal(), f.scope())
	if err != nil || current.ID != r.ID {
		t.Fatalf("current: %v %v", current, err)
	}
	page, err := ListDefinitions(t.Context(), f.pool, f.principal(), f.scope(), definition.KindAgent, nil, 1, nil)
	if err != nil || len(page.Items) != 1 || !page.HasMore || page.Items[0].ID != "build" {
		t.Fatalf("page: %+v %v", page, err)
	}
	id, name, err := GetDefinition(t.Context(), f.pool, f.principal(), f.scope(), definition.KindTemplate, &r.ID, "repo")
	if err != nil || id != r.ID || name.ID != "repo" {
		t.Fatalf("computer: %s %+v %v", id, name, err)
	}
	wrong := f.scope()
	wrong.ProjectID = uuid.NewV7().String()
	if _, err := Get(t.Context(), f.pool, f.principal(), wrong, r.ID); err == nil {
		t.Fatal("wrong scope read allowed")
	}
}

func TestPromotePreservesTextInputAndNULTrigger(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	m := f.request.bundle.bundle.Program.Metadata.Definitions[0].Agent
	for key, trigger := range m.Triggers {
		trigger.Input = []byte(`[{"type":"text","text":"a\u0000b"}]`)
		m.Triggers[key] = trigger
	}
	r, err := register(t.Context(), f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.principal(), f.scope(), r.ID, nil); err != nil {
		t.Fatal(err)
	}
	var input []byte
	if err := f.pool.QueryRow(t.Context(), `SELECT input FROM agent_triggers WHERE deployment_id=$1 LIMIT 1`, r.ID).Scan(&input); err != nil {
		t.Fatal(err)
	}

	if string(input) != `[{"type":"text","text":"a\u0000b"}]` {
		t.Fatalf("trigger changed %s", input)
	}
}

// TestTriggerActivationsFollowPromotionsAndShareOccurrenceIdentity promotes a
// deployment, a successor, then the first again. Each promotion creates a new
// activation from that deployment's declaration rows, a rollback starts at its
// own cutover instead of replaying the gap, and an occurrence is identified by
// (agent, key, scheduled_at) across every activation and deployment.
func TestTriggerActivationsFollowPromotionsAndShareOccurrenceIdentity(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	minute := map[string]definition.CronTrigger{"minute": {Cron: "* * * * *", Timezone: "UTC", Input: []byte(`[]`)}}
	f.request.bundle.bundle.Program.Metadata.Definitions[0].Agent.Triggers = minute
	first, err := register(t.Context(), f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	f.request.idempotencyKey = "second"
	f.request.bundle.root.Digest = artifacttest.Digest("second bundle")
	f.request.bundle.bundle.Program.Artifact.Digest = artifacttest.Digest("second program")
	second, err := register(t.Context(), f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for i, deployment := range []uuid.UUID{first.ID, second.ID, first.ID} {
		cutover := base.Add(time.Duration(i)*time.Minute + 30*time.Second)
		if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
			return reconcileTriggers(t.Context(), tx, first.EnvironmentID, deployment, cutover, nil)
		}); err != nil {
			t.Fatal(err)
		}
	}
	type activation struct {
		id, deployment uuid.UUID
		from, next     time.Time
		until          *time.Time
	}
	rows, err := f.pool.Query(t.Context(), `SELECT a.id,a.deployment_id,a.active_from,a.next_fire_at,a.active_until FROM trigger_activations a
 JOIN agent_triggers t ON (t.environment_id,t.agent_id,t.deployment_id,t.key)=(a.environment_id,a.agent_id,a.deployment_id,a.trigger_key)
 WHERE a.environment_id=$1 AND a.trigger_key='minute' ORDER BY a.active_from`, first.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	activations, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (activation, error) {
		var a activation
		err := row.Scan(&a.id, &a.deployment, &a.from, &a.next, &a.until)
		return a, err
	})
	if err != nil || len(activations) != 3 {
		t.Fatalf("activations=%d: %v", len(activations), err)
	}
	for i, want := range []uuid.UUID{first.ID, second.ID, first.ID} {
		a := activations[i]
		if a.deployment != want || !a.next.Equal(a.from.Truncate(time.Minute).Add(time.Minute)) {
			t.Fatalf("activation %d: deployment=%s next=%s from=%s", i, a.deployment, a.next, a.from)
		}
		if (i < 2) != (a.until != nil) || (i < 2 && !a.until.Equal(activations[i+1].from)) {
			t.Fatalf("activation %d interval does not end at its successor's cutover", i)
		}
	}
	var agentID uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT agent_id FROM trigger_activations WHERE id=$1`, activations[1].id).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	instant := activations[1].next
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO trigger_occurrences(environment_id,activation_id,agent_id,trigger_key,scheduled_at,disposition,reason,evaluated_at) VALUES($1,$2,$3,'minute',$4,'missed','superseded',now())`, first.EnvironmentID, activations[1].id, agentID, instant)
	_, err = f.pool.Exec(t.Context(), `INSERT INTO trigger_occurrences(environment_id,activation_id,agent_id,trigger_key,scheduled_at,disposition,reason,evaluated_at) VALUES($1,$2,$3,'minute',$4,'missed','superseded',now())`, first.EnvironmentID, activations[2].id, agentID, instant)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("same instant recorded under another deployment's activation: %v", err)
	}
}

func TestPromotionAuditGrantActorAndRollback(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	record, err := register(t.Context(), f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	principal := f.principal()
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE FUNCTION reject_promotion_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected audit failure'; END $$`)
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE TRIGGER reject_promotion_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_promotion_audit()`)
	if _, err = Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, principal, f.scope(), record.ID, nil); err == nil {
		t.Fatal("audit failure allowed promotion")
	}
	var unchanged bool
	if err = f.pool.QueryRow(t.Context(), `SELECT current_deployment_id IS NULL FROM environments WHERE id=$1`, f.request.environmentID).Scan(&unchanged); err != nil || !unchanged {
		t.Fatal("promotion survived rollback", unchanged, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `DROP TRIGGER reject_promotion_audit ON audit_events`)
	if _, err = Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, principal, f.scope(), record.ID, nil); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='deployment.promoted' AND target_id=$1 AND actor_kind='api_key' AND credential_id=$2 AND actor_id=$2 AND org_id=$3 AND project_id=$4 AND environment_id=$5 AND outcome='succeeded'`, record.ID, principal.APIKeyID, principal.OrgID, f.request.projectID, f.request.environmentID).Scan(&count); err != nil || count != 1 {
		t.Fatal("promotion fact differs", count, err)
	}
}

func TestPromoteRejectsStructuredContractForConnectedFront(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	first, err := register(t.Context(), f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.principal(), f.scope(), first.ID, nil); err != nil {
		t.Fatal(err)
	}
	user, registration, connection := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO users(id,display_name) VALUES($1,'Schema'); INSERT INTO slack_app_registrations(transport,id,org_id,app_id,client_id,credential_revision,credential_ciphertext,credential_nonce,created_by_user_id) VALUES('http',$2,$3,'app','client',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),$1); INSERT INTO slack_connections(id,environment_id,agent_id,slack_app_registration_id,created_by_user_id) SELECT $4,$5,id,$2,$1 FROM agents WHERE environment_id=$5 AND key=$6`, pgx.QueryExecModeSimpleProtocol, user, registration, f.request.orgID, connection, first.EnvironmentID, f.request.bundle.bundle.Program.Metadata.Definitions[0].DeclaredID)
	f.request.idempotencyKey = "structured"
	f.request.bundle.root.Digest = artifacttest.Digest("structured-bundle")
	f.request.bundle.bundle.Program.Metadata.Definitions[0].Agent.InputKind = "json"
	f.request.bundle.bundle.Program.Metadata.Definitions[0].Agent.InputSchema = &definition.InputSchemaDiscovery{Status: "unavailable", Reason: "missing_converter"}
	next, err := register(t.Context(), f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.principal(), f.scope(), next.ID, nil); err == nil {
		t.Fatal("connected front changed its input contract")
	}
	var current uuid.UUID
	if err = f.pool.QueryRow(t.Context(), `SELECT current_deployment_id FROM environments WHERE id=$1`, first.EnvironmentID).Scan(&current); err != nil || current != first.ID {
		t.Fatalf("failed promotion changed deployment: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_connections SET revoked_at=clock_timestamp() WHERE id=$1`, connection)
	if _, err = Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.principal(), f.scope(), next.ID, nil); err != nil {
		t.Fatal(err)
	}
}
