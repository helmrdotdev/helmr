package deployment

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func (f deploymentFinalizePostgresFixture) principal() auth.Principal {
	return auth.Principal{OrgID: f.request.orgID, Kind: auth.PrincipalKindAPIKey, Role: auth.RoleDeveloper, ProjectID: pgvalue.UUIDString(f.request.projectID), EnvironmentID: pgvalue.UUIDString(f.request.environmentID), Permissions: []auth.Permission{auth.PermissionDeploymentsWrite}}
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
	if _, err = Promote(t.Context(), f.pool, f.principal(), f.scope(), first.ID, nil); err != nil {
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
	if _, err = Promote(t.Context(), f.pool, f.principal(), f.scope(), first.ID, nil); err != nil {
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
	f.request.retryKey = "second"
	f.request.bundle.root.Digest = artifacttest.Digest("second bundle")
	f.request.bundle.bundle.Program.Artifact.Digest = artifacttest.Digest("second program")
	f.request.bundle.bundle.Program.Metadata.Definitions[2].Template.Refresh = nil
	f.request.bundle.bundle.Program.Metadata.Definitions[0].Agent.Triggers = map[string]definition.CronTrigger{}
	second, err := register(t.Context(), f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Promote(t.Context(), f.pool, f.principal(), f.scope(), second.ID, nil); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM image_specs WHERE next_refresh_at IS NOT NULL`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("abandoned refresh %d: %v", count, err)
	}
	// Re-promoting without refresh must not rewrite inactive historical specs.
	var beforeCTID, afterCTID string
	if err = f.pool.QueryRow(t.Context(), `SELECT s.ctid::text FROM image_specs s JOIN template_definitions d ON (d.environment_id,d.preparation_spec_id)=(s.environment_id,s.id) WHERE d.deployment_id=$1`, first.ID).Scan(&beforeCTID); err != nil {
		t.Fatal(err)
	}
	if _, err = Promote(t.Context(), f.pool, f.principal(), f.scope(), second.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT s.ctid::text FROM image_specs s JOIN template_definitions d ON (d.environment_id,d.preparation_spec_id)=(s.environment_id,s.id) WHERE d.deployment_id=$1`, first.ID).Scan(&afterCTID); err != nil || afterCTID != beforeCTID {
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

func TestPromoteRejectsRevokedAndRollsBackInvalidTrigger(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	r, err := register(t.Context(), f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(t.Context(), `UPDATE deployments SET execution_revoked_at=clock_timestamp() WHERE id=$1`, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = Promote(t.Context(), f.pool, f.principal(), f.scope(), r.ID, nil); !errors.Is(err, ErrNotDeployable) {
		t.Fatalf("revoked promote: %v", err)
	}
	if _, err = f.pool.Exec(t.Context(), `UPDATE deployments SET execution_revoked_at=NULL WHERE id=$1; INSERT INTO agent_triggers(environment_id,agent_id,deployment_id,key,cron,timezone,input) SELECT environment_id,agent_id,deployment_id,'bad','invalid','UTC','null' FROM agent_definitions WHERE deployment_id=$1 LIMIT 1`, pgx.QueryExecModeSimpleProtocol, r.ID); err != nil {
		t.Fatal(err)
	}
	_, err = Promote(t.Context(), f.pool, f.principal(), f.scope(), r.ID, nil)
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
	if _, err = Promote(t.Context(), f.pool, f.principal(), f.scope(), r.ID, nil); err != nil {
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
	if _, err := Promote(t.Context(), f.pool, f.principal(), f.scope(), r.ID, nil); err != nil {
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
	f.request.retryKey = "second"
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
