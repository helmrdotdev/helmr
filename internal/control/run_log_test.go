package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/jackc/pgx/v5"
)

func TestGetRunLogsUsesDurableRunIdentity(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	store := &fakeStore{
		run:    db.Run{ID: pgvalue.UUID(runID), OrgID: pgvalue.UUID(dbtest.DefaultOrgID), TaskID: "deploy", Status: db.RunStatusRunning, CreatedAt: testTime(), UpdatedAt: testTime()},
		stdout: []byte("hello\n"), stderr: []byte("warn\n"),
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})
	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+runID.String()+"/logs", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.LogSnapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.StdoutBase64 != base64.StdEncoding.EncodeToString(store.stdout) || response.StderrBase64 != base64.StdEncoding.EncodeToString(store.stderr) {
		t.Fatalf("logs = %+v", response)
	}
	if store.runLogSnapshot.OrgID != dbtest.DefaultOrgID || store.runLogSnapshot.RunID != runID {
		t.Fatalf("snapshot scope = %+v", store.runLogSnapshot)
	}
}

func TestGetRunLogsReportsTruncatedSnapshot(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	store := &fakeStore{
		run:    db.Run{ID: pgvalue.UUID(runID), OrgID: pgvalue.UUID(dbtest.DefaultOrgID), TaskID: "deploy", Status: db.RunStatusRunning, CreatedAt: testTime(), UpdatedAt: testTime()},
		stdout: []byte("hello\n"), logTruncated: true, logCursor: 42,
	}
	server := newTestServer(testServerConfig{Log: discardTestLogger(), DB: store, Auth: fakeAuth{}})
	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+runID.String()+"/logs", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	var response api.LogSnapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || !response.Truncated || response.Cursor != telemetryCursor(42) {
		t.Fatalf("status=%d response=%+v", rec.Code, response)
	}
}

func (f *fakeStore) ListRunLogChunksAfter(_ context.Context, arg telemetry.RunLogChunkQuery) ([]db.AppendRunLogChunkRow, error) {
	f.runLogChunksAfter = arg
	rows := make([]db.AppendRunLogChunkRow, 0, len(f.logChunks))
	for _, chunk := range f.logChunks {
		if chunk.Seq > arg.AfterSeq {
			rows = append(rows, chunk)
		}
	}
	return rows, nil
}

func (f *fakeStore) GetRunLogSnapshot(_ context.Context, arg telemetry.RunLogSnapshotQuery) (telemetry.RunLogSnapshot, error) {
	f.runLogSnapshot = arg
	if f.run.ID != pgvalue.UUID(arg.RunID) || (len(f.stdout) == 0 && len(f.stderr) == 0) {
		return telemetry.RunLogSnapshot{}, pgx.ErrNoRows
	}
	return telemetry.RunLogSnapshot{Stdout: f.stdout, Stderr: f.stderr, Truncated: f.logTruncated, Cursor: f.logCursor, UpdatedAt: pgvalue.Time(testTime())}, nil
}
