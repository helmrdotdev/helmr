package controlplane

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/api"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCommandCancelHTTPPendingAndTerminalReplay(t *testing.T) {
	f := newActorStartPostgresFixture(t, 1)
	principal := computerCommandHTTPPrincipal(f.orgID, f.projectID, f.environmentID)
	admitted := httptest.NewRecorder()
	f.server.executeComputerHTTP(admitted, computerCommandHTTPPostRequest(`{"command":["sleep","10"],"idempotency_key":"cancel-pending"}`, f.computerRefs[0], principal))
	if admitted.Code != http.StatusAccepted {
		t.Fatalf("admit=%d %s", admitted.Code, admitted.Body.String())
	}
	var command api.CommandReceipt
	if err := json.Unmarshal(admitted.Body.Bytes(), &command); err != nil {
		t.Fatal(err)
	}
	cancel := func() api.CommandCancelReceipt {
		t.Helper()
		request := computerCommandHTTPGetRequest(command.CommandID, principal)
		request.Method = http.MethodPost
		response := httptest.NewRecorder()
		f.server.cancelCommandHTTP(response, request)
		if response.Code != http.StatusAccepted {
			t.Fatalf("cancel=%d %s", response.Code, response.Body.String())
		}
		var receipt api.CommandCancelReceipt
		if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		return receipt
	}
	first := cancel()
	if first.ID == "" || first.TargetID != command.CommandID || first.Status != "accepted" {
		t.Fatalf("receipt=%+v", first)
	}
	var status string
	var revision int64
	var terminal, requested bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status,revision,terminal_at IS NOT NULL,cancel_requested_at IS NOT NULL FROM computer_commands WHERE id=$1`, command.CommandID).Scan(&status, &revision, &terminal, &requested); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || !terminal || !requested {
		t.Fatalf("pending cancel status=%s terminal=%v requested=%v", status, terminal, requested)
	}
	if replay := cancel(); replay != first {
		t.Fatalf("replay=%+v first=%+v", replay, first)
	}
	var after int64
	if err := f.pool.QueryRow(t.Context(), `SELECT revision FROM computer_commands WHERE id=$1`, command.CommandID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != revision {
		t.Fatalf("replay mutated revision %d -> %d", revision, after)
	}
	got := httptest.NewRecorder()
	f.server.getComputerCommandHTTP(got, computerCommandHTTPGetRequest(command.CommandID, principal))
	var info api.CommandInfo
	if err := json.Unmarshal(got.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if got.Code != http.StatusOK || info.Status != "cancelled" || info.Outcome == nil || info.Outcome.Kind != "cancelled" {
		t.Fatalf("retrieve=%d %+v", got.Code, info)
	}
}
