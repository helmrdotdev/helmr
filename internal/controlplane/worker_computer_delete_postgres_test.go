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
)

func TestWorkerDeleteComputerReplaysAfterTombstone(t *testing.T) {
	fixture := runtest.New(t)
	work := fixture.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	expiresAt := time.Now().Add(10 * time.Minute).UTC()
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE run_leases
   SET status = 'running', started_at = now(), expires_at = $2
 WHERE id = $1`, work.LeaseID, expiresAt)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE computer_instances SET writer_expires_at = $2
 WHERE id = (SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID, expiresAt)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE runs
   SET status = 'running', started_at = now(), active_started_at = now()
 WHERE id = $1`, work.RunID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE run_attempts SET entrypoint_entered_at = now()
 WHERE run_id = $1 AND number = 1`, work.RunID)

	computerID := uuid.NewV7()
	versionID := uuid.NewV7()
	tx, err := fixture.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `
INSERT INTO computers (
    id, environment_id, region_id, sandbox_declared_id, key, head_disk_version_id
, computer_spec_id, creation_deployment_id) VALUES ($1, $2, $3, 'test-computer', 'worker-delete-replay', $5, (SELECT computer_spec_id FROM deployment_definitions WHERE environment_id=$2 AND id=$4), (SELECT deployment_id FROM deployment_definitions WHERE environment_id=$2 AND id=$4))`,
		computerID, fixture.EnvironmentID, runtest.Region,
		fixture.ComputerDefinitionID, versionID)
	dbtest.InsertCommittedComputerRoot(t, t.Context(), tx, versionID, fixture.EnvironmentID, computerID)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	var workerClaimVersion, groupClaimVersion int64
	if err := fixture.Pool.QueryRow(t.Context(), `
SELECT worker_hosts.claim_version, worker_groups.claim_version
  FROM worker_hosts
  JOIN worker_groups ON worker_groups.id = worker_hosts.worker_group_id
 WHERE worker_hosts.id = $1`, fixture.WorkerID).Scan(
		&workerClaimVersion, &groupClaimVersion,
	); err != nil {
		t.Fatal(err)
	}
	worker := workerActor{
		WorkerHostID: fixture.WorkerID, WorkerGroupID: runtest.WorkerGroupID,
		WorkerEpoch: 1, ClaimVersion: workerClaimVersion, GroupClaimVersion: groupClaimVersion,
	}
	request := workerapi.DeleteComputerRequest{
		RetrieveComputerRequest: workerapi.RetrieveComputerRequest{
			Lease:         workerapi.RunLeaseFence{ID: work.LeaseID.String(), LeaseSequence: 1},
			CorrelationID: uuid.NewV7().String(),
			Computer:      workerapi.ComputerAddress{ComputerID: computerID.String()},
		},
		IdempotencyKey: "worker-delete-replay",
	}
	server := &Server{
		db: db.New(fixture.Pool), tx: fixture.Pool,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	invoke := func() workerapi.DeleteComputerResponse {
		t.Helper()
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		httpRequest := httptest.NewRequest(
			http.MethodPost, "/worker/v1/run/computers/delete", bytes.NewReader(body),
		)
		httpRequest = httpRequest.WithContext(context.WithValue(
			httpRequest.Context(), workerContextKey{}, worker,
		))
		response := httptest.NewRecorder()
		server.workerDeleteComputer(response, httpRequest)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
		}
		var decoded workerapi.DeleteComputerResponse
		if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	first := invoke()
	if first.Completed == nil || first.Completed.ComputerID != computerID.String() ||
		first.Failed != nil {
		t.Fatalf("first response = %+v", first)
	}
	finalized, err := server.db.FinalizeDeletingComputers(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalized) != 1 || finalized[0].Bytes != computerID {
		t.Fatalf("finalized = %+v, want %s", finalized, computerID)
	}
	replayed := invoke()
	if replayed.Completed == nil || replayed.Completed.ComputerID != computerID.String() ||
		replayed.Failed != nil {
		t.Fatalf("replayed response = %+v", replayed)
	}
}
