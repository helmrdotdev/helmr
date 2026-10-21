package session_test

import (
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"testing"
	"uuid"
)

func TestPromotionRechecksPublishedImageEligibility(t *testing.T) {
	p, key := newPreparationPublicationTest(t)
	waiter := p.f.waiter(t)
	original := p.f.attach(t, waiter)
	root := p.capture(t, key)
	if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "certified"); err != nil {
		t.Fatal(err)
	}
	if err := computer.ObserveImageBuildStopped(t.Context(), p.f.pool, *p.f.host(), preparationIdentity(p.ref)); err != nil {
		t.Fatal(err)
	}
	var org, project uuid.UUID
	if err := p.f.pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, p.f.env).Scan(&org, &project); err != nil {
		t.Fatal(err)
	}
	principal := auth.Principal{OrgID: org, Kind: auth.PrincipalKindAPIKey, APIKeyID: uuid.NewV7(), Role: auth.RoleDeveloper, ProjectID: project.String(), EnvironmentID: p.f.env.String(), Permissions: []auth.Permission{auth.PermissionDeploymentsWrite}}
	scope := auth.Scope{OrgID: org, ProjectID: project.String(), EnvironmentID: p.f.env.String()}
	dbtest.MustExec(t, t.Context(), p.f.pool, `UPDATE template_definitions SET prepare=true WHERE environment_id=$1 AND deployment_id=$2`, p.f.env, p.f.deployment)
	got, err := deployment.Promote(t.Context(), p.f.pool, principal, scope, p.f.deployment, nil)
	if err != nil || got.PreparationState != "" {
		t.Fatalf("eligible promotion: %+v %v", got, err)
	}
	if _, err := p.f.secrets.Rotate(t.Context(), audit.System("test-fixture"), p.f.env, p.f.secretID, []byte("v2"), "rotate"); err != nil {
		t.Fatal(err)
	}
	got, err = deployment.Promote(t.Context(), p.f.pool, principal, scope, p.f.deployment, nil)
	if err != nil || got.PreparationState != "preparing" {
		t.Fatalf("rotated image promotion: %+v %v", got, err)
	}
	var count int
	if err := p.f.pool.QueryRow(t.Context(), `SELECT count(*) FROM image_builds WHERE environment_id=$1`, p.f.env).Scan(&count); err != nil || count != 1 {
		t.Fatalf("promotion preempted attached rotation successor: %d %v", count, err)
	}
	observed, err := deployment.Get(t.Context(), p.f.pool, principal, scope, p.f.deployment)
	if err != nil || observed.PreparationState != "preparing" {
		t.Fatalf("pending successor observation: %+v %v", observed, err)
	}
	successor, err := computer.EnsureImageBuildSuccessor(t.Context(), p.f.pool, p.f.env, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err = deployment.Promote(t.Context(), p.f.pool, principal, scope, p.f.deployment, nil)
	if err != nil || got.PreparationState != "preparing" {
		t.Fatalf("coalescing successor: %+v %v", got, err)
	}
	var retained uuid.UUID
	if err := p.f.pool.QueryRow(t.Context(), `SELECT image_build_id FROM computers WHERE environment_id=$1 AND id=$2`, p.f.env, waiter).Scan(&retained); err != nil || retained != original.ID {
		t.Fatalf("promotion changed original attachment: %v %v", retained, err)
	}
	var direct bool
	if err := p.f.pool.QueryRow(t.Context(), `SELECT successor_of=$3 AND (SELECT count(*) FROM image_builds WHERE environment_id=$1)=2 FROM image_builds WHERE environment_id=$1 AND id=$2`, p.f.env, successor.ID, original.ID).Scan(&direct); err != nil || !direct {
		t.Fatalf("promotion did not share the direct successor: %v %v", direct, err)
	}

}
