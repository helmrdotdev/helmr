package auth

import (
	"testing"

	"github.com/google/uuid"
)

func TestAPIKeyPermissionIsLimitedByCreatorRole(t *testing.T) {
	orgID := uuid.New()
	actor := Actor{
		OrgID: orgID,
		Kind:  ActorKindAPIKey,
		Role:  RoleViewer,
		Permissions: []PermissionGrant{{
			ProjectID:     "00000000-0000-0000-0000-000000000101",
			EnvironmentID: "00000000-0000-0000-0000-000000000102",
			Permissions:   []Permission{PermissionRunsRead, PermissionSecretsWrite},
		}},
	}

	scope := Scope{OrgID: orgID, ProjectID: "00000000-0000-0000-0000-000000000101", EnvironmentID: "00000000-0000-0000-0000-000000000102"}
	if !actor.HasPermission(PermissionRunsRead, scope) {
		t.Fatal("viewer-backed api key should keep read grants")
	}
	if actor.HasPermission(PermissionSecretsWrite, scope) {
		t.Fatal("viewer-backed api key should not keep write grants after demotion")
	}
}

func TestOrgLevelGrantDoesNotMatchEnvironmentScope(t *testing.T) {
	orgID := uuid.New()
	actor := Actor{
		OrgID: orgID,
		Kind:  ActorKindAPIKey,
		Role:  RoleOwner,
		Permissions: []PermissionGrant{{
			Permissions: []Permission{PermissionRunsRead},
		}},
	}
	scope := Scope{
		OrgID:         orgID,
		ProjectID:     "00000000-0000-0000-0000-000000000101",
		EnvironmentID: "00000000-0000-0000-0000-000000000102",
	}

	if actor.HasPermission(PermissionRunsRead, scope) {
		t.Fatal("org-level grant matched an environment-scoped resource")
	}
	if !actor.HasPermission(PermissionRunsRead, Scope{OrgID: orgID}) {
		t.Fatal("org-level grant should match org-level scope")
	}

	concreteActor := Actor{
		OrgID: orgID,
		Kind:  ActorKindAPIKey,
		Role:  RoleOwner,
		Permissions: []PermissionGrant{{
			ProjectID:     scope.ProjectID,
			EnvironmentID: scope.EnvironmentID,
			Permissions:   []Permission{PermissionRunsRead},
		}},
	}
	if concreteActor.HasPermission(PermissionRunsRead, Scope{OrgID: orgID}) {
		t.Fatal("environment-scoped grant matched an org-level scope")
	}
}
