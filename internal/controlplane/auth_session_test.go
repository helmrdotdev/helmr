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
	p.Grants = []auth.PermissionGrant{{Permission: auth.CapabilityAccessManage}, {Permission: auth.CapabilityKeysManage, ProjectID: uuid.NewV7().String(), EnvironmentID: uuid.NewV7().String()}}
	got := sessionPermissions(p)
	if !slices.Contains(got, "access.manage") || !slices.Contains(got, string(auth.PermissionMembersManage)) || slices.Contains(got, "keys.manage") || slices.Contains(got, "organization.manage") {
		t.Fatalf("incorrect organization projection: %v", got)
	}
}
