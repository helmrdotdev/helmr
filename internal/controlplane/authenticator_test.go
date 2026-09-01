package controlplane

import (
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/auth"
)

func TestPermissionsFromAPIKeyKeepsGrantablePermissions(t *testing.T) {
	permissions := permissionsFromAPIKey([]string{" sessions.read ", "members.manage", "unknown"})
	want := []auth.Permission{auth.PermissionSessionsRead}
	if !reflect.DeepEqual(permissions, want) {
		t.Fatalf("permissions = %v, want %v", permissions, want)
	}
}

func TestPermissionsFromAPIKeyReturnsNilWithoutKnownPermission(t *testing.T) {
	if permissions := permissionsFromAPIKey([]string{"members.manage", "unknown"}); permissions != nil {
		t.Fatalf("permissions = %v, want nil", permissions)
	}
}
