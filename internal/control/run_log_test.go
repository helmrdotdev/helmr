package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
)

func TestGetRunLogs(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	store := &fakeStore{
		run: db.Run{
			ID:        pgvalue.UUID(runID),
			OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
			TaskID:    "deploy",
			Status:    db.RunStatusRunning,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
		stdout: []byte("hello\n"),
		stderr: []byte("warn\n"),
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
	if response.StdoutBase64 != base64.StdEncoding.EncodeToString([]byte("hello\n")) || response.StderrBase64 != base64.StdEncoding.EncodeToString([]byte("warn\n")) {
		t.Fatalf("logs = %+v", response)
	}
	if store.runLogSnapshot.StdoutLimit != maxRunLogSnapshotBytes || store.runLogSnapshot.StderrLimit != maxRunLogSnapshotBytes {
		t.Fatalf("log snapshot params = %+v", store.runLogSnapshot)
	}
}

func TestGetRunLogsReportsTruncatedSnapshot(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	store := &fakeStore{
		run: db.Run{
			ID:        pgvalue.UUID(runID),
			OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
			TaskID:    "deploy",
			Status:    db.RunStatusRunning,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
		stdout:       []byte("hello\n"),
		logTruncated: true,
		logCursor:    42,
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
	if !response.Truncated {
		t.Fatalf("logs = %+v", response)
	}
	if response.Cursor != "42" {
		t.Fatalf("cursor = %q", response.Cursor)
	}
}

func TestFollowRunLogsStreamsChunksAfterCursor(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	sessionID := uuid.Must(uuid.NewV7())
	store := &fakeStore{
		run: db.Run{
			ID:        pgvalue.UUID(runID),
			OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
			TaskID:    "deploy",
			Status:    db.RunStatusSucceeded,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
		logChunks: []db.RunLogChunk{
			{
				OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
				RunID:         pgvalue.UUID(runID),
				RunLeaseID:    pgvalue.UUID(sessionID),
				AttemptNumber: 1,
				Stream:        db.RunLogStreamStdout,
				Seq:           8,
				ObservedSeq:   2,
				Content:       []byte("new\n"),
				SizeBytes:     4,
				CreatedAt:     testTime(),
			},
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})

	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+runID.String()+"/logs?follow=1&cursor=1", nil)
	req.Header.Set("authorization", "Bearer test-key")
	req.Header.Set("accept", "text/event-stream")
	req.Header.Set("Last-Event-ID", "7")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.firstRunLogChunksAfterSeq != 7 {
		t.Fatalf("log cursor = %d", store.firstRunLogChunksAfterSeq)
	}
	if !strings.Contains(rec.Body.String(), "event: run_log") || !strings.Contains(rec.Body.String(), "id: 8") {
		t.Fatalf("sse body = %q", rec.Body.String())
	}
	var chunk api.RunLogChunk
	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if chunk.ID != "8" || chunk.Stream != "stdout" || chunk.ContentBase64 != base64.StdEncoding.EncodeToString([]byte("new\n")) {
		t.Fatalf("chunk = %+v", chunk)
	}
}

func TestFollowRunLogsDrainsAfterTerminalStatus(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	sessionID := uuid.Must(uuid.NewV7())
	store := &fakeStore{
		run: db.Run{
			ID:        pgvalue.UUID(runID),
			OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
			TaskID:    "deploy",
			Status:    db.RunStatusSucceeded,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
		deferLogChunksUntilSecondList: true,
		logChunks: []db.RunLogChunk{
			{
				OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
				RunID:         pgvalue.UUID(runID),
				RunLeaseID:    pgvalue.UUID(sessionID),
				AttemptNumber: 1,
				Stream:        db.RunLogStreamStderr,
				Seq:           12,
				ObservedSeq:   4,
				Content:       []byte("final error\n"),
				SizeBytes:     int64(len("final error\n")),
				CreatedAt:     testTime(),
			},
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})

	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+runID.String()+"/logs?follow=1&cursor=11", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.runLogChunksAfterCalls != 2 {
		t.Fatalf("list calls = %d, want terminal drain", store.runLogChunksAfterCalls)
	}
	if !strings.Contains(rec.Body.String(), "id: 12") || !strings.Contains(rec.Body.String(), base64.StdEncoding.EncodeToString([]byte("final error\n"))) {
		t.Fatalf("sse body = %q", rec.Body.String())
	}
}

func TestWorkerLogsAndEvents(t *testing.T) {
	store := &fakeStore{
		run: db.Run{
			ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:              pgvalue.UUID(dbtest.DefaultOrgID),
			TaskID:             "deploy",
			Status:             db.RunStatusQueued,
			Payload:            []byte(`{}`),
			MaxDurationSeconds: 3600,
			CreatedAt:          testTime(),
			UpdatedAt:          testTime(),
		},
	}
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	eventStream := &EventStream{log: slog.New(slog.NewTextHandler(io.Discard, nil)), db: store, redis: redisClient}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}, WorkerTokenSecret: []byte("01234567890123456789012345678901"), WorkerTokenTTL: time.Hour, EventStream: eventStream})
	workerBearer := mintTestWorkerToken(t, server, "00000000-0000-0000-0000-000000000401")
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/lease", bytes.NewReader(testWorkerRunLeaseRequestBody(t)))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("claim status = %d body=%s", rec.Code, rec.Body.String())
	}
	var claimResponse api.WorkerRunLeaseResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &claimResponse); err != nil {
		t.Fatal(err)
	}
	logBody, err := json.Marshal(api.WorkerAppendLogRequest{
		Lease:         *claimResponse.Lease,
		Stream:        api.WorkerLogStreamStdout,
		ContentBase64: "aGVsbG8K",
	})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/worker/leases/logs", bytes.NewReader(logBody))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logs status = %d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/runs/"+pgvalue.MustUUIDValue(store.run.ID).String()+"/events", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("events status = %d body=%s", rec.Code, rec.Body.String())
	}
	var events api.RunEventPage
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatal(err)
	}
	if len(events.Events) != 1 || events.Events[0].Message != "log.stdout" {
		t.Fatalf("events = %+v", events)
	}
	if string(events.Events[0].Attributes) != `{"redacted":true}` || events.Events[0].RedactionClass != "sensitive" {
		t.Fatalf("log event redaction = class %q attributes %s", events.Events[0].RedactionClass, events.Events[0].Attributes)
	}
	if events.NextCursor != nil {
		t.Fatalf("next cursor = %v, want nil for final page", *events.NextCursor)
	}

	store.events = []db.Event{{
		Seq:            1,
		OrgID:          store.run.OrgID,
		RunID:          store.run.ID,
		Kind:           "run.completed",
		Payload:        []byte(`{"secret":"do-not-stream"}`),
		RedactionClass: "sensitive",
		CreatedAt:      testTime(),
	}}
	req = httptest.NewRequest(http.MethodGet, "/api/runs/"+pgvalue.MustUUIDValue(store.run.ID).String()+"/events?follow=1", nil)
	req.Header.Set("authorization", "Bearer test-key")
	req.Header.Set("accept", "text/event-stream")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("follow events status = %d body=%s", rec.Code, rec.Body.String())
	}
	var followed api.RunEvent
	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			if err := json.Unmarshal([]byte(data), &followed); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if followed.ID == "" {
		t.Fatalf("follow event body = %q", rec.Body.String())
	}
	if string(followed.Attributes) != `{"redacted":true}` || followed.RedactionClass != "sensitive" {
		t.Fatalf("follow event redaction = class %q attributes %s", followed.RedactionClass, followed.Attributes)
	}
}

func (f *fakeStore) AppendRunLogChunk(_ context.Context, arg db.AppendRunLogChunkParams) (db.AppendRunLogChunkRow, error) {
	if f.sessionID != arg.RunLeaseID || f.executionWorkerInstanceID != arg.WorkerInstanceID {
		return db.AppendRunLogChunkRow{}, pgx.ErrNoRows
	}
	switch arg.Stream {
	case "stdout":
		f.stdout = append(f.stdout, arg.Content...)
	case "stderr":
		f.stderr = append(f.stderr, arg.Content...)
	}
	event := db.Event{
		Seq:            int64(len(f.events) + 1),
		OrgID:          arg.OrgID,
		RunID:          arg.RunID,
		RunLeaseID:     arg.RunLeaseID,
		AttemptNumber:  pgtype.Int4{Int32: 1, Valid: true},
		Kind:           arg.Kind,
		Payload:        arg.Payload,
		RedactionClass: "sensitive",
		CreatedAt:      testTime(),
	}
	f.events = append(f.events, event)
	return db.AppendRunLogChunkRow{
		RunID:         arg.RunID,
		RunLeaseID:    arg.RunLeaseID,
		AttemptNumber: 1,
		Stream:        arg.Stream,
		Seq:           int64(len(f.events)),
		ObservedSeq:   arg.ObservedSeq,
		Content:       arg.Content,
		CreatedAt:     testTime(),
	}, nil
}

func (f *fakeStore) ListRunLogChunksAfter(_ context.Context, arg db.ListRunLogChunksAfterParams) ([]db.RunLogChunk, error) {
	f.runLogChunksAfter = arg
	f.runLogChunksAfterCalls++
	if f.runLogChunksAfterCalls == 1 {
		f.firstRunLogChunksAfterSeq = arg.Seq
	}
	if f.deferLogChunksUntilSecondList && f.runLogChunksAfterCalls == 1 {
		return nil, nil
	}
	rows := make([]db.RunLogChunk, 0, len(f.logChunks))
	for _, chunk := range f.logChunks {
		if chunk.Seq <= arg.Seq {
			continue
		}
		rows = append(rows, chunk)
		if len(rows) == int(arg.RowLimit) {
			break
		}
	}
	return rows, nil
}

func (f *fakeStore) GetRunLogSnapshot(_ context.Context, arg db.GetRunLogSnapshotParams) (db.GetRunLogSnapshotRow, error) {
	f.runLogSnapshot = arg
	if f.run.ID != arg.RunID || (len(f.stdout) == 0 && len(f.stderr) == 0) {
		return db.GetRunLogSnapshotRow{}, pgx.ErrNoRows
	}
	return db.GetRunLogSnapshotRow{
		RunID:     arg.RunID,
		Stdout:    f.stdout,
		Stderr:    f.stderr,
		Truncated: pgtype.Bool{Bool: f.logTruncated, Valid: true},
		Cursor:    f.logCursor,
		UpdatedAt: testTime(),
	}, nil
}
