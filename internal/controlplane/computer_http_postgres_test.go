package controlplane

import (
	"encoding/json"
	"net/http"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestComputerCreateAndDeleteHTTPPostgresContract(t *testing.T) {
	f := sessiontest.New(t, 1)
	handler := newPostgresServer(t, f.Pool, func(cfg *ServerConfig) { cfg.SecretProxy = testComputerCAStore(t, f.Pool) })
	key := issueEnvironmentAPIKey(t, f.Pool, f.OrgID, f.ProjectID, f.EnvironmentID,
		auth.PermissionComputersCreate, auth.PermissionComputersDelete, auth.PermissionComputersRead)
	expect := func(method, path, body string, status int, code string) []byte {
		t.Helper()
		response := serveAPIKey(handler, method, path, key, body)
		if response.Code != status {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, response.Code, status, response.Body.String())
		}
		if code != "" {
			if got := decodeHTTPError(t, response.Body.Bytes()); got.Code != code {
				t.Fatalf("%s %s code = %q, want %q", method, path, got.Code, code)
			}
		}
		return response.Body.Bytes()
	}

	const create = "/v1/sandboxes/computer.v1/computers"
	body := `{"key":"http-created","secrets":[{"secret":"API_TOKEN","env":{"name":"TOKEN","mode":"protected","allowed_origins":["https://example.com"]}}],"idempotency_key":"http-create"}`
	var created api.ComputerSnapshot
	if err := json.Unmarshal(expect(http.MethodPost, create, body, http.StatusCreated, ""), &created); err != nil {
		t.Fatal(err)
	}
	if created.Status != api.ComputerStatusAvailable || created.Residency != "cold" || created.Key == nil || *created.Key != "http-created" || len(created.Secrets) != 1 {
		t.Fatalf("created = %+v", created)
	}
	var replayed api.ComputerSnapshot
	if err := json.Unmarshal(expect(http.MethodPost, create, body, http.StatusCreated, ""), &replayed); err != nil || replayed.ID != created.ID {
		t.Fatalf("replayed = %+v, %v", replayed, err)
	}
	expect(http.MethodPost, create, `{"key":"http-created","idempotency_key":"http-create"}`, http.StatusConflict, "idempotency_conflict")
	expect(http.MethodPost, create, `{"key":"http-created"}`, http.StatusConflict, "computer_key_conflict")
	expect(http.MethodPost, create, `{"key":" padded "}`, http.StatusBadRequest, "invalid_computer_create")
	expect(http.MethodPost, create, `{"secrets":[{"secret":"MISSING","env":{"name":"TOKEN","mode":"raw"}}]}`, http.StatusConflict, "secret_unavailable")
	expect(http.MethodPost, "/v1/sandboxes/absent.v1/computers", `{}`, http.StatusNotFound, "computer_not_deployed")

	deletePath := "/v1/computers/" + created.ID
	var receipt api.DeleteComputerReceipt
	if err := json.Unmarshal(expect(http.MethodDelete, deletePath, `{"idempotency_key":"http-delete"}`, http.StatusAccepted, ""), &receipt); err != nil || receipt.ComputerID != created.ID {
		t.Fatalf("delete receipt = %+v, %v", receipt, err)
	}
	if err := json.Unmarshal(expect(http.MethodDelete, deletePath, `{"idempotency_key":"http-delete"}`, http.StatusAccepted, ""), &receipt); err != nil || receipt.ComputerID != created.ID {
		t.Fatalf("delete replay = %+v, %v", receipt, err)
	}
	expect(http.MethodDelete, "/v1/computers/"+uuid.NewV7().String(), "", http.StatusNotFound, "computer_not_found")
	expect(http.MethodDelete, "/v1/computers/not-a-uuid", "", http.StatusBadRequest, "invalid_computer_reference")

	startSession(t, f, 0, nil, "http-delete-member")
	expect(http.MethodDelete, "/v1/computers/"+f.ComputerIDs[0].String(), "", http.StatusConflict, "computer_busy")
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE idempotency_claims SET receipt=NULL,receipt_expires_at=now()-interval '1 day',receipt_pruned_at=now() WHERE operation='computer.delete'`)
	expect(http.MethodDelete, deletePath, `{"idempotency_key":"http-delete"}`, http.StatusGone, "operation_expired")
}
