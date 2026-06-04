package control

import (
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
)

func TestWaitpointTimeoutRequiresDelayTimeout(t *testing.T) {
	if _, err := waitpointTimeout(db.WaitpointKindDelay, nil); err == nil {
		t.Fatal("delay timeout validation succeeded without timeout")
	}
}

func TestWaitpointTimeoutAllowsNonDelayWithoutTimeout(t *testing.T) {
	timeout, err := waitpointTimeout(db.WaitpointKindHuman, nil)
	if err != nil {
		t.Fatal(err)
	}
	if timeout.Valid {
		t.Fatalf("timeout = %+v, want invalid", timeout)
	}
}

func TestWaitpointTimeoutRejectsNonPositiveTimeout(t *testing.T) {
	zero := int32(0)
	if _, err := waitpointTimeout(db.WaitpointKindHuman, &zero); err == nil {
		t.Fatal("timeout validation succeeded with zero")
	}
}

func TestWaitpointTimeoutAcceptsPositiveTimeout(t *testing.T) {
	seconds := int32(30)
	timeout, err := waitpointTimeout(db.WaitpointKindDelay, &seconds)
	if err != nil {
		t.Fatal(err)
	}
	if !timeout.Valid || timeout.Int32 != seconds {
		t.Fatalf("timeout = %+v, want %d", timeout, seconds)
	}
}

func TestWaitpointRequestLinkedIDAllowsArbitraryJSON(t *testing.T) {
	for _, request := range []string{`[1,2,3]`, `"hello"`, `42`, `{"waitpoint_id":123}`} {
		id, ok, err := waitpointRequestLinkedID(db.WaitpointKindHuman, []byte(request))
		if err != nil {
			t.Fatalf("request %s error = %v", request, err)
		}
		if ok || id != uuid.Nil {
			t.Fatalf("request %s linked id = %s, ok = %v", request, id, ok)
		}
	}
}

func TestWaitpointRequestLinkedIDExtractsStringID(t *testing.T) {
	waitpointID := ids.New()
	id, ok, err := waitpointRequestLinkedID(db.WaitpointKindHuman, []byte(`{"waitpoint_id":"`+waitpointID.String()+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !ok || id != waitpointID {
		t.Fatalf("linked id = %s, ok = %v", id, ok)
	}
}

func TestInferAPIKeyWaitpointScopeUsesWaitpointGrant(t *testing.T) {
	projectID := ids.New().String()
	environmentID := ids.New().String()
	scope, err := inferAPIKeyPermissionScope(auth.Actor{
		OrgID: ids.DefaultOrgID,
		Permissions: []auth.PermissionGrant{{
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			Permissions:   []auth.Permission{auth.PermissionWaitpointsRespond},
		}},
	}, auth.PermissionWaitpointsRespond, "waitpoint creation")
	if err != nil {
		t.Fatal(err)
	}
	if scope.ProjectID != projectID || scope.EnvironmentID != environmentID {
		t.Fatalf("scope = %+v", scope)
	}
}
