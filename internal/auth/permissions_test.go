package auth

import (
	"testing"

	"uuid"
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
			PermissionWorkspacesRead,
			PermissionRunsRead,
		},
	}

	for _, permission := range []Permission{
		PermissionWorkspacesCreate,
		PermissionWorkspacesDelete,
		PermissionWorkspaceExecCreate,
	} {
		if actor.HasPermission(permission, scope) {
			t.Fatalf("read-only workspace grants allowed %s", permission)
		}
	}
}

func TestSessionSendPermissionRequiresWritableRole(t *testing.T) {
	for _, permission := range []Permission{PermissionSessionsSend, PermissionSessionsInterrupt, PermissionSessionsResume, PermissionSessionsClose} {
		if !RoleAllows(RoleDeveloper, permission) || RoleAllows(RoleViewer, permission) {
			t.Fatalf("Session mutation %s must require a writable role", permission)
		}
		if normalized, ok := ParseAPIKeyGrant(string(permission)); !ok || normalized != permission {
			t.Fatalf("Session mutation grant = %v", normalized)
		}
	}
}

func TestSessionRecoveryRequiresPrivilegedRoleAndExplicitScopedKeyGrant(t *testing.T) {
	scope := Scope{ProjectID: "project", EnvironmentID: "environment"}
	for _, role := range []Role{RoleOwner, RoleAdmin, RoleDeveloper, RoleViewer} {
		key := Actor{Kind: ActorKindAPIKey, Role: role, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID}
		if key.HasPermission(PermissionSessionsRecover, scope) {
			t.Fatalf("%s key recovered without an explicit grant", role)
		}
		key.Permissions = []Permission{PermissionSessionsRecover}
		want := role == RoleOwner || role == RoleAdmin
		if got := key.HasPermission(PermissionSessionsRecover, scope); got != want {
			t.Fatalf("recovery role %s = %v, want %v", role, got, want)
		}
		if key.HasPermission(PermissionSessionsRecover, Scope{ProjectID: scope.ProjectID, EnvironmentID: "foreign"}) {
			t.Fatalf("%s key recovered in another environment", role)
		}
	}
	if permission, ok := ParseAPIKeyGrant(string(PermissionSessionsRecover)); !ok || permission != PermissionSessionsRecover {
		t.Fatal("recovery must be an explicit grant")
	}
}

func TestActorStartPermissionIsWritableButNotReadableRoleAuthority(t *testing.T) {
	if !RoleAllows(RoleDeveloper, PermissionActorsStart) {
		t.Fatal("developer should be allowed to start an Actor")
	}
	if RoleAllows(RoleViewer, PermissionActorsStart) {
		t.Fatal("viewer should not be allowed to start an Actor")
	}
	normalized, ok := ParseAPIKeyGrant(string(PermissionActorsStart))
	if !ok || normalized != PermissionActorsStart {
		t.Fatalf("normalized Actor start permission = %v", normalized)
	}
}

func TestActorReadPermissionAllowsReadOnlyRoles(t *testing.T) {
	for _, role := range []Role{RoleOwner, RoleAdmin, RoleDeveloper, RoleViewer} {
		if !RoleAllows(role, PermissionSessionsRead) {
			t.Fatalf("%s should be allowed to read Actors", role)
		}
	}
	normalized, ok := ParseAPIKeyGrant(string(PermissionSessionsRead))
	if !ok || normalized != PermissionSessionsRead {
		t.Fatalf("normalized Actor read permission = %v", normalized)
	}
}
