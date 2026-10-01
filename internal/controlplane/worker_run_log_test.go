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

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type workerLogReplayStore struct {
	db.Querier
	replayMatches bool
	authorization *db.AuthorizeWorkerHostCredentialRow
}

func (s workerLogReplayStore) AuthorizeWorkerHostCredential(_ context.Context, _ db.AuthorizeWorkerHostCredentialParams) (db.AuthorizeWorkerHostCredentialRow, error) {
	if s.authorization == nil {
		return db.AuthorizeWorkerHostCredentialRow{}, pgx.ErrNoRows
	}
	return *s.authorization, nil
}

func (workerLogReplayStore) GetRunLogChunkReplay(context.Context, db.GetRunLogChunkReplayParams) (db.GetRunLogChunkReplayRow, error) {
	return db.GetRunLogChunkReplayRow{}, pgx.ErrNoRows
}

func (s workerLogReplayStore) AppendRunLogChunk(context.Context, db.AppendRunLogChunkParams) (db.AppendRunLogChunkRow, error) {
	return db.AppendRunLogChunkRow{ReplayMatches: s.replayMatches}, nil
}

func TestMountedWorkerRunLogRouteAcceptsExactMaximumAndRejectsOneByteOver(t *testing.T) {
	workerID := uuid.NewV7()
	credentialID := uuid.NewV7()
	lease := validRunLeaseAssignment(workerID)
	now := time.Now().UTC()
	store := workerLogReplayStore{
		replayMatches: true,
		authorization: &db.AuthorizeWorkerHostCredentialRow{
			WorkerGroupID: pgvalue.UUID(uuid.MustParse(lease.WorkerGroupID)), WorkerHostID: pgvalue.UUID(workerID),
			ClaimVersion: 1, ResourceID: "test-resource", WorkerStatus: "active",
			EpochStartedAt: pgtype.Timestamptz{Time: now, Valid: true},
		},
	}
	cfg := completeServerConfig(t)
	cfg.DB = store
	router, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The store authorizes any credential, so this transport test signs the
	// token the exchange would issue for the lease's host and epoch.
	claims := rawWorkerJWTClaims(workerID.String(), lease.WorkerGroupID, credentialID.String())
	claims["worker_epoch"] = lease.WorkerEpoch
	token := signRawWorkerJWT(t, claims)

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
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, workergroup.HostPrincipal{
		HostID: workerID, GroupID: uuid.MustParse(lease.WorkerGroupID), Epoch: lease.WorkerEpoch,
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

var _ db.Querier = workerLogReplayStore{}
