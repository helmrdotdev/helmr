package deployment

import (
	"errors"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestTemplateRefreshCoalescesAndPinsReceiptAcrossPromotion(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	f.request.bundle.bundle.Program.Metadata.Definitions[2].Template.Prepare = true
	f.request.bundle.bundle.Preparations = artifacttest.PreparationOutputs(t, f.request.bundle.bundle.Program)
	first, err := register(t.Context(), f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	env := pgvalue.MustUUIDValue(f.request.environmentID)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, env, first.ID)
	builds := make([]TemplateBuild, 3)
	errs := make([]error, 3)
	var wg sync.WaitGroup
	for i := range builds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := "same"
			if i == 2 {
				key = "different"
			}
			builds[i], errs[i] = RefreshTemplate(t.Context(), f.pool, f.principal(), f.scope(), "repo", key)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
		if builds[i].ImageBuildID != builds[0].ImageBuildID || builds[i].DeploymentID != first.ID {
			t.Fatalf("requests did not coalesce: %+v", builds)
		}
	}
	original := builds[0]
	req := f.request
	req.idempotencyKey = "second"
	req.bundle.root.Digest = artifacttest.Digest("second bundle")
	second, err := register(t.Context(), f.pool, req)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, env, second.ID)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_builds SET status='failed',error_code='authored_preparation_failed' WHERE environment_id=$1 AND id=$2`, env, original.ImageBuildID)
	replay, err := RefreshTemplate(t.Context(), f.pool, f.principal(), f.scope(), "repo", "same")
	if err != nil || replay.DeploymentID != first.ID || replay.ImageBuildID != original.ImageBuildID || replay.Status != "failed" || replay.Ready {
		t.Fatalf("receipt drifted: %+v %v", replay, err)
	}
	observed, err := ReadTemplateBuild(t.Context(), f.pool, f.principal(), f.scope(), "repo", &first.ID, original.ImageBuildID)
	if err != nil || observed != replay {
		t.Fatalf("observation: %+v %v", observed, err)
	}
	fresh, err := RefreshTemplate(t.Context(), f.pool, f.principal(), f.scope(), "repo", "new-current")
	if err != nil || fresh.DeploymentID != second.ID || fresh.ImageBuildID == original.ImageBuildID || fresh.Status != "queued" {
		t.Fatalf("new current refresh: %+v %v", fresh, err)
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM image_builds WHERE environment_id=$1`, env).Scan(&count); err != nil || count != 2 {
		t.Fatalf("build count %d: %v", count, err)
	}
	wrong := uuid.NewV7()
	if _, err := ReadTemplateBuild(t.Context(), f.pool, f.principal(), f.scope(), "repo", &first.ID, wrong); err == nil {
		t.Fatal("foreign build accepted")
	}
}

func TestTemplateRefreshSerializesWithPromotion(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	// The candidate has a seed-only Template, so promotion needs no preparation.
	candidate, err := register(t.Context(), f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	f.request.idempotencyKey = "prepared-current"
	f.request.bundle.root.Digest = artifacttest.Digest("prepared current")
	f.request.bundle.bundle.Program.Metadata.Definitions[2].Template.Prepare = true
	f.request.bundle.bundle.Preparations = artifacttest.PreparationOutputs(t, f.request.bundle.bundle.Program)
	current, err := register(t.Context(), f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	env := pgvalue.MustUUIDValue(f.request.environmentID)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, env, current.ID)
	lock, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(t.Context())
	if _, err := lock.Exec(t.Context(), `SELECT id FROM environments WHERE id=$1 FOR NO KEY UPDATE`, env); err != nil {
		t.Fatal(err)
	}
	var refreshed TemplateBuild
	var refreshErr, promoteErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		refreshed, refreshErr = RefreshTemplate(t.Context(), f.pool, f.principal(), f.scope(), "repo", "racing-refresh")
	}()
	go func() {
		defer wg.Done()
		_, promoteErr = Promote(t.Context(), f.pool, f.principal(), f.scope(), candidate.ID, nil)
	}()
	if err := lock.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if promoteErr != nil {
		t.Fatal(promoteErr)
	}
	if refreshErr == nil {
		if refreshed.DeploymentID != current.ID {
			t.Fatalf("mixed current resolution: %+v", refreshed)
		}
		replay, err := RefreshTemplate(t.Context(), f.pool, f.principal(), f.scope(), "repo", "racing-refresh")
		if err != nil || replay.ImageBuildID != refreshed.ImageBuildID || replay.DeploymentID != current.ID {
			t.Fatalf("racing receipt: %+v %v", replay, err)
		}
	} else {
		var inputError InputError
		if !errors.As(refreshErr, &inputError) {
			t.Fatalf("unexpected promotion race error: %v", refreshErr)
		}
	}
}
