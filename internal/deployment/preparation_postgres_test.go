package deployment

import (
	"errors"
	"slices"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestPromotionPreparesBeforeChangingCurrent(t *testing.T) {
	t.Run("authored", func(t *testing.T) { testPendingPromotion(t, true) })
	t.Run("agent-seed", func(t *testing.T) { testPendingPromotion(t, false) })
}

func testPendingPromotion(t *testing.T, authored bool) {
	t.Helper()
	f := newDeploymentFinalizePostgresFixture(t)
	defs := f.request.bundle.bundle.Program.Metadata.Definitions
	f.request.bundle.bundle.Program.Metadata.Definitions = defs[2:]
	prior, err := register(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.principal(), f.scope(), prior.ID, nil); err != nil {
		t.Fatal(err)
	}
	f.request.bundle.bundle.Program.Metadata.Definitions = defs
	f.request.idempotencyKey = "prepared-candidate"
	f.request.bundle.root.Digest = artifacttest.Digest("prepared bundle")
	f.request.bundle.bundle.Program.Artifact.Digest = artifacttest.Digest("prepared program")
	f.request.bundle.bundle.Program.Metadata.Definitions[2].Template.Prepare = authored
	f.request.bundle.bundle.Preparations = artifacttest.PreparationOutputs(t, f.request.bundle.bundle.Program)
	candidate, err := register(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		pending, err := Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.principal(), f.scope(), candidate.ID, nil)
		if err != nil || pending.PreparationState != "preparing" {
			t.Fatalf("initial preparation: %+v %v", pending, err)
		}
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM image_builds`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("promotion did not coalesce: %d %v", count, err)
	}
	current, err := GetCurrent(t.Context(), f.pool, f.principal(), f.scope())
	if err != nil || current.ID != prior.ID {
		t.Fatalf("unprepared candidate changed current: %+v %v", current, err)
	}
	var attempt uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM image_builds`).Scan(&attempt); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_builds SET deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, attempt)
	if _, err := Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.principal(), f.scope(), candidate.ID, nil); !errors.Is(err, computer.ErrNotReady) {
		t.Fatalf("unusable build reported as accepted preparation: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_builds SET deadline_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, attempt)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_builds SET status='failed',error_code='preparation_execution_failed' WHERE id=$1`, attempt)
	for range 2 {
		failed, err := Get(t.Context(), f.pool, f.principal(), f.scope(), candidate.ID)
		if err != nil || failed.PreparationState != "failed" {
			t.Fatalf("failure observation: %+v %v", failed, err)
		}
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM image_builds`).Scan(&count); err != nil || count != 1 {
		t.Fatal("observation retried preparation")
	}
	if _, err := Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.principal(), f.scope(), candidate.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM image_builds`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("explicit retry: %d %v", count, err)
	}
	current, err = GetCurrent(t.Context(), f.pool, f.principal(), f.scope())
	if err != nil || current.ID != prior.ID {
		t.Fatal("failed preparation replaced current")
	}
}

// Other promotion tests start from a prepared image so their cutover and
// rollback assertions exercise the transaction that changes current.
func (f deploymentFinalizePostgresFixture) seedReadyImage(t *testing.T, deployment uuid.UUID) {
	t.Helper()
	var env, spec uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT environment_id,(SELECT image_spec_id FROM template_definitions WHERE deployment_id=d.id) FROM deployments d WHERE id=$1`, deployment).Scan(&env, &spec); err != nil {
		t.Fatal(err)
	}
	sessiontest.PublishFixtureImage(t, f.pool, env, spec)
}

func TestPromotionSelectsTemplatesFromCandidateDeployment(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	ctx := t.Context()
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	// Both Agents reference repo in the earlier deployment only.
	if _, err := register(ctx, policy, f.pool, f.request); err != nil {
		t.Fatal(err)
	}
	f.request.idempotencyKey = "mixed"
	f.request.bundle.root.Digest = artifacttest.Digest("mixed bundle")
	program := &f.request.bundle.bundle.Program
	program.Artifact.Digest = artifacttest.Digest("mixed program")
	defs := program.Metadata.Definitions
	seed := defs[2].Clone()
	seed.DeclaredID = "selected"
	authored := defs[2].Clone()
	authored.DeclaredID = "with-prepare"
	authored.Template.Prepare = true
	for _, agent := range defs[:2] {
		agent.Agent.TemplateDefinitionID = seed.DeclaredID
	}
	program.Metadata.Definitions = append(defs, seed, authored)
	f.request.bundle.bundle.Preparations = artifacttest.PreparationOutputs(t, *program)
	next, err := register(ctx, policy, f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		pending, err := Promote(ctx, f.pool, policy, f.principal(), f.scope(), next.ID, nil)
		if err != nil || pending.PreparationState != "preparing" {
			t.Fatalf("mixed promotion: %+v %v", pending, err)
		}
	}
	var builds []string
	err = f.pool.QueryRow(ctx, `SELECT array_agg(t.key ORDER BY t.key) FROM image_builds b
 JOIN template_definitions d ON (d.environment_id,d.image_spec_id)=(b.environment_id,b.image_spec_id)
 JOIN templates t ON (t.environment_id,t.id)=(d.environment_id,d.template_id)
 WHERE d.deployment_id=$1`, next.ID).Scan(&builds)
	if err != nil || !slices.Equal(builds, []string{"selected", "with-prepare"}) {
		t.Fatalf("built templates: %v, %v", builds, err)
	}
}
