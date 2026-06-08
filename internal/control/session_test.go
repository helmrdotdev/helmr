package control

import (
	"slices"
	"testing"

	"github.com/helmrdotdev/helmr/internal/auth"
)

func TestSessionPermissionsAdvertiseRunsManage(t *testing.T) {
	developerPermissions := sessionPermissions(auth.RoleDeveloper)
	if !slices.Contains(developerPermissions, string(auth.PermissionRunsManage)) {
		t.Fatalf("developer permissions = %+v, want %s", developerPermissions, auth.PermissionRunsManage)
	}
	viewerPermissions := sessionPermissions(auth.RoleViewer)
	if slices.Contains(viewerPermissions, string(auth.PermissionRunsManage)) {
		t.Fatalf("viewer permissions = %+v, did not expect %s", viewerPermissions, auth.PermissionRunsManage)
	}
}
