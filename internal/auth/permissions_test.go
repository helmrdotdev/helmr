package auth

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestAPIKeyPermissionIsLimitedByCreatorRole(t *testing.T) {
	orgID := uuid.New()
	actor := Actor{
		OrgID:         orgID,
		Kind:          ActorKindAPIKey,
		Role:          RoleViewer,
		ProjectID:     "00000000-0000-0000-0000-000000000101",
		EnvironmentID: "00000000-0000-0000-0000-000000000102",
		Permissions:   []Permission{PermissionRunsRead, PermissionSecretsWrite},
	}

	scope := Scope{OrgID: orgID, ProjectID: "00000000-0000-0000-0000-000000000101", EnvironmentID: "00000000-0000-0000-0000-000000000102"}
	if !actor.HasPermission(PermissionRunsRead, scope) {
		t.Fatal("viewer-backed api key should keep read grants")
	}
	if actor.HasPermission(PermissionSecretsWrite, scope) {
		t.Fatal("viewer-backed api key should not keep write grants after demotion")
	}
}

func TestAPIKeyScopeDoesNotMatchOrgScope(t *testing.T) {
	orgID := uuid.New()
	actor := Actor{
		OrgID:       orgID,
		Kind:        ActorKindAPIKey,
		Role:        RoleOwner,
		Permissions: []Permission{PermissionRunsRead},
	}
	scope := Scope{
		OrgID:         orgID,
		ProjectID:     "00000000-0000-0000-0000-000000000101",
		EnvironmentID: "00000000-0000-0000-0000-000000000102",
	}

	if actor.HasPermission(PermissionRunsRead, scope) {
		t.Fatal("api key without environment scope matched an environment-scoped resource")
	}
	if actor.HasPermission(PermissionRunsRead, Scope{OrgID: orgID}) {
		t.Fatal("api key matched org-level scope")
	}

	concreteActor := Actor{
		OrgID:         orgID,
		Kind:          ActorKindAPIKey,
		Role:          RoleOwner,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		Permissions:   []Permission{PermissionRunsRead},
	}
	if concreteActor.HasPermission(PermissionRunsRead, Scope{OrgID: orgID}) {
		t.Fatal("environment-scoped api key matched an org-level scope")
	}
}

func TestPermissionsFromAPIKeyIncludesWaitpointPolicies(t *testing.T) {
	permissions, err := permissionsFromAPIKey(json.RawMessage(`[{"permission":"waitpoint_policies.manage"}]`))
	if err != nil {
		t.Fatalf("permissionsFromAPIKey returned error: %v", err)
	}
	if len(permissions) != 1 {
		t.Fatalf("got %d permissions, want 1", len(permissions))
	}
	if permissions[0] != PermissionWaitpointPolicies {
		t.Fatalf("got permission %q, want %q", permissions[0], PermissionWaitpointPolicies)
	}
}
