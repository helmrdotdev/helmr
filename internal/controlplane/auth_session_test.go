package controlplane

import (
	"slices"
	"testing"

	"github.com/helmrdotdev/helmr/internal/auth"
)

func TestSessionPermissionsAdvertiseEveryRoleGrant(t *testing.T) {
	var owner []string
	for _, permission := range auth.AllPermissions() {
		if auth.RoleAllows(auth.RoleOwner, permission) {
			owner = append(owner, string(permission))
		}
	}
	if len(owner) != len(auth.AllPermissions()) {
		t.Fatalf("owner is denied a permission: %v", owner)
	}
	if got := sessionPermissions(auth.RoleOwner); !slices.Equal(got, owner) {
		t.Fatalf("owner permissions = %v, want %v", got, owner)
	}
	for _, permission := range []string{
		"tokens.create", "tokens.read", "tokens.complete", "tokens.cancel",
		"sessions.read", "sessions.input.send", "sessions.close", "actors.start",
	} {
		if !slices.Contains(owner, permission) {
			t.Fatalf("owner permissions omit %s: %v", permission, owner)
		}
	}
	if got := sessionPermissions(auth.RoleViewer); !slices.Equal(got, []string{
		"runs.read", "sessions.read", "tokens.read", "workspaces.read",
	}) {
		t.Fatalf("viewer permissions = %v", got)
	}
	if got := sessionPermissions(auth.Role("unknown")); len(got) != 0 {
		t.Fatalf("unknown role permissions = %v", got)
	}
}
