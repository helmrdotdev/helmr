package deployment

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/computer"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestPromotionPreparesBeforeChangingCurrent(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	prior, err := register(t.Context(), f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.principal(), f.scope(), prior.ID, nil); err != nil {
		t.Fatal(err)
	}
	f.request.idempotencyKey = "prepared-candidate"
	f.request.bundle.root.Digest = artifacttest.Digest("prepared bundle")
	f.request.bundle.bundle.Program.Artifact.Digest = artifacttest.Digest("prepared program")
	f.request.bundle.bundle.Program.Metadata.Definitions[2].Template.Prepare = true
	f.request.bundle.bundle.Preparations = artifacttest.PreparationOutputs(t, f.request.bundle.bundle.Program)
	candidate, err := register(t.Context(), f.pool, f.request)
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
		t.Fatal("observation retried authored preparation")
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
