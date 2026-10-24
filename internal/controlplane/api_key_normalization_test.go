package controlplane

import (
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
)

func TestNormalizeAPIKeyPermissionGrantsCanonicalizes(t *testing.T) {
	grants, permissions, err := normalizeAPIKeyPermissionGrants([]api.APIKeyPermissionGrant{
		{Scopes: []api.APIKeyScope{api.APIKeyScopeSessionsManage, api.APIKeyScopeSessionsView}},
		{Scopes: []api.APIKeyScope{api.APIKeyScopeSessionsView, api.APIKeyScopeAgentsUse}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantPermissions := []auth.Permission{auth.CapabilityAgentsUse, auth.CapabilitySessionsManage, auth.CapabilitySessionsView}
	if !reflect.DeepEqual(permissions, wantPermissions) {
		t.Fatalf("permissions = %v, want %v", permissions, wantPermissions)
	}
	wantGrants := []api.APIKeyPermissionGrant{{Scopes: []api.APIKeyScope{
		api.APIKeyScopeAgentsUse,
		api.APIKeyScopeSessionsManage,
		api.APIKeyScopeSessionsView,
	}}}
	if !reflect.DeepEqual(grants, wantGrants) {
		t.Fatalf("grants = %+v, want %+v", grants, wantGrants)
	}
}

func TestNormalizeAPIKeyPermissionGrantsRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name   string
		grants []api.APIKeyPermissionGrant
	}{
		{name: "empty"},
		{name: "empty grant", grants: []api.APIKeyPermissionGrant{{}}},
		{name: "unsupported", grants: []api.APIKeyPermissionGrant{{Scopes: []api.APIKeyScope{"unsupported"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := normalizeAPIKeyPermissionGrants(test.grants); err == nil {
				t.Fatal("normalizeAPIKeyPermissionGrants accepted invalid input")
			}
		})
	}
}

func TestNormalizeAPIKeyPermissionGrantsAllowsComputerRetentionUpdates(t *testing.T) {
	wantGrants := []api.APIKeyPermissionGrant{{Scopes: []api.APIKeyScope{api.APIKeyScopeComputersOperate}}}
	grants, permissions, err := normalizeAPIKeyPermissionGrants(wantGrants)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(grants, wantGrants) {
		t.Fatalf("grants = %+v, want %+v", grants, wantGrants)
	}
	wantPermissions := []auth.Permission{auth.CapabilityComputersOperate}
	if !reflect.DeepEqual(permissions, wantPermissions) {
		t.Fatalf("permissions = %v, want %v", permissions, wantPermissions)
	}
}
