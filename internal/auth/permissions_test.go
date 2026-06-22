package auth

import (
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

func TestGranularWorkspacePermissionsDoNotEscalate(t *testing.T) {
	orgID := uuid.New()
	scope := Scope{
		OrgID:         orgID,
		ProjectID:     "00000000-0000-0000-0000-000000000101",
		EnvironmentID: "00000000-0000-0000-0000-000000000102",
	}
	actor := Actor{
		OrgID:         orgID,
		Kind:          ActorKindAPIKey,
		Role:          RoleDeveloper,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		Permissions: []Permission{
			PermissionFilesRead,
			PermissionVersionsRead,
			PermissionExecRead,
			PermissionPtyRead,
			PermissionPortsRead,
		},
	}

	for _, permission := range []Permission{
		PermissionWorkspaceLifecycleManage,
		PermissionFilesWrite,
		PermissionVersionsCapture,
		PermissionVersionsRestore,
		PermissionVersionsDiff,
		PermissionExecCreate,
		PermissionExecManage,
		PermissionPtyCreate,
		PermissionPtyManage,
		PermissionPortsExpose,
		PermissionPortsClose,
	} {
		if actor.HasPermission(permission, scope) {
			t.Fatalf("read-only workspace grants allowed %s", permission)
		}
	}
}
