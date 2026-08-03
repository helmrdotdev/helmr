package controlplane

import (
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/auth"
)

func TestPermissionsFromAPIKeyKeepsGrantablePermissions(t *testing.T) {
	permissions, err := permissionsFromAPIKey([]byte(`[
		{"permission":" actors.read "},
		{"permission":"members.manage"},
		{"permission":"unknown"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	want := []auth.Permission{auth.PermissionActorsRead}
	if !reflect.DeepEqual(permissions, want) {
		t.Fatalf("permissions = %v, want %v", permissions, want)
	}
}

func TestPermissionsFromAPIKeyRejectsUnsupportedPayloadType(t *testing.T) {
	if _, err := permissionsFromAPIKey(42); err == nil {
		t.Fatal("expected unsupported grant payload type to fail")
	}
}
