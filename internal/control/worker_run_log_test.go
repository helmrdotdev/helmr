package control

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
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
)

type workerLogReplayStore struct {
	db.Querier
	replayMatches bool
}

func (s workerLogReplayStore) GetStartingRunLease(context.Context, db.GetStartingRunLeaseParams) (db.RunLease, error) {
	return db.RunLease{TaskAttemptNumber: 1}, nil
}

func (s workerLogReplayStore) GetCurrentRunningRunLease(context.Context, db.GetCurrentRunningRunLeaseParams) (db.RunLease, error) {
	return db.RunLease{TaskAttemptNumber: 1}, nil
}

func (s workerLogReplayStore) AppendRunLogChunk(context.Context, db.AppendRunLogChunkParams) (db.AppendRunLogChunkRow, error) {
	return db.AppendRunLogChunkRow{ReplayMatches: s.replayMatches}, nil
}

func TestWorkerAppendLogsReturnsConflictForChangedReplay(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	lease := api.WorkerRunLease{
		ID: uuid.Must(uuid.NewV7()).String(), OrgID: uuid.Must(uuid.NewV7()).String(),
		RunID: uuid.Must(uuid.NewV7()).String(), WorkerGroupID: "worker-group",
		WorkerInstanceID: workerID.String(), WorkerEpoch: 1, LeaseSequence: 1,
		SnapshotVersion: 1, RuntimeInstanceID: uuid.Must(uuid.NewV7()).String(),
		NetworkSlotID: uuid.Must(uuid.NewV7()).String(), NetworkSlotGeneration: 1,
		ProtocolVersion: api.CurrentWorkerProtocolVersion, AttemptNumber: 1,
	}
	body, err := json.Marshal(api.WorkerAppendLogRequest{
		Lease: lease, Stream: api.WorkerLogStreamStdout, ObservedSeq: 1,
		ContentBase64: "YWxwaGE=",
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		db:  workerLogReplayStore{replayMatches: false},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodPost, "/api/worker/leases/logs", bytes.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, workerActor{
		WorkerInstanceID: workerID, WorkerGroupID: lease.WorkerGroupID, WorkerEpoch: lease.WorkerEpoch,
		ProtocolVersion: lease.ProtocolVersion,
	}))
	recorder := httptest.NewRecorder()

	server.workerAppendLogs(recorder, request)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want conflict", recorder.Code, recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("different content")) {
		t.Fatalf("body=%s, want typed replay conflict", recorder.Body.String())
	}
}

func TestWorkerAppendLogsAcceptsIdenticalReplay(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	lease := api.WorkerRunLease{
		ID: uuid.Must(uuid.NewV7()).String(), OrgID: uuid.Must(uuid.NewV7()).String(),
		RunID: uuid.Must(uuid.NewV7()).String(), WorkerGroupID: "worker-group",
		WorkerInstanceID: workerID.String(), WorkerEpoch: 1, LeaseSequence: 1,
		SnapshotVersion: 1, RuntimeInstanceID: uuid.Must(uuid.NewV7()).String(),
		NetworkSlotID: uuid.Must(uuid.NewV7()).String(), NetworkSlotGeneration: 1,
		ProtocolVersion: api.CurrentWorkerProtocolVersion, AttemptNumber: 1,
	}
	body, err := json.Marshal(api.WorkerAppendLogRequest{
		Lease: lease, Stream: api.WorkerLogStreamStdout, ObservedSeq: 1,
		ContentBase64: "YWxwaGE=",
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		db:  workerLogReplayStore{replayMatches: true},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodPost, "/api/worker/leases/logs", bytes.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, workerActor{
		WorkerInstanceID: workerID, WorkerGroupID: lease.WorkerGroupID, WorkerEpoch: lease.WorkerEpoch,
		ProtocolVersion: lease.ProtocolVersion,
	}))
	recorder := httptest.NewRecorder()

	server.workerAppendLogs(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want success", recorder.Code, recorder.Body.String())
	}
}

var _ db.Querier = workerLogReplayStore{}
