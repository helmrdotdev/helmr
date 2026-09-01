package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type workerLogReplayStore struct {
	db.Querier
	replayMatches bool
	called        *bool
	workerID      pgtype.UUID
	params        *db.AppendRunLogChunkParams
	replay        *db.GetRunLogChunkReplayRow
	authorization *db.AuthorizeWorkerInstanceCredentialRow
}

func (s workerLogReplayStore) AuthorizeWorkerInstanceCredential(_ context.Context, _ db.AuthorizeWorkerInstanceCredentialParams) (db.AuthorizeWorkerInstanceCredentialRow, error) {
	if s.authorization == nil {
		return db.AuthorizeWorkerInstanceCredentialRow{}, pgx.ErrNoRows
	}
	return *s.authorization, nil
}

func (s workerLogReplayStore) GetRunLogChunkReplay(_ context.Context, _ db.GetRunLogChunkReplayParams) (db.GetRunLogChunkReplayRow, error) {
	if s.replay == nil {
		return db.GetRunLogChunkReplayRow{}, pgx.ErrNoRows
	}
	return *s.replay, nil
}

func (s workerLogReplayStore) AppendRunLogChunk(_ context.Context, params db.AppendRunLogChunkParams) (db.AppendRunLogChunkRow, error) {
	if s.called != nil {
		*s.called = true
	}
	if s.params != nil {
		*s.params = params
	}
	if s.workerID.Valid && params.WorkerInstanceID != s.workerID {
		return db.AppendRunLogChunkRow{}, pgx.ErrNoRows
	}
	return db.AppendRunLogChunkRow{ReplayMatches: s.replayMatches}, nil
}

func TestMountedWorkerRunLogRouteAcceptsExactMaximumAndRejectsOneByteOver(t *testing.T) {
	workerID := uuid.NewV7()
	credentialID := uuid.NewV7()
	lease := validRunLeaseAssignment(workerID)
	now := time.Now().UTC()
	signingKey := []byte("01234567890123456789012345678901")
	store := workerLogReplayStore{
		replayMatches: true,
		authorization: &db.AuthorizeWorkerInstanceCredentialRow{
			WorkerGroupID: lease.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(workerID),
			ClaimVersion: 1, ResourceID: "test-resource", WorkerState: "active",
			EpochStartedAt: pgtype.Timestamptz{Time: now, Valid: true},
		},
	}
	server := &Server{
		db: store, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		workerTokenSigningKey: signingKey,
	}
	router := chi.NewRouter()
	server.mountWorkerRoutes(router)
	token, err := auth.IssueWorkerToken(signingKey, auth.WorkerClaims{
		WorkerGroupID: lease.WorkerGroupID, WorkerInstanceID: workerID.String(), CredentialID: credentialID.String(),
		WorkerEpoch: lease.WorkerEpoch, ClaimVersion: 1, GroupClaimVersion: 1,
		IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	requestBody := func(contentBytes int) []byte {
		t.Helper()
		body, err := json.Marshal(workerapi.RunLogAppendRequest{
			Lease:  workerapi.RunLeaseFence{ID: lease.ID, LeaseSequence: math.MaxInt64},
			Stream: workerapi.LogStreamStdout, ObservedSeq: math.MaxInt64,
			ContentBase64: base64.StdEncoding.EncodeToString(make([]byte, contentBytes)),
		})
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	request := func(body []byte) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/worker/v1/run/logs/append", bytes.NewReader(body))
		req.Header.Set("authorization", "Bearer "+token)
		return req
	}

	maximumBody := requestBody(telemetry.MaxRunLogContentBytes)
	if int64(len(maximumBody)) != workerRunLogRequestBodyLimit {
		t.Fatalf("maximum request body = %d bytes, route limit = %d", len(maximumBody), workerRunLogRequestBodyLimit)
	}
	maximumRecorder := httptest.NewRecorder()
	router.ServeHTTP(maximumRecorder, request(maximumBody))
	if maximumRecorder.Code != http.StatusNoContent {
		t.Fatalf("maximum status=%d body=%s, want success", maximumRecorder.Code, maximumRecorder.Body.String())
	}

	overRecorder := httptest.NewRecorder()
	router.ServeHTTP(overRecorder, request(requestBody(telemetry.MaxRunLogContentBytes+1)))
	if overRecorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-limit status=%d body=%s, want request entity too large", overRecorder.Code, overRecorder.Body.String())
	}
}

func TestWorkerAppendLogsReturnsConflictForChangedReplay(t *testing.T) {
	workerID := uuid.NewV7()
	lease := validRunLeaseAssignment(workerID)
	body, err := json.Marshal(workerapi.RunLogAppendRequest{
		Lease: lease.Fence(), Stream: workerapi.LogStreamStdout, ObservedSeq: 1,
		ContentBase64: "YWxwaGE=",
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		db:  workerLogReplayStore{replayMatches: false},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/run/logs/append", bytes.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, workerActor{
		WorkerInstanceID: workerID, WorkerGroupID: lease.WorkerGroupID, WorkerEpoch: lease.WorkerEpoch,
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
	workerID := uuid.NewV7()
	lease := validRunLeaseAssignment(workerID)
	body, err := json.Marshal(workerapi.RunLogAppendRequest{
		Lease: lease.Fence(), Stream: workerapi.LogStreamStdout, ObservedSeq: 1,
		ContentBase64: "YWxwaGE=",
	})
	if err != nil {
		t.Fatal(err)
	}
	var params db.AppendRunLogChunkParams
	server := &Server{
		db:  workerLogReplayStore{replayMatches: true, params: &params},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/run/logs/append", bytes.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, workerActor{
		WorkerInstanceID: workerID, WorkerGroupID: lease.WorkerGroupID, WorkerEpoch: lease.WorkerEpoch,
	}))
	recorder := httptest.NewRecorder()

	server.workerAppendRunLogs(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s, want success", recorder.Code, recorder.Body.String())
	}
	if pgvalue.UUIDString(params.RunLeaseID) != lease.ID ||
		params.LeaseSequence != lease.LeaseSequence ||
		params.WorkerGroupID != lease.WorkerGroupID ||
		pgvalue.UUIDString(params.WorkerInstanceID) != lease.WorkerInstanceID ||
		params.WorkerEpoch != lease.WorkerEpoch {
		t.Fatalf("database receipt params = %+v", params)
	}
}

func TestWorkerAppendLogsReplaysAfterLeaseIsNoLongerLive(t *testing.T) {
	workerID := uuid.NewV7()
	lease := validRunLeaseAssignment(workerID)
	body, err := json.Marshal(workerapi.RunLogAppendRequest{
		Lease: lease.Fence(), Stream: workerapi.LogStreamStdout, ObservedSeq: 1,
		ContentBase64: "YWxwaGE=",
	})
	if err != nil {
		t.Fatal(err)
	}
	leaseFenceFingerprint, err := runLeaseFenceFingerprint(lease.Fence())
	if err != nil {
		t.Fatal(err)
	}
	called := false
	server := &Server{
		db: workerLogReplayStore{
			called: &called,
			replay: &db.GetRunLogChunkReplayRow{
				RunID:                 pgvalue.UUID(uuid.MustParse(lease.RunID)),
				RunLeaseID:            pgvalue.UUID(uuid.MustParse(lease.ID)),
				AttemptNumber:         pgtype.Int4{Int32: lease.AttemptNumber, Valid: true},
				Stream:                string(workerapi.LogStreamStdout),
				ObservedSeq:           pgtype.Int8{Int64: 1, Valid: true},
				Content:               []byte("alpha"),
				SizeBytes:             pgtype.Int8{Int64: 5, Valid: true},
				EventPayload:          `{"bytes":5,"observed_seq":1,"stream":"stdout"}`,
				LeaseFenceFingerprint: leaseFenceFingerprint,
			},
		},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/run/logs/append", bytes.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, workerActor{
		WorkerInstanceID: workerID, WorkerGroupID: lease.WorkerGroupID,
		WorkerEpoch: lease.WorkerEpoch}))
	recorder := httptest.NewRecorder()

	server.workerAppendRunLogs(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s, want success", recorder.Code, recorder.Body.String())
	}
	if called {
		t.Fatal("live lease append was called for a completed replay")
	}
}

func TestWorkerAppendLogsRejectsAnotherWorkersFence(t *testing.T) {
	workerID := uuid.NewV7()
	lease := validRunLeaseAssignment(workerID)
	body, err := json.Marshal(workerapi.RunLogAppendRequest{
		Lease: lease.Fence(), Stream: workerapi.LogStreamStdout, ObservedSeq: 1,
		ContentBase64: "YWxwaGE=",
	})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	server := &Server{
		db: workerLogReplayStore{
			replayMatches: true, called: &called,
			workerID: pgvalue.UUID(workerID),
		},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/run/logs/append", bytes.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, workerActor{
		WorkerInstanceID: uuid.NewV7(),
		WorkerGroupID:    lease.WorkerGroupID,
		WorkerEpoch:      lease.WorkerEpoch,
	}))
	recorder := httptest.NewRecorder()

	server.workerAppendRunLogs(recorder, request)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want conflict", recorder.Code, recorder.Body.String())
	}
	if !called {
		t.Fatal("database authority was not consulted for another worker's fence")
	}
}

var _ db.Querier = workerLogReplayStore{}
