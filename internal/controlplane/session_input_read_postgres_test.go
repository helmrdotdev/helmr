package controlplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
)

func TestSessionInputReadPostgresPagesInSequenceOrder(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 2)
	firstKey, secondKey := "input:first", "input:empty"
	request := fixture.request(0, &firstKey, "input-first")
	request.InputPresent = true
	request.Input = json.RawMessage(`{"message":"hello"}`)
	first, err := fixture.server.startActor(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.InitialRecordID == nil {
		t.Fatalf("first = %+v", first)
	}
	second, err := fixture.server.startActor(t.Context(), fixture.request(1, &secondKey, "input-empty"))
	if err != nil {
		t.Fatal(err)
	}

	followUpID := uuid.NewV7()
	createdAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE sessions
		   SET next_input_sequence = 3
		 WHERE id = $1
	`, first.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		INSERT INTO session_records (
		    id, environment_id, session_id, direction, sequence, data,
		    content_type, source_kind, source_run_id, created_at
		) VALUES (
		    $1, $2, $3, 'input', 2, '{"message":"follow-up"}'::jsonb,
		    'application/json', 'run', $4, $5
		)
	`, followUpID, fixture.environmentID, first.SessionID, first.BootRunID, createdAt); err != nil {
		t.Fatal(err)
	}

	principal := auth.Actor{
		OrgID: fixture.orgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: fixture.projectID.String(), EnvironmentID: fixture.environmentID.String(),
		Permissions: []auth.Permission{auth.PermissionSessionsRead},
	}
	firstPage := readSessionInputPostgresHTTP(t, fixture, principal, first.SessionID.String(), "/?limit=1")
	if len(firstPage.Records) != 1 || firstPage.NextAfter != 1 || !firstPage.HasMore {
		t.Fatalf("first page = %+v", firstPage)
	}
	initial := firstPage.Records[0]
	if initial.ID != first.InitialRecordID.String() || initial.Sequence != 1 ||
		initial.Source != (api.SessionInputSource{Type: "external"}) ||
		!jsonEquivalent(initial.Data, `{"message":"hello"}`) ||
		initial.CreatedAt.IsZero() || initial.CreatedAt.Location() != time.UTC {
		t.Fatalf("initial record = %+v", initial)
	}

	nextPage := readSessionInputPostgresHTTP(t, fixture, principal, first.SessionID.String(), "/?after=1&limit=1")
	if len(nextPage.Records) != 1 || nextPage.NextAfter != 2 || nextPage.HasMore {
		t.Fatalf("next page = %+v", nextPage)
	}
	followUp := nextPage.Records[0]
	if followUp.ID != followUpID.String() || followUp.Sequence != 2 ||
		followUp.Source != (api.SessionInputSource{Type: "run", RunID: first.BootRunID.String()}) ||
		!jsonEquivalent(followUp.Data, `{"message":"follow-up"}`) ||
		!followUp.CreatedAt.Equal(createdAt) {
		t.Fatalf("follow-up record = %+v", followUp)
	}

	full := readSessionInputPostgresHTTP(t, fixture, principal, first.SessionID.String(), "/")
	if len(full.Records) != 2 || full.Records[0].Sequence != 1 || full.Records[1].Sequence != 2 ||
		full.NextAfter != 2 || full.HasMore {
		t.Fatalf("full page = %+v", full)
	}
	emptyPage := readSessionInputPostgresHTTP(t, fixture, principal, second.SessionID.String(), "/")
	if emptyPage.Records == nil || len(emptyPage.Records) != 0 || emptyPage.NextAfter != 0 || emptyPage.HasMore {
		t.Fatalf("empty page = %+v", emptyPage)
	}
	futurePage := readSessionInputPostgresHTTP(
		t, fixture, principal, first.SessionID.String(), "/?after=9007199254740991",
	)
	if len(futurePage.Records) != 0 || futurePage.NextAfter != maxSessionRecordSequence || futurePage.HasMore {
		t.Fatalf("future page = %+v", futurePage)
	}

	missingRecorder := httptest.NewRecorder()
	fixture.server.readSessionInputHTTP(
		missingRecorder, sessionReadPostgresRequest("/", uuid.NewV7().String(), principal),
	)
	if missingRecorder.Code != http.StatusNotFound ||
		!strings.Contains(missingRecorder.Body.String(), `"code":"session_not_found"`) {
		t.Fatalf("missing response = %d %s", missingRecorder.Code, missingRecorder.Body.String())
	}
	unauthorized := principal
	unauthorized.Permissions = []auth.Permission{auth.PermissionSessionsInputSend}
	forbiddenRecorder := httptest.NewRecorder()
	fixture.server.readSessionInputHTTP(
		forbiddenRecorder, sessionReadPostgresRequest("/", first.SessionID.String(), unauthorized),
	)
	if forbiddenRecorder.Code != http.StatusForbidden ||
		!strings.Contains(forbiddenRecorder.Body.String(), `"code":"permission_required"`) {
		t.Fatalf("forbidden response = %d %s", forbiddenRecorder.Code, forbiddenRecorder.Body.String())
	}
}

func jsonEquivalent(raw json.RawMessage, want string) bool {
	var got, expected bytes.Buffer
	if json.Compact(&got, raw) != nil || json.Compact(&expected, []byte(want)) != nil {
		return false
	}
	return bytes.Equal(got.Bytes(), expected.Bytes())
}

func readSessionInputPostgresHTTP(
	t *testing.T,
	fixture actorStartPostgresFixture,
	principal auth.Actor,
	sessionID string,
	target string,
) api.SessionInputPage {
	t.Helper()
	recorder := httptest.NewRecorder()
	fixture.server.readSessionInputHTTP(recorder, sessionReadPostgresRequest(target, sessionID, principal))
	if recorder.Code != http.StatusOK {
		t.Fatalf("Session input HTTP = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response api.SessionInputPage
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}
