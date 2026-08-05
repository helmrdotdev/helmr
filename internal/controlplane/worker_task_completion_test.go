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

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkerCompleteTaskReplaysPreviousEpochWithoutCAS(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	request := validTaskCompletionRequest(t)
	lease := validRunLeaseAssignment(workerID)
	lease.WorkerEpoch = 1
	request.Lease = lease.Fence()
	request.Workspace.Captured = validTaskWorkspaceCapture(t, lease)
	parsed, err := parseTaskCompletionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	store := &workerTaskCompletionReplayStore{fingerprint: pgvalue.Text(parsed.fingerprint)}
	server := &Server{log: taskCompletionTestLogger(), db: store}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost, "/api/worker/v0/run/tasks/complete", bytes.NewReader(body))
	httpRequest = httpRequest.WithContext(context.WithValue(httpRequest.Context(), workerContextKey{}, workerActor{
		WorkerInstanceID: workerID,
		WorkerGroupID:    lease.WorkerGroupID,
		WorkerEpoch:      2,
	}))
	response := httptest.NewRecorder()

	server.workerCompleteTask(response, httpRequest)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if store.replayCalls != 1 {
		t.Fatalf("replay lookups = %d", store.replayCalls)
	}
}

func TestWorkerCompleteTaskRejectsChangedTerminalRequest(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	request := validTaskCompletionRequest(t)
	lease := validRunLeaseAssignment(workerID)
	request.Lease = lease.Fence()
	request.Workspace.Captured = validTaskWorkspaceCapture(t, lease)
	store := &workerTaskCompletionReplayStore{fingerprint: pgvalue.Text("sha256:different")}
	server := &Server{log: taskCompletionTestLogger(), db: store}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost, "/api/worker/v0/run/tasks/complete", bytes.NewReader(body))
	httpRequest = httpRequest.WithContext(context.WithValue(httpRequest.Context(), workerContextKey{}, workerActor{
		WorkerInstanceID: workerID,
		WorkerGroupID:    lease.WorkerGroupID,
		WorkerEpoch:      lease.WorkerEpoch,
	}))
	response := httptest.NewRecorder()

	server.workerCompleteTask(response, httpRequest)

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestWorkerCompleteTaskRejectsUnknownFields(t *testing.T) {
	request := validTaskCompletionRequest(t)
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	body = append(bytes.TrimSuffix(body, []byte("}")), []byte(`,"unexpected":true}`)...)
	server := &Server{log: taskCompletionTestLogger(), db: &workerTaskCompletionReplayStore{}}
	httpRequest := httptest.NewRequest(
		http.MethodPost,
		"/api/worker/v0/run/tasks/complete",
		bytes.NewReader(body),
	)
	response := httptest.NewRecorder()

	server.workerCompleteTask(response, httpRequest)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

type workerTaskCompletionReplayStore struct {
	db.Querier
	fingerprint pgtype.Text
	replayCalls int
}

func (store *workerTaskCompletionReplayStore) GetTaskCompletionReplay(
	_ context.Context,
	_ db.GetTaskCompletionReplayParams,
) (pgtype.Text, error) {
	store.replayCalls++
	return store.fingerprint, nil
}

func taskCompletionTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
