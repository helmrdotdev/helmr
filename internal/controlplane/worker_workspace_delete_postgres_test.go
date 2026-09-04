package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func TestWorkerDeleteWorkspaceReplaysAfterTombstone(t *testing.T) {
	fixture := runtest.New(t)
	work := fixture.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	expiresAt := time.Now().Add(10 * time.Minute).UTC()
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE run_leases
   SET state = 'running', started_at = now(), expires_at = $2
 WHERE id = $1`, work.LeaseID, expiresAt)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE workspace_leases SET expires_at = $2
 WHERE owner_run_lease_id = $1`, work.LeaseID, expiresAt)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE runs
   SET status = 'running', started_at = now(), active_started_at = now()
 WHERE id = $1`, work.RunID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE run_attempts SET entrypoint_entered_at = now()
 WHERE run_id = $1 AND number = 1`, work.RunID)

	workspaceID := uuid.NewV7()
	versionID := uuid.NewV7()
	tx, err := fixture.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `
INSERT INTO workspaces (
    id, environment_id, region_id, sandbox_declared_id,
    deployment_definition_id, key, head_version_id
) VALUES ($1, $2, $3, 'test-workspace', $4, 'worker-delete-replay', $5)`,
		workspaceID, fixture.EnvironmentID, runtest.Region,
		fixture.WorkspaceDefinitionID, versionID)
	dbtest.MustExec(t, t.Context(), tx, `
INSERT INTO workspace_versions (
    id, environment_id, workspace_id, kind, content_digest, state,
    ownership_generation, writer_generation, published_at
) VALUES ($1, $2, $3, 'system', $4, 'committed', 0, 0, now())`,
		versionID, fixture.EnvironmentID, workspaceID, workspace.CanonicalEmptyTreeDigest)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	var workerClaimVersion, groupClaimVersion int64
	if err := fixture.Pool.QueryRow(t.Context(), `
SELECT worker_instances.claim_version, worker_groups.claim_version
  FROM worker_instances
  JOIN worker_groups ON worker_groups.id = worker_instances.worker_group_id
 WHERE worker_instances.id = $1`, fixture.WorkerID).Scan(
		&workerClaimVersion, &groupClaimVersion,
	); err != nil {
		t.Fatal(err)
	}
	worker := workerActor{
		WorkerInstanceID: fixture.WorkerID, WorkerGroupID: runtest.WorkerGroupID,
		WorkerEpoch: 1, ClaimVersion: workerClaimVersion, GroupClaimVersion: groupClaimVersion,
	}
	request := workerapi.DeleteWorkspaceRequest{
		RetrieveWorkspaceRequest: workerapi.RetrieveWorkspaceRequest{
			Lease:         workerapi.RunLeaseFence{ID: work.LeaseID.String(), LeaseSequence: 1},
			CorrelationID: uuid.NewV7().String(),
			Workspace:     workerapi.WorkspaceAddress{WorkspaceID: workspaceID.String()},
		},
		IdempotencyKey: "worker-delete-replay",
	}
	server := &Server{
		db: db.New(fixture.Pool), tx: fixture.Pool,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	invoke := func() workerapi.DeleteWorkspaceResponse {
		t.Helper()
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		httpRequest := httptest.NewRequest(
			http.MethodPost, "/worker/v1/run/workspaces/delete", bytes.NewReader(body),
		)
		httpRequest = httpRequest.WithContext(context.WithValue(
			httpRequest.Context(), workerContextKey{}, worker,
		))
		response := httptest.NewRecorder()
		server.workerDeleteWorkspace(response, httpRequest)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
		}
		var decoded workerapi.DeleteWorkspaceResponse
		if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	first := invoke()
	if first.Completed == nil || first.Completed.WorkspaceID != workspaceID.String() ||
		first.Failed != nil {
		t.Fatalf("first response = %+v", first)
	}
	finalized, err := server.db.FinalizeDeletingWorkspaces(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalized) != 1 || finalized[0].Bytes != workspaceID {
		t.Fatalf("finalized = %+v, want %s", finalized, workspaceID)
	}
	replayed := invoke()
	if replayed.Completed == nil || replayed.Completed.WorkspaceID != workspaceID.String() ||
		replayed.Failed != nil {
		t.Fatalf("replayed response = %+v", replayed)
	}
}
