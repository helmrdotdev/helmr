package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A Session whose Computer can no longer be locked for admission is a
// missing Session to public callers and an internal failure to the worker
// executing it.
func TestSessionComputerAuthorityStatusByCaller(t *testing.T) {
	f := newActorExecution(t, nil, true)
	web := f.httpPostgresFixture
	handler := f.handler
	ownerID := web.user(t, "Owner")
	web.member(t, f.OrgID, ownerID, db.OrgMemberRoleOwner)
	owner := web.session(t, ownerID, f.OrgID)
	workerCredential := f.workerCredential
	var generation int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT run_generation FROM sessions WHERE id=$1`, f.SessionID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	writeOutput := func(t *testing.T) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(workerapi.WriteSessionOutputRequest{
			Lease: f.fence(), CorrelationID: uuid.NewV7().String(), RunGeneration: generation, Data: json.RawMessage(`{"ok":true}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/worker/v1/run/sessions/output/write", strings.NewReader(string(body)))
		request.Header.Set("Authorization", "Bearer "+workerCredential)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	expectError := func(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		var body api.HTTPErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("status %d body %s: %v", response.Code, response.Body.String(), err)
		}
		if response.Code != status || body.Error.Code != code {
			t.Fatalf("response = %d %s, want %d %s", response.Code, response.Body.String(), status, code)
		}
	}

	if response := writeOutput(t); response.Code != http.StatusOK {
		t.Fatalf("Session output with Computer authority = %d %s", response.Code, response.Body.String())
	}
	// A private head disk version withdraws the Computer's admission
	// authority while the Session and its execution remain.
	privateHead(t, f.Pool, f.ComputerID)

	expectError(t, writeOutput(t), http.StatusInternalServerError, "internal_error")

	base := fmt.Sprintf("/api/projects/%s/environments/%s/sessions/%s", f.ProjectID, f.EnvironmentID, f.SessionID)
	if response := web.request(t, http.MethodGet, base, owner, ""); response.Code != http.StatusOK {
		t.Fatalf("Session read = %d %s", response.Code, response.Body.String())
	}
	expectError(t, web.request(t, http.MethodPost, base+"/close", owner, `{}`), http.StatusNotFound, "session_not_found")
	var status string
	if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM sessions WHERE id=$1`, f.SessionID).Scan(&status); err != nil || status != "open" {
		t.Fatalf("rejected close changed the Session: %q %v", status, err)
	}
}

// privateHead moves the Computer's head to a private child of its current
// head, so the Computer can no longer be locked for admission.
func privateHead(t *testing.T, pool *pgxpool.Pool, computerID uuid.UUID) {
	t.Helper()
	head := uuid.NewV7()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_disk_versions(id,environment_id,computer_id,parent_version_id,root_pack_digest,logical_bytes,status,writer_generation,source_computer_instance_id)
 SELECT $2,v.environment_id,v.computer_id,v.id,v.root_pack_digest,v.logical_bytes,'private',i.writer_generation,i.id FROM computers c JOIN computer_disk_versions v ON v.id=c.head_disk_version_id JOIN computer_instances i ON i.computer_id=c.id AND i.reclaimed_at IS NULL WHERE c.id=$1`, computerID, head)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_disk_version_roots(environment_id,computer_id,version_id,locator) SELECT r.environment_id,r.computer_id,$2,r.locator FROM computers c JOIN computer_disk_version_roots r ON r.version_id=c.head_disk_version_id WHERE c.id=$1`, computerID, head)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computers SET head_disk_version_id=$2 WHERE id=$1`, computerID, head)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}
