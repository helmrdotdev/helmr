package auth

import (
	"testing"

	"uuid"
)

func TestAPIKeyPermissionIsLimitedByCreatorRole(t *testing.T) {
	orgID := uuid.New()
	principal := Principal{
		OrgID:         orgID,
		Kind:          PrincipalKindAPIKey,
		Role:          RoleViewer,
		ProjectID:     "00000000-0000-0000-0000-000000000101",
		EnvironmentID: "00000000-0000-0000-0000-000000000102",
		Permissions:   []Permission{PermissionSessionsRead, PermissionSecretsWrite},
	}

	scope := Scope{OrgID: orgID, ProjectID: "00000000-0000-0000-0000-000000000101", EnvironmentID: "00000000-0000-0000-0000-000000000102"}
	if !principal.HasPermission(PermissionSessionsRead, scope) {
		t.Fatal("viewer-backed api key should keep read grants")
	}
	if principal.HasPermission(PermissionSecretsWrite, scope) {
		t.Fatal("viewer-backed api key should not keep write grants after demotion")
	}
}

func TestAPIKeyScopeDoesNotMatchOrgScope(t *testing.T) {
	orgID := uuid.New()
	principal := Principal{
		OrgID:       orgID,
		Kind:        PrincipalKindAPIKey,
		Role:        RoleOwner,
		Permissions: []Permission{PermissionSessionsRead},
	}
	scope := Scope{
		OrgID:         orgID,
		ProjectID:     "00000000-0000-0000-0000-000000000101",
		EnvironmentID: "00000000-0000-0000-0000-000000000102",
	}

	if principal.HasPermission(PermissionSessionsRead, scope) {
		t.Fatal("api key without environment scope matched an environment-scoped resource")
	}
	if principal.HasPermission(PermissionSessionsRead, Scope{OrgID: orgID}) {
		t.Fatal("api key matched org-level scope")
	}

	concretePrincipal := Principal{
		OrgID:         orgID,
		Kind:          PrincipalKindAPIKey,
		Role:          RoleOwner,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		Permissions:   []Permission{PermissionSessionsRead},
	}
	if concretePrincipal.HasPermission(PermissionSessionsRead, Scope{OrgID: orgID}) {
		t.Fatal("environment-scoped api key matched an org-level scope")
	}
}

func TestGranularComputerPermissionsDoNotEscalate(t *testing.T) {
	orgID := uuid.New()
	scope := Scope{
		OrgID:         orgID,
		ProjectID:     "00000000-0000-0000-0000-000000000101",
		EnvironmentID: "00000000-0000-0000-0000-000000000102",
	}
	principal := Principal{
		OrgID:         orgID,
		Kind:          PrincipalKindAPIKey,
		Role:          RoleDeveloper,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		Permissions: []Permission{
			PermissionComputersRead,
			PermissionSessionsRead,
		},
	}

	for _, permission := range []Permission{
		PermissionComputersCreate,
		PermissionComputersDelete,
		PermissionComputerCommandCreate,
	} {
		if principal.HasPermission(permission, scope) {
			t.Fatalf("read-only computer grants allowed %s", permission)
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

func TestAgentStartPermissionIsWritableButNotReadableRoleAuthority(t *testing.T) {
	if !RoleAllows(RoleDeveloper, PermissionAgentsStart) {
		t.Fatal("developer should be allowed to start an Agent")
	}
	if RoleAllows(RoleViewer, PermissionAgentsStart) {
		t.Fatal("viewer should not be allowed to start an Agent")
	}
	normalized, ok := ParseAPIKeyGrant(string(PermissionAgentsStart))
	if !ok || normalized != PermissionAgentsStart {
		t.Fatalf("normalized Agent start permission = %v", normalized)
	}
}

func TestSessionReadPermissionAllowsReadOnlyRoles(t *testing.T) {
	for _, role := range []Role{RoleOwner, RoleAdmin, RoleDeveloper, RoleViewer} {
		if !RoleAllows(role, PermissionSessionsRead) {
			t.Fatalf("%s should be allowed to read Sessions", role)
		}
	}
	normalized, ok := ParseAPIKeyGrant(string(PermissionSessionsRead))
	if !ok || normalized != PermissionSessionsRead {
		t.Fatalf("normalized Session read permission = %v", normalized)
	}
}

func TestAPIKeyGrantRejectsUnknownPermission(t *testing.T) {
	for _, grant := range []string{"", "*", "unsupported.permission"} {
		if permission, ok := ParseAPIKeyGrant(grant); ok {
			t.Fatalf("unknown grant %q accepted as %q", grant, permission)
		}
	}
}
