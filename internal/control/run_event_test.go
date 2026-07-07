package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
)

func TestTerminalRunEventDoesNotTrustWorkerFailureKind(t *testing.T) {
	message := "worker failed"
	kind := "source_unavailable"
	eventKind, payload, err := terminalRunEventForFields(db.RunStatusFailed, pgtype.Int4{}, pgtype.Text{String: message, Valid: true}, api.WorkerReleaseResult{Kind: "failed", FailureKind: &kind})
	if err != nil {
		t.Fatal(err)
	}
	if eventKind != "run.failed" {
		t.Fatalf("event kind = %s", eventKind)
	}
	assertJSONBytes(t, payload, `{"detail":{"message":"worker failed"},"failure_kind":"worker_failed"}`)
	var eventPayload struct {
		FailureKind string `json:"failure_kind"`
	}
	if err := json.Unmarshal(payload, &eventPayload); err != nil {
		t.Fatal(err)
	}
	if eventPayload.FailureKind != "worker_failed" {
		t.Fatalf("failure kind = %s", eventPayload.FailureKind)
	}
}

func TestTerminalRunEventPreservesMaxDurationFailureKind(t *testing.T) {
	message := "runtime max_duration exceeded after 30s active time"
	kind := "max_duration"
	limitSeconds := int32(30)
	eventKind, payload, err := terminalRunEventForFields(db.RunStatusFailed, pgtype.Int4{}, pgtype.Text{String: message, Valid: true}, api.WorkerReleaseResult{Kind: "failed", FailureKind: &kind, LimitSeconds: &limitSeconds})
	if err != nil {
		t.Fatal(err)
	}
	if eventKind != "run.failed" {
		t.Fatalf("event kind = %s", eventKind)
	}
	assertJSONBytes(t, payload, `{"detail":{"limit_seconds":30,"message":"runtime max_duration exceeded after 30s active time"},"failure_kind":"max_duration"}`)
	var eventPayload struct {
		FailureKind string `json:"failure_kind"`
		Detail      struct {
			Message      string `json:"message"`
			LimitSeconds int32  `json:"limit_seconds"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(payload, &eventPayload); err != nil {
		t.Fatal(err)
	}
	if eventPayload.FailureKind != "max_duration" {
		t.Fatalf("failure kind = %s", eventPayload.FailureKind)
	}
	if eventPayload.Detail.Message != message || eventPayload.Detail.LimitSeconds != 30 {
		t.Fatalf("detail = %+v", eventPayload.Detail)
	}
}

func TestTerminalRunEventPreservesTaskParseFailureKind(t *testing.T) {
	message := "task not found: deploy"
	kind := "task_not_found"
	eventKind, payload, err := terminalRunEventForFields(db.RunStatusFailed, pgtype.Int4{}, pgtype.Text{String: message, Valid: true}, api.WorkerReleaseResult{Kind: "failed", FailureKind: &kind})
	if err != nil {
		t.Fatal(err)
	}
	if eventKind != "run.failed" {
		t.Fatalf("event kind = %s", eventKind)
	}
	assertJSONBytes(t, payload, `{"detail":{"message":"task not found: deploy"},"failure_kind":"task_not_found"}`)
	var eventPayload struct {
		FailureKind string `json:"failure_kind"`
		Detail      struct {
			Message string `json:"message"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(payload, &eventPayload); err != nil {
		t.Fatal(err)
	}
	if eventPayload.FailureKind != "task_not_found" || eventPayload.Detail.Message != message {
		t.Fatalf("payload = %+v", eventPayload)
	}
}

func TestTerminalRunEventJSONShapes(t *testing.T) {
	tests := []struct {
		name        string
		status      db.RunStatus
		exitCode    pgtype.Int4
		message     pgtype.Text
		result      api.WorkerReleaseResult
		wantKind    string
		wantPayload string
	}{
		{
			name:        "completed",
			status:      db.RunStatusSucceeded,
			exitCode:    pgtype.Int4{Int32: 0, Valid: true},
			wantKind:    "run.completed",
			wantPayload: `{"exit_code":0}`,
		},
		{
			name:        "task failed",
			status:      db.RunStatusFailed,
			exitCode:    pgtype.Int4{Int32: 2, Valid: true},
			wantKind:    "run.failed",
			wantPayload: `{"detail":{"exit_code":2},"failure_kind":"task_failed"}`,
		},
		{
			name:        "cancelled",
			status:      db.RunStatusCancelled,
			message:     pgtype.Text{String: "operator cancelled", Valid: true},
			wantKind:    "run.cancelled",
			wantPayload: `{"reason":"operator cancelled"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eventKind, payload, err := terminalRunEventForFields(tt.status, tt.exitCode, tt.message, tt.result)
			if err != nil {
				t.Fatal(err)
			}
			if eventKind != tt.wantKind {
				t.Fatalf("event kind = %s, want %s", eventKind, tt.wantKind)
			}
			assertJSONBytes(t, payload, tt.wantPayload)
		})
	}
}

func TestWorkerEventPayloadJSONShapes(t *testing.T) {
	payload, err := runCreatedEventPayload("deploy", json.RawMessage(`{"env":"prod"}`), 300, []string{"TOKEN", "API_KEY"}, []byte(`{"enabled":false}`), []byte("{}"), []string{}, "initial", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	assertJSONBytes(t, payload, `{"cause":{},"max_duration_seconds":300,"metadata":{},"payload":{"env":"prod"},"reason":"initial","retry_policy":{"enabled":false},"secret_names":["API_KEY","TOKEN"],"tags":[],"task_id":"deploy"}`)

	payload, err = json.Marshal(workerLogChunkPayload{
		RunID:       "run-1",
		Stream:      api.WorkerLogStreamStdout,
		ObservedSeq: 7,
		Bytes:       12,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertJSONBytes(t, payload, `{"bytes":12,"observed_seq":7,"run_id":"run-1","stream":"stdout"}`)

	params := workerInstanceHeartbeatParams(workerActor{WorkerInstanceID: uuid.Must(uuid.NewV7()), WorkerGroupID: "us-east-1-worker-group-1", ResourceID: "worker-resource"}, api.WorkerCapabilities{
		ProtocolVersion: api.CurrentWorkerProtocolVersion,
		RuntimeID:       "sha256:runtime",
		RuntimeArch:     "arm64",
		RuntimeABI:      "helmr/v1",
		KernelDigest:    "sha256:kernel",
		InitramfsDigest: "sha256:initramfs",
		RootfsDigest:    "sha256:rootfs",
		CNIProfile:      "helmr/v0",
	})
	assertJSONBytes(t, params.Heartbeat, `{"cni_profile":"helmr/v0","initramfs_digest":"sha256:initramfs","kernel_digest":"sha256:kernel","rootfs_digest":"sha256:rootfs","runtime_abi":"helmr/v1","runtime_arch":"arm64","runtime_id":"sha256:runtime"}`)
}

func TestRunEventsPaginationUsesLookahead(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	store := &fakeStore{
		run: db.Run{
			ID:        pgvalue.UUID(runID),
			OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
			TaskID:    "deploy",
			Status:    db.RunStatusQueued,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
	}
	for i := int64(1); i <= 201; i++ {
		store.events = append(store.events, db.ClaimLiveTelemetryOutboxRow{
			Seq:       i,
			OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
			RunID:     pgvalue.UUID(runID),
			Kind:      "run.created",
			Payload:   []byte(`{}`),
			CreatedAt: testTime(),
		})
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})

	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+runID.String()+"/events?limit=2", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("events status = %d body=%s", rec.Code, rec.Body.String())
	}
	var limited api.RunEventPage
	if err := json.Unmarshal(rec.Body.Bytes(), &limited); err != nil {
		t.Fatal(err)
	}
	if len(limited.Events) != 2 || limited.NextCursor == nil || *limited.NextCursor != telemetryCursor(2) {
		t.Fatalf("limited page len=%d next=%v", len(limited.Events), limited.NextCursor)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/runs/"+runID.String()+"/events", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("events status = %d body=%s", rec.Code, rec.Body.String())
	}
	var first api.RunEventPage
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 200 || first.NextCursor == nil || *first.NextCursor != telemetryCursor(200) {
		t.Fatalf("first page len=%d next=%v", len(first.Events), first.NextCursor)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/runs/"+runID.String()+"/events?cursor="+telemetryCursor(200), nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("events status = %d body=%s", rec.Code, rec.Body.String())
	}
	var second api.RunEventPage
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 1 || second.Events[0].ID != telemetryCursor(201) || second.NextCursor != nil {
		t.Fatalf("second page = %+v", second)
	}
}

func TestEventCursorPrefersLastEventID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/runs/run-1/events?cursor="+telemetryCursor(4), nil)
	req.Header.Set("Last-Event-ID", telemetryCursor(9))

	cursor, err := eventCursor(req)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != 9 {
		t.Fatalf("cursor = %d, want 9", cursor)
	}
}

func TestEventStreamTreatsTrimmedOlderDuplicateAsPublished(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	streamKey := eventStreamKey(dbtest.DefaultOrgID, "us-east-1-worker-group-1", eventSubjectTypeRun, runID)
	if err := redisClient.XAdd(context.Background(), &redis.XAddArgs{
		Stream: streamKey,
		ID:     "2-0",
		Values: map[string]any{"event": `{"id":"2","kind":"run.completed"}`},
	}).Err(); err != nil {
		t.Fatal(err)
	}
	stream := &EventStream{log: slog.New(slog.NewTextHandler(io.Discard, nil)), db: &fakeStore{}, redis: redisClient}
	err := stream.publishOutboxRow(context.Background(), db.ClaimLiveTelemetryOutboxRow{
		OutboxID:       1,
		StreamKind:     db.TelemetryStreamKindEvent,
		StreamKey:      streamKey,
		Seq:            1,
		OrgID:          pgvalue.UUID(dbtest.DefaultOrgID),
		RunID:          pgvalue.UUID(runID),
		Kind:           "run.created",
		Message:        "run.created",
		Payload:        []byte(`{}`),
		RedactionClass: "internal",
		CreatedAt:      testTime(),
		OccurredAt:     testTime(),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEventStreamPublishesRunLogAndTerminalOutput(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	workspaceID := uuid.Must(uuid.NewV7())
	processID := uuid.Must(uuid.NewV7())
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	stream := &EventStream{log: slog.New(slog.NewTextHandler(io.Discard, nil)), db: &fakeStore{}, redis: redisClient}

	runLogKey := runLogStreamKey(dbtest.DefaultOrgID, dbtest.DefaultWorkerGroupID, runID)
	if err := stream.publishOutboxRow(context.Background(), db.ClaimLiveTelemetryOutboxRow{
		OutboxID:      10,
		StreamKind:    db.TelemetryStreamKindRunLog,
		StreamKey:     runLogKey,
		Seq:           11,
		OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		RunID:         pgvalue.UUID(runID),
		StreamName:    "stdout",
		Content:       []byte("hello\n"),
		SizeBytes:     6,
		ObservedSeq:   4,
		CreatedAt:     testTime(),
	}); err != nil {
		t.Fatal(err)
	}
	runLogRecords, err := redisClient.XRangeN(context.Background(), runLogKey, redisEventID(11), redisEventID(11), 1).Result()
	if err != nil {
		t.Fatal(err)
	}
	var runLog api.RunLogChunk
	if len(runLogRecords) != 1 {
		t.Fatalf("run log records = %d", len(runLogRecords))
	}
	if err := json.Unmarshal([]byte(runLogRecords[0].Values["run_log"].(string)), &runLog); err != nil {
		t.Fatal(err)
	}
	if runLog.ID != telemetryCursor(11) || runLog.ContentBase64 != base64.StdEncoding.EncodeToString([]byte("hello\n")) {
		t.Fatalf("run log = %+v", runLog)
	}

	terminalKey := terminalOutputStreamKey(dbtest.DefaultOrgID, dbtest.DefaultWorkerGroupID, workspaceID, "workspace_process", processID, "stdout")
	if err := stream.publishOutboxRow(context.Background(), db.ClaimLiveTelemetryOutboxRow{
		OutboxID:      12,
		StreamKind:    db.TelemetryStreamKindTerminalOutput,
		StreamKey:     terminalKey,
		OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		WorkspaceID:   pgvalue.UUID(workspaceID),
		ResourceKind:  "workspace_process",
		ResourceID:    pgvalue.UUID(processID),
		StreamName:    "stdout",
		Content:       []byte("term\n"),
		OffsetStart:   3,
		OffsetEnd:     8,
		OccurredAt:    testTime(),
		CreatedAt:     testTime(),
	}); err != nil {
		t.Fatal(err)
	}
	terminalRecords, err := redisClient.XRangeN(context.Background(), terminalKey, redisEventID(8), redisEventID(8), 1).Result()
	if err != nil {
		t.Fatal(err)
	}
	var terminal telemetry.TerminalOutputChunk
	if len(terminalRecords) != 1 {
		t.Fatalf("terminal records = %d", len(terminalRecords))
	}
	if err := json.Unmarshal([]byte(terminalRecords[0].Values["terminal_output"].(string)), &terminal); err != nil {
		t.Fatal(err)
	}
	if terminal.OffsetStart != 3 || terminal.OffsetEnd != 8 || string(terminal.Data) != "term\n" {
		t.Fatalf("terminal = %+v", terminal)
	}
}

func TestEventStreamReadsTerminalOutputFromRedisWhenHistoricalUnavailable(t *testing.T) {
	workspaceID := uuid.Must(uuid.NewV7())
	processID := uuid.Must(uuid.NewV7())
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	stream := &EventStream{
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:              &fakeStore{},
		redis:           redisClient,
		workerGroupID:   dbtest.DefaultWorkerGroupID,
		telemetryReader: fakeTelemetryReader{store: &fakeStore{}, listTerminalOutputErr: telemetry.ErrHistoricalUnavailable},
	}
	chunk := telemetry.TerminalOutputChunk{
		ID:          "5",
		Stream:      "stdout",
		OffsetStart: 0,
		OffsetEnd:   5,
		Data:        []byte("hello"),
		ObservedAt:  pgvalue.Time(testTime()),
		CreatedAt:   pgvalue.Time(testTime()),
	}
	payload, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	key := terminalOutputStreamKey(dbtest.DefaultOrgID, dbtest.DefaultWorkerGroupID, workspaceID, "workspace_process", processID, "stdout")
	if err := redisClient.XAdd(context.Background(), &redis.XAddArgs{
		Stream: key,
		ID:     redisEventID(5),
		Values: map[string]any{"terminal_output": string(payload)},
	}).Err(); err != nil {
		t.Fatal(err)
	}
	var got telemetry.TerminalOutputChunk
	err = stream.ReadTerminalOutput(context.Background(), telemetry.TerminalOutputQuery{
		OrgID:         dbtest.DefaultOrgID,
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ProjectID:     pgvalue.MustUUIDValue(testProjectID()),
		EnvironmentID: pgvalue.MustUUIDValue(testEnvironmentID()),
		WorkspaceID:   workspaceID,
		ResourceKind:  "workspace_process",
		ResourceID:    processID,
		StreamName:    "stdout",
	}, 0, 10, func(chunk telemetry.TerminalOutputChunk) error {
		got = chunk
		return errLiveTelemetryFollowComplete
	}, nil)
	if !errors.Is(err, errLiveTelemetryFollowComplete) {
		t.Fatal(err)
	}
	if got.OffsetEnd != 5 || string(got.Data) != "hello" {
		t.Fatalf("terminal chunk = %+v", got)
	}
}

func (f *fakeStore) AppendRunEvent(_ context.Context, arg db.AppendRunEventParams) (db.AppendRunEventRow, error) {
	f.runEvent = arg
	event := db.ClaimLiveTelemetryOutboxRow{
		Seq:       int64(len(f.events) + 1),
		OrgID:     arg.OrgID,
		RunID:     arg.RunID,
		Kind:      arg.Kind,
		Payload:   arg.Payload,
		CreatedAt: testTime(),
	}
	f.events = append(f.events, event)
	return db.AppendRunEventRow{
		ID:                   event.RunID,
		CurrentAttemptNumber: 1,
		EventKind:            event.Kind,
		EventPayload:         event.Payload,
	}, nil
}

func (f *fakeStore) AppendRunEventForExecution(_ context.Context, arg db.AppendRunEventForExecutionParams) (db.AppendRunEventForExecutionRow, error) {
	if f.sessionID != arg.RunLeaseID || f.executionWorkerInstanceID != arg.WorkerInstanceID || arg.WorkerGroupID != dbtest.DefaultWorkerGroupID {
		return db.AppendRunEventForExecutionRow{}, pgx.ErrNoRows
	}
	event := db.ClaimLiveTelemetryOutboxRow{
		Seq:            int64(len(f.events) + 1),
		OrgID:          arg.OrgID,
		RunID:          arg.RunID,
		RunLeaseID:     arg.RunLeaseID,
		AttemptNumber:  pgtype.Int4{Int32: 1, Valid: true},
		Kind:           arg.Kind,
		Payload:        arg.Payload,
		RedactionClass: fakeEventRedactionClass(arg.Kind),
		CreatedAt:      testTime(),
	}
	f.events = append(f.events, event)
	return db.AppendRunEventForExecutionRow{
		ID:            event.RunID,
		WorkerGroupID: arg.WorkerGroupID,
		RunLeaseID:    event.RunLeaseID,
		AttemptNumber: 1,
		EventKind:     event.Kind,
		EventPayload:  event.Payload,
	}, nil
}

func (f *fakeStore) UpdateRunMetadataForExecution(_ context.Context, arg db.UpdateRunMetadataForExecutionParams) (db.UpdateRunMetadataForExecutionRow, error) {
	if f.run.ID != arg.RunID || f.sessionID != arg.RunLeaseID {
		return db.UpdateRunMetadataForExecutionRow{}, pgx.ErrNoRows
	}
	f.updateRunMetadata = arg
	return db.UpdateRunMetadataForExecutionRow{
		ID:                   f.run.ID,
		OrgID:                f.run.OrgID,
		ProjectID:            fakeRunProjectID(f.run),
		EnvironmentID:        fakeRunEnvironmentID(f.run),
		DeploymentID:         f.run.DeploymentID,
		DeploymentTaskID:     f.run.DeploymentTaskID,
		DeploymentVersion:    f.run.DeploymentVersion,
		ApiVersion:           f.run.ApiVersion,
		SdkVersion:           f.run.SdkVersion,
		CliVersion:           f.run.CliVersion,
		TaskID:               f.run.TaskID,
		Status:               f.run.Status,
		ExecutionStatus:      f.run.ExecutionStatus,
		TerminalOutcome:      f.run.TerminalOutcome,
		Metadata:             f.run.Metadata,
		Tags:                 f.run.Tags,
		LockedRetryPolicy:    f.run.LockedRetryPolicy,
		CurrentAttemptNumber: f.run.CurrentAttemptNumber,
		ExitCode:             f.run.ExitCode,
		Output:               f.run.Output,
		CreatedAt:            f.run.CreatedAt,
		UpdatedAt:            f.run.UpdatedAt,
	}, nil
}

func fakeEventRedactionClass(kind string) string {
	return "sensitive"
}
