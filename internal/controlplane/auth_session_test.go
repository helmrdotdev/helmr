package controlplane

import (
	"github.com/helmrdotdev/helmr/internal/auth"
	"slices"
	"testing"
	"uuid"
)

func TestSessionPermissionsAreExplicitOrganizationGrants(t *testing.T) {
	p := auth.Principal{Kind: auth.PrincipalKindSession, OrgID: uuid.NewV7(), IsOwner: true}
	if got := sessionPermissions(p); len(got) != 0 {
		t.Fatalf("owner bypass: %v", got)
	}
	p.Grants = []auth.PermissionGrant{{Permission: auth.PermissionAssignmentsCreate}, {Permission: auth.PermissionAPIKeysCreate, ProjectID: uuid.NewV7().String(), EnvironmentID: uuid.NewV7().String()}}
	got := sessionPermissions(p)
	if !slices.Contains(got, "assignments.create") || slices.Contains(got, string(auth.PermissionMembersRead)) || slices.Contains(got, "api_keys.create") || slices.Contains(got, "projects.create") {
		t.Fatalf("incorrect organization projection: %v", got)
	}
}
