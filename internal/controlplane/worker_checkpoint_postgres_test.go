package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func TestCreateSameWorkspaceChildRunQueryMatchesGreenfieldSchema(t *testing.T) {
	fixture := runtest.New(t)
	_, err := db.New(fixture.Pool).CreateSameWorkspaceChildRunFromParentDeployment(
		t.Context(),
		db.CreateSameWorkspaceChildRunFromParentDeploymentParams{
			RunWaitID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
			EntrypointDeclaredID:   pgvalue.Text("test-task"),
			ClaimID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
			ParentRunLeaseID:       pgvalue.UUID(uuid.Must(uuid.NewV7())),
			SuspendCheckpointID:    pgvalue.UUID(uuid.Must(uuid.NewV7())),
			BaseWorkspaceVersionID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
			EnvironmentID:          pgvalue.UUID(fixture.EnvironmentID),
			ParentRunID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
			ParentAttemptNumber:    1,
			ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
			QueueName:              "default",
			QueueOriginAt:          pgvalue.Timestamptz(time.Now()),
			QueueScoreAt:           pgvalue.Timestamptz(time.Now()),
			MaxActiveDurationMs:    60_000,
			RetryPolicy:            []byte(`{"enabled":false}`),
			RootSpanID:             "1111111111111111",
		},
	)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("empty-authority query error = %v, want no rows", err)
	}
}

func TestWorkerCheckpointFailedRejectsInvalidPinnedRetryPolicyPermanently(t *testing.T) {
	fixture := runtest.New(t)
	work := fixture.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	waitID := uuid.Must(uuid.NewV7())
	checkpointID := uuid.Must(uuid.NewV7())
	resumeAttachID := uuid.Must(uuid.NewV7())
	var workspaceID, workspaceLeaseID, baseVersionID uuid.UUID
	if err := fixture.Pool.QueryRow(t.Context(), `
SELECT runs.workspace_id, workspace_leases.id, workspace_leases.base_version_id
  FROM runs
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = runs.current_run_lease_id
 WHERE runs.id = $1`, work.RunID).Scan(&workspaceID, &workspaceLeaseID, &baseVersionID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE run_leases
   SET state = 'checkpointing', started_at = claimed_at
 WHERE id = $1`, work.LeaseID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE runs
   SET status = 'waiting', state_version = 2,
       started_at = transaction_timestamp() - interval '10 seconds',
       active_started_at = transaction_timestamp() - interval '10 seconds',
       retry_policy = '{"enabled":true}'::jsonb
 WHERE id = $1`, work.RunID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind, due_at,
    expected_run_state_version, attempt_number, current_run_lease_id,
    checkpoint_request_version, resume_attach_id, suspension_state
) VALUES (
    $1, $2, $3, $4, 'timer', transaction_timestamp() + interval '1 hour',
    2, 1, $5, 1, $6, 'checkpointing'
)`, waitID, fixture.EnvironmentID, work.RunID, workspaceID, work.LeaseID, resumeAttachID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
INSERT INTO run_checkpoints (
    id, run_id, attempt_number, run_wait_id, source_run_lease_id,
    source_workspace_lease_id, workspace_id, base_workspace_version_id, state
) VALUES ($1, $2, 1, $3, $4, $5, $6, $7, 'creating')`,
		checkpointID, work.RunID, waitID, work.LeaseID, workspaceLeaseID, workspaceID, baseVersionID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE run_waits SET suspend_checkpoint_id = $2 WHERE id = $1`, waitID, checkpointID)

	requestBody, err := json.Marshal(workerapi.CheckpointFailedRequest{
		Lease:          workerapi.RunLeaseFence{ID: work.LeaseID.String(), LeaseSequence: 1},
		RequestVersion: 1, RunWaitID: waitID.String(), CheckpointID: checkpointID.String(),
		Error: "snapshot failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	var workerClaimVersion, groupClaimVersion int64
	if err := fixture.Pool.QueryRow(t.Context(), `
SELECT worker_instances.claim_version, worker_groups.claim_version
  FROM worker_instances
  JOIN worker_groups ON worker_groups.id = worker_instances.worker_group_id
 WHERE worker_instances.id = $1`, fixture.WorkerID).Scan(&workerClaimVersion, &groupClaimVersion); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/worker/v1/run/checkpoints/failed",
		bytes.NewReader(requestBody),
	)
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, workerActor{
		WorkerInstanceID: fixture.WorkerID, WorkerGroupID: runtest.WorkerGroup,
		WorkerEpoch: 1, ClaimVersion: workerClaimVersion, GroupClaimVersion: groupClaimVersion,
	}))
	response := httptest.NewRecorder()
	server := &Server{db: db.New(fixture.Pool), tx: fixture.Pool, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	server.workerMarkCheckpointFailed(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "unprocessable_entity" {
		t.Fatalf("error code = %q", envelope.Error.Code)
	}
}
