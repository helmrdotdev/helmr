package controlplane

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"testing"
)

func TestGuestSecretCeilingRejectsRawAndWiderExistingTargets(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 2)
	source, target := pgvalue.UUID(fixture.workspaceIDs[0]), pgvalue.UUID(fixture.workspaceIDs[1])
	q := db.New(fixture.pool)
	dbtest.MustExec(t, t.Context(), fixture.pool, `UPDATE workspace_secrets SET mode='protected',allowed_origins=ARRAY['https://api.example.com'],placeholder='hlmr_protected_'||repeat('a',64) WHERE workspace_id=$1`, source)
	if err := authorizeWorkspaceSecretTarget(t.Context(), q, source, target); !errors.Is(err, errWorkspaceSecretUnavailable) {
		t.Fatalf("protected -> raw target=%v", err)
	}
	protected := api.WorkspaceSecret{Name: "API_TOKEN", Env: &api.SecretEnv{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://api.example.com"}}}
	if err := authorizeWorkspaceSecretCreate(t.Context(), q, source, pgvalue.UUID(fixture.environmentID), []api.WorkspaceSecret{protected}); err != nil {
		t.Fatal(err)
	}
	for _, binding := range []api.WorkspaceSecret{
		{Name: "API_TOKEN", Env: &api.SecretEnv{Name: "TOKEN", Mode: "raw"}},
		{Name: "API_TOKEN", File: &api.SecretFile{Path: "/run/secrets/key"}},
		{Name: "API_TOKEN", Env: &api.SecretEnv{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://other.example.com"}}},
		{Name: "other-token", Env: &api.SecretEnv{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://api.example.com"}}},
	} {
		if err := authorizeWorkspaceSecretCreate(t.Context(), q, source, pgvalue.UUID(fixture.environmentID), []api.WorkspaceSecret{binding}); !errors.Is(err, errWorkspaceSecretUnavailable) {
			t.Fatalf("create escalation=%v", err)
		}
	}
	dbtest.MustExec(t, t.Context(), fixture.pool, `UPDATE workspace_secrets SET mode='protected',allowed_origins=ARRAY['https://api.example.com','https://other.example.com'],placeholder='hlmr_protected_'||repeat('b',64) WHERE workspace_id=$1`, target)
	if err := authorizeWorkspaceSecretTarget(t.Context(), q, source, target); !errors.Is(err, errWorkspaceSecretUnavailable) {
		t.Fatalf("wider target=%v", err)
	}
	dbtest.MustExec(t, t.Context(), fixture.pool, `UPDATE workspace_secrets SET allowed_origins=ARRAY['https://api.example.com'] WHERE workspace_id=$1`, target)
	if err := authorizeWorkspaceSecretTarget(t.Context(), q, source, target); err != nil {
		t.Fatal(err)
	}
	// Trusted authors may deliberately add raw authority at a distinct target.
	dbtest.MustExec(t, t.Context(), fixture.pool, `INSERT INTO workspace_secrets(workspace_id,environment_id,secret_id,placement_kind,placement_target,mode) SELECT workspace_id,environment_id,secret_id,'env','RAW_TOKEN','raw' FROM workspace_secrets WHERE workspace_id=$1`, source)
	if err := authorizeWorkspaceSecretCreate(t.Context(), q, source, pgvalue.UUID(fixture.environmentID), []api.WorkspaceSecret{{Name: "API_TOKEN", Env: &api.SecretEnv{Name: "TOKEN", Mode: "raw"}}}); err != nil {
		t.Fatal(err)
	}
}
