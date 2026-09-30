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
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func checkpointFailureFixture(t *testing.T) (runtest.Fixture, runtest.RunLease, workerapi.CheckpointFailedRequest, workergroup.HostPrincipal) {
	t.Helper()
	f := runtest.New(t)
	run := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='waiting' WHERE id=$1`, run.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_attempts SET entrypoint_entered_at=now() WHERE run_id=$1`, run.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO run_waits(id,environment_id,run_id,computer_id,kind,due_at,expected_run_revision,attempt_number,current_run_lease_id) SELECT $2,environment_id,id,computer_id,'timer',now()+interval '1 hour',revision,1,$3 FROM runs WHERE id=$1`, run.RunID, uuid.NewV7(), run.LeaseID)
	begin := db.BeginComputerCheckpointParams{CheckpointID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(f.EnvironmentID)}
	if err := f.Pool.QueryRow(t.Context(), `SELECT i.id,i.writer_generation,i.membership_revision,i.desired_version FROM computer_instances i JOIN run_leases l ON l.computer_instance_id=i.id WHERE l.id=$1`, run.LeaseID).Scan(&begin.ComputerInstanceID, &begin.WriterGeneration, &begin.MembershipRevision, &begin.DesiredVersion); err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = dispatch.BeginComputerCapture(t.Context(), tx, begin); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return f, run, workerapi.CheckpointFailedRequest{ComputerInstanceID: pgvalue.UUIDString(begin.ComputerInstanceID), WorkerEpoch: 1, DesiredVersion: begin.DesiredVersion + 1, CheckpointID: pgvalue.UUIDString(begin.CheckpointID), Error: "snapshot failed"}, workergroup.HostPrincipal{HostID: f.WorkerID, GroupID: runtest.WorkerGroupID, Epoch: 1}
}

func callCheckpointFailure(t *testing.T, f runtest.Fixture, receipt workerapi.CheckpointFailedRequest, worker workergroup.HostPrincipal) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/computer/checkpoints/failed", bytes.NewReader(body))
	request = request.WithContext(context.WithValue(t.Context(), workerContextKey{}, worker))
	response := httptest.NewRecorder()
	server := &Server{db: db.New(f.Pool), tx: f.Pool, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	server.workerMarkCheckpointFailed(response, request)
	return response
}

func TestWorkerCheckpointFailureRequestsSourceExclusion(t *testing.T) {
	f, run, request, worker := checkpointFailureFixture(t)
	for range 2 {
		response := callCheckpointFailure(t, f, request, worker)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var receipt workerapi.ComputerCheckpointResponse
		if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.ComputerInstanceID != request.ComputerInstanceID || receipt.WorkerEpoch != request.WorkerEpoch || receipt.DesiredVersion != request.DesiredVersion || receipt.CheckpointID != request.CheckpointID {
			t.Fatalf("receipt=%+v", receipt)
		}
	}
	var preserved bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.status='waiting' AND l.process_reconciled_at IS NULL AND i.reclaimed_at IS NULL AND i.desired_state='closed' AND i.desired_version=$2 FROM runs r JOIN run_leases l ON l.id=r.current_run_lease_id JOIN computer_instances i ON i.id=l.computer_instance_id WHERE r.id=$1`, run.RunID, request.DesiredVersion+1).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("source retained=%v err=%v", preserved, err)
	}
	request.Error = "different failure"
	if response := callCheckpointFailure(t, f, request, worker); response.Code != http.StatusConflict {
		t.Fatalf("changed replay=%d %s", response.Code, response.Body.String())
	}
}

func TestWorkerCheckpointFailureRejectsInvalidSource(t *testing.T) {
	f, _, request, worker := checkpointFailureFixture(t)
	wrongWorker := worker
	wrongWorker.HostID = uuid.NewV7()
	if response := callCheckpointFailure(t, f, request, wrongWorker); response.Code != http.StatusConflict {
		t.Fatalf("wrong host=%d %s", response.Code, response.Body.String())
	}
	request.Error = " "
	if response := callCheckpointFailure(t, f, request, worker); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid error=%d %s", response.Code, response.Body.String())
	}
}
