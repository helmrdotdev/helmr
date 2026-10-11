package deployment

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
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
	f.request.bundle.bundle.Program.Metadata.Definitions[2].Computer.Refresh = &definition.ComputerRefresh{EveryMs: every}
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
	if err = f.pool.QueryRow(t.Context(), `SELECT id,active_from FROM agent_schedules WHERE active_until IS NULL LIMIT 1`).Scan(&previous, &activeFrom); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT refresh_every_ms,next_refresh_at FROM computer_preparation_specs`).Scan(&cadence, &nextRefresh); err != nil || cadence != every {
		t.Fatalf("refresh=%d: %v", cadence, err)
	}
	if _, err = Promote(t.Context(), f.pool, f.principal(), f.scope(), first.ID, nil); err != nil {
		t.Fatal(err)
	}
	var until, started time.Time
	var current uuid.UUID
	var preserved time.Time
	if err = f.pool.QueryRow(t.Context(), `SELECT active_until FROM agent_schedules WHERE id=$1`, previous).Scan(&until); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT id,active_from FROM agent_schedules WHERE active_until IS NULL LIMIT 1`).Scan(&current, &started); err != nil {
		t.Fatal(err)
	}
	if current == previous || !until.Equal(started) || until.Before(activeFrom) {
		t.Fatal("re-promotion rewrote interval or left cutover gap")
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT next_refresh_at FROM computer_preparation_specs`).Scan(&preserved); err != nil || !preserved.Equal(nextRefresh) {
		t.Fatalf("unchanged cadence reset: %v", err)
	}
	// A new Program with no refresh stops background work for its older inputs.
	f.request.retryKey = "second"
	f.request.bundle.root.Digest = artifacttest.Digest("second bundle")
	f.request.bundle.bundle.Program.Artifact.Digest = artifacttest.Digest("second program")
	f.request.bundle.bundle.Program.Metadata.Definitions[2].Computer.Refresh = nil
	f.request.bundle.bundle.Program.Metadata.Definitions[0].Agent.Triggers = map[string]definition.CronTrigger{}
	second, err := register(t.Context(), f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Promote(t.Context(), f.pool, f.principal(), f.scope(), second.ID, nil); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_preparation_specs WHERE next_refresh_at IS NOT NULL`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("abandoned refresh %d: %v", count, err)
	}
	// Re-promoting without refresh must not rewrite inactive historical specs.
	var beforeCTID, afterCTID string
	if err = f.pool.QueryRow(t.Context(), `SELECT s.ctid::text FROM computer_preparation_specs s JOIN computer_definitions d ON (d.environment_id,d.preparation_spec_id)=(s.environment_id,s.id) WHERE d.deployment_id=$1`, first.ID).Scan(&beforeCTID); err != nil {
		t.Fatal(err)
	}
	if _, err = Promote(t.Context(), f.pool, f.principal(), f.scope(), second.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT s.ctid::text FROM computer_preparation_specs s JOIN computer_definitions d ON (d.environment_id,d.preparation_spec_id)=(s.environment_id,s.id) WHERE d.deployment_id=$1`, first.ID).Scan(&afterCTID); err != nil || afterCTID != beforeCTID {
		t.Fatalf("historical spec rewritten %s -> %s: %v", beforeCTID, afterCTID, err)
	}
	// Old definitions remain executable pins for existing Sessions.
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_definitions WHERE deployment_id=$1`, first.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("old definitions lost: %d %v", count, err)
	}
}

func TestScheduleCutoverOwnsExactBoundary(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	f.request.bundle.bundle.Program.Metadata.Definitions[0].Agent.Triggers = map[string]definition.CronTrigger{"minute": {Cron: "* * * * *", Timezone: "UTC", Input: []byte(`[]`)}}
	r, err := register(t.Context(), f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	cutover := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	for range 2 {
		if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error { return reconcileSchedules(t.Context(), tx, r.EnvironmentID, r.ID, cutover, nil) }); err != nil {
			t.Fatal(err)
		}
	}
	var next time.Time
	if err := f.pool.QueryRow(t.Context(), `SELECT next_fire_at FROM agent_schedules WHERE active_until IS NULL AND trigger_key='minute'`).Scan(&next); err != nil || !next.Equal(cutover) {
		t.Fatalf("boundary next=%s: %v", next, err)
	}
	var bad int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_schedules WHERE active_until IS NOT NULL AND active_until<>$1`, cutover).Scan(&bad); err != nil || bad != 0 {
		t.Fatalf("bad cutover %d: %v", bad, err)
	}
}

func TestPromoteRejectsRevokedAndRollsBackInvalidSchedule(t *testing.T) {
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
	if _, err = f.pool.Exec(t.Context(), `UPDATE deployments SET execution_revoked_at=NULL WHERE id=$1; UPDATE agent_definitions SET triggers='{"bad":{"cron":"invalid","timezone":"UTC","input":null}}' WHERE deployment_id=$1`, pgx.QueryExecModeSimpleProtocol, r.ID); err != nil {
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
	if err != nil || len(page.DeclaredIDs) != 1 || !page.HasMore || page.DeclaredIDs[0] != "build" {
		t.Fatalf("page: %+v %v", page, err)
	}
	id, name, err := GetDefinition(t.Context(), f.pool, f.principal(), f.scope(), definition.KindComputer, &r.ID, "repo")
	if err != nil || id != r.ID || name != "repo" {
		t.Fatalf("computer: %s %s %v", id, name, err)
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
	if err := f.pool.QueryRow(t.Context(), `SELECT input FROM agent_schedules WHERE deployment_id=$1 LIMIT 1`, r.ID).Scan(&input); err != nil {
		t.Fatal(err)
	}

	if string(input) != `[{"type":"text","text":"a\u0000b"}]` {
		t.Fatalf("trigger changed %s", input)
	}
}
