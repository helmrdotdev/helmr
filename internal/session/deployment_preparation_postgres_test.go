package session_test

import (
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"testing"
	"uuid"
)

func TestPromotionRechecksPublishedImageEligibility(t *testing.T) {
	p, key := newPreparationPublicationTest(t)
	root := p.capture(t, key)
	if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "certified"); err != nil {
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
}
