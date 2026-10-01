package controlplane

import (
	"encoding/json"
	"net/http"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestComputerReadPostgresListsAttachedAndIdleComputers(t *testing.T) {
	fixture := sessiontest.New(t, 2)
	key := "owner:session"
	startSession(t, fixture, 0, &key, "owner-session")
	owned := fixture.ComputerIDs[0].String()
	free := fixture.ComputerIDs[1].String()
	handler := newPostgresServer(t, fixture.Pool)
	reader := issueEnvironmentAPIKey(t, fixture.Pool, fixture.OrgID, fixture.ProjectID, fixture.EnvironmentID, auth.PermissionComputersRead)

	listRecorder := serveAPIKey(handler, http.MethodGet, "/v1/computers", reader, "")
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list HTTP = %d body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var list api.ListComputersResponse
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Computers) != 2 {
		t.Fatalf("computers = %+v", list)
	}
	for _, item := range list.Computers {
		if item.ID != owned && item.ID != free {
			t.Fatalf("unexpected Computer %+v", item)
		}
	}
	byKey := serveAPIKey(handler, http.MethodGet, "/v1/computers?key="+fixture.ComputerKeys[1], reader, "")
	var keyed api.ListComputersResponse
	if err := json.Unmarshal(byKey.Body.Bytes(), &keyed); err != nil || byKey.Code != http.StatusOK || len(keyed.Computers) != 1 || keyed.Computers[0].ID != free {
		t.Fatalf("key lookup = %d %s", byKey.Code, byKey.Body.String())
	}

	for _, id := range []string{owned, free} {
		recorder := serveAPIKey(handler, http.MethodGet, "/v1/computers/"+id, reader, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("get %s HTTP = %d body=%s", id, recorder.Code, recorder.Body.String())
		}
		var snapshot api.ComputerSnapshot
		if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.ID != id || snapshot.Status != api.ComputerStatusAvailable || len(snapshot.Secrets) != 1 {
			t.Fatalf("snapshot %s = %+v", id, snapshot)
		}
	}
	missing := serveAPIKey(handler, http.MethodGet, "/v1/computers/"+uuid.NewV7().String(), reader, "")
	if body := decodeHTTPError(t, missing.Body.Bytes()); missing.Code != http.StatusNotFound || body.Code != "computer_not_found" {
		t.Fatalf("missing Computer = %d %+v", missing.Code, body)
	}
	malformed := serveAPIKey(handler, http.MethodGet, "/v1/computers/not-a-uuid", reader, "")
	if body := decodeHTTPError(t, malformed.Body.Bytes()); malformed.Code != http.StatusBadRequest || body.Code != "invalid_computer_reference" {
		t.Fatalf("malformed Computer = %d %+v", malformed.Code, body)
	}
}
