package controlplane

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestActorStartHTTPPostgresCreatesAndReplaysIDs(t *testing.T) {
	h := newSessionHTTP(t, sessiontest.New(t, 1))
	body := fmt.Sprintf(
		`{"computer":{"id":%q},"idempotency_key":"http-start-1","run":{"ttl":"30m","retry":{"max_attempts":3}}}`,
		h.ComputerIDs[0],
	)
	token := h.apiKey(auth.Principal{
		OrgID: h.OrgID, Kind: auth.PrincipalKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: h.ProjectID.String(), EnvironmentID: h.EnvironmentID.String(),
		Permissions: []auth.Permission{auth.PermissionActorsStart},
	})
	var first api.StartActorResponse
	for attempt := range 2 {
		recorder := h.request(t, http.MethodPost, "/v1/actors/operator.v1/start", token, body)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("attempt %d status=%d body=%s", attempt, recorder.Code, recorder.Body.String())
		}
		var response api.StartActorResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if err := ids.Validate(response.SessionID); err != nil {
			t.Fatalf("Actor ID: %v", err)
		}
		if err := ids.Validate(response.RunID); err != nil {
			t.Fatalf("Run ID: %v", err)
		}
		if attempt == 0 {
			first = response
		} else if response != first {
			t.Fatalf("replay response = %+v, first = %+v", response, first)
		}
	}
	missing := h.request(t, http.MethodPost, "/v1/actors/missing.v1/start", token, fmt.Sprintf(`{"computer":{"id":%q}}`, h.ComputerIDs[0]))
	if missing.Code != http.StatusNotFound || decodeHTTPError(t, missing.Body.Bytes()).Code != "actor_not_deployed" {
		t.Fatalf("undeployed start = %d %s", missing.Code, missing.Body.String())
	}
}

func TestActorStartHTTPPostgresDeniesBeforeAdmission(t *testing.T) {
	h := newSessionHTTP(t, sessiontest.New(t, 1))
	token := h.apiKey(auth.Principal{
		OrgID: h.OrgID, Kind: auth.PrincipalKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: h.ProjectID.String(), EnvironmentID: h.EnvironmentID.String(),
	})
	recorder := h.request(t, http.MethodPost, "/v1/actors/operator.v1/start", token, fmt.Sprintf(`{"computer":{"id":%q}}`, h.ComputerIDs[0]))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var sessions int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("sessions = %d, want 0", sessions)
	}
}

func TestActorStartHTTPSessionPostgresCreates(t *testing.T) {
	h := newSessionHTTP(t, sessiontest.New(t, 1))
	token := h.memberSession(t, db.OrgMemberRoleDeveloper)
	recorder := h.request(t, http.MethodPost, h.environmentPath("/actors/operator.v1/start"), token, fmt.Sprintf(`{"computer":{"id":%q}}`, h.ComputerIDs[0]))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response api.StartActorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if err := ids.Validate(response.SessionID); err != nil {
		t.Fatalf("Actor ID: %v", err)
	}
}
