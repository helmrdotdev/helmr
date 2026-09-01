package controlplane

import (
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestNormalizeAPIKeyPermissionGrantsCanonicalizes(t *testing.T) {
	grants, permissions, err := normalizeAPIKeyPermissionGrants([]api.APIKeyPermissionGrant{
		{Scopes: []api.APIKeyScope{api.APIKeyScopeTokensRead, api.APIKeyScopeRunsRead}},
		{Scopes: []api.APIKeyScope{api.APIKeyScopeRunsRead, api.APIKeyScopeActorsStart}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantPermissions := []string{"actors.start", "runs.read", "tokens.read"}
	if !reflect.DeepEqual(permissions, wantPermissions) {
		t.Fatalf("permissions = %v, want %v", permissions, wantPermissions)
	}
	wantGrants := []api.APIKeyPermissionGrant{{Scopes: []api.APIKeyScope{
		api.APIKeyScopeActorsStart,
		api.APIKeyScopeRunsRead,
		api.APIKeyScopeTokensRead,
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
