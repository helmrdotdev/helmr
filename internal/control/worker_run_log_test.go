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
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type workerLogReplayStore struct {
	db.Querier
	replayMatches bool
	called        *bool
	params        *db.AppendReceiptRunLogChunkParams
	replay        *db.GetRunLogChunkReplayRow
}

func (s workerLogReplayStore) GetRunLogChunkReplay(_ context.Context, _ db.GetRunLogChunkReplayParams) (db.GetRunLogChunkReplayRow, error) {
	if s.replay == nil {
		return db.GetRunLogChunkReplayRow{}, pgx.ErrNoRows
	}
	return *s.replay, nil
}

func (s workerLogReplayStore) AppendReceiptRunLogChunk(_ context.Context, params db.AppendReceiptRunLogChunkParams) (db.AppendReceiptRunLogChunkRow, error) {
	if s.called != nil {
		*s.called = true
	}
	if s.params != nil {
		*s.params = params
	}
	return db.AppendReceiptRunLogChunkRow{ReplayMatches: s.replayMatches}, nil
}

func TestWorkerAppendLogsReturnsConflictForChangedReplay(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	lease := validRunLeaseReceipt(workerID)
	body, err := json.Marshal(api.WorkerRunLogAppendRequest{
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
	request := httptest.NewRequest(http.MethodPost, "/api/worker/leases/run-logs", bytes.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, workerActor{
		WorkerInstanceID: workerID, WorkerGroupID: lease.WorkerGroupID, WorkerEpoch: lease.WorkerEpoch,
		ProtocolVersion: lease.WorkerProtocolVersion,
	}))
	recorder := httptest.NewRecorder()

	server.workerAppendRunLogs(recorder, request)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want conflict", recorder.Code, recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("different content")) {
		t.Fatalf("body=%s, want typed replay conflict", recorder.Body.String())
	}
}

func TestWorkerAppendLogsAcceptsIdenticalReplay(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	lease := validRunLeaseReceipt(workerID)
	body, err := json.Marshal(api.WorkerRunLogAppendRequest{
		Lease: lease, Stream: api.WorkerLogStreamStdout, ObservedSeq: 1,
		ContentBase64: "YWxwaGE=",
	})
	if err != nil {
		t.Fatal(err)
	}
	var params db.AppendReceiptRunLogChunkParams
	server := &Server{
		db:  workerLogReplayStore{replayMatches: true, params: &params},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodPost, "/api/worker/leases/run-logs", bytes.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, workerActor{
		WorkerInstanceID: workerID, WorkerGroupID: lease.WorkerGroupID, WorkerEpoch: lease.WorkerEpoch,
		ProtocolVersion: lease.WorkerProtocolVersion,
	}))
	recorder := httptest.NewRecorder()

	server.workerAppendRunLogs(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s, want success", recorder.Code, recorder.Body.String())
	}
	if pgvalue.UUIDString(params.RunLeaseID) != lease.ID ||
		pgvalue.UUIDString(params.RunID) != lease.RunID ||
		pgvalue.UUIDString(params.WorkspaceMountID) != lease.WorkspaceMountID ||
		pgvalue.UUIDString(params.WorkspaceLeaseID) != lease.WorkspaceLeaseID ||
		pgvalue.UUIDString(params.BaseWorkspaceVersionID) != lease.BaseWorkspaceVersionID ||
		params.WriterGeneration != lease.WriterGeneration ||
		params.RequestedMemoryBytes != lease.RequestedMemoryBytes ||
		params.TraceID.String != lease.Trace.TraceID ||
		!params.StartDeadlineAt.Time.Equal(lease.StartDeadlineAt) ||
		!params.ExpiresAt.Time.Equal(lease.ExpiresAt) {
		t.Fatalf("database receipt params = %+v", params)
	}
}

func TestWorkerAppendLogsReplaysAfterLeaseIsNoLongerLive(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	lease := validRunLeaseReceipt(workerID)
	body, err := json.Marshal(api.WorkerRunLogAppendRequest{
		Lease: lease, Stream: api.WorkerLogStreamStdout, ObservedSeq: 1,
		ContentBase64: "YWxwaGE=",
	})
	if err != nil {
		t.Fatal(err)
	}
	receiptFingerprint, err := runLeaseReceiptFingerprint(lease)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	server := &Server{
		db: workerLogReplayStore{
			called: &called,
			replay: &db.GetRunLogChunkReplayRow{
				RunID:              pgvalue.UUID(uuid.MustParse(lease.RunID)),
				RunLeaseID:         pgvalue.UUID(uuid.MustParse(lease.ID)),
				AttemptNumber:      pgtype.Int4{Int32: lease.AttemptNumber, Valid: true},
				Stream:             string(api.WorkerLogStreamStdout),
				ObservedSeq:        pgtype.Int8{Int64: 1, Valid: true},
				Content:            []byte("alpha"),
				SizeBytes:          pgtype.Int8{Int64: 5, Valid: true},
				EventPayload:       `{"bytes":5,"observed_seq":1,"run_id":"` + lease.RunID + `","stream":"stdout"}`,
				ReceiptFingerprint: receiptFingerprint,
			},
		},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodPost, "/api/worker/leases/run-logs", bytes.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, workerActor{
		WorkerInstanceID: workerID, WorkerGroupID: lease.WorkerGroupID,
		WorkerEpoch: lease.WorkerEpoch, ProtocolVersion: lease.WorkerProtocolVersion,
	}))
	recorder := httptest.NewRecorder()

	server.workerAppendRunLogs(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s, want success", recorder.Code, recorder.Body.String())
	}
	if called {
		t.Fatal("live lease append was called for a completed replay")
	}
}

func TestWorkerAppendLogsRejectsAnotherWorkersReceipt(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	lease := validRunLeaseReceipt(workerID)
	body, err := json.Marshal(api.WorkerRunLogAppendRequest{
		Lease: lease, Stream: api.WorkerLogStreamStdout, ObservedSeq: 1,
		ContentBase64: "YWxwaGE=",
	})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	server := &Server{
		db:  workerLogReplayStore{replayMatches: true, called: &called},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodPost, "/api/worker/leases/run-logs", bytes.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, workerActor{
		WorkerInstanceID: uuid.Must(uuid.NewV7()),
		WorkerGroupID:    lease.WorkerGroupID,
		WorkerEpoch:      lease.WorkerEpoch,
		ProtocolVersion:  lease.WorkerProtocolVersion,
	}))
	recorder := httptest.NewRecorder()

	server.workerAppendRunLogs(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want forbidden", recorder.Code, recorder.Body.String())
	}
	if called {
		t.Fatal("database was called for another worker's receipt")
	}
}

var _ db.Querier = workerLogReplayStore{}
