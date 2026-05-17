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
			ProjectID:     DefaultProjectID,
			EnvironmentID: DefaultEnvironmentID,
			Permissions:   []Permission{PermissionRunsRead, PermissionSecretsWrite},
		}},
	}

	if !actor.HasPermission(PermissionRunsRead, DefaultScope(orgID)) {
		t.Fatal("viewer-backed api key should keep read grants")
	}
	if actor.HasPermission(PermissionSecretsWrite, DefaultScope(orgID)) {
		t.Fatal("viewer-backed api key should not keep write grants after demotion")
	}
}
