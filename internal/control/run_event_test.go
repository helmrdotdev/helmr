package control

import (
	"context"
	"encoding/json"
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

	params := workerInstanceHeartbeatParams(workerActor{WorkerInstanceID: uuid.Must(uuid.NewV7()), WorkerGroupID: pgvalue.MustUUIDValue(testWorkerGroupID()), ResourceID: "worker-resource"}, api.WorkerCapabilities{
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
		store.events = append(store.events, db.EventHotPayload{
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
	streamKey := eventStreamKey(dbtest.DefaultOrgID, "us-east-1-cell-1", db.EventSubjectTypeRun, runID)
	if err := redisClient.XAdd(context.Background(), &redis.XAddArgs{
		Stream: streamKey,
		ID:     "2-0",
		Values: map[string]any{"event": `{"id":"2","kind":"run.completed"}`},
	}).Err(); err != nil {
		t.Fatal(err)
	}
	stream := &EventStream{log: slog.New(slog.NewTextHandler(io.Discard, nil)), db: &fakeStore{}, redis: redisClient}
	err := stream.publishOutboxRow(context.Background(), db.ClaimEventOutboxRow{
		OutboxID:       1,
		StreamKey:      streamKey,
		Seq:            1,
		OrgID:          pgvalue.UUID(dbtest.DefaultOrgID),
		RunID:          pgvalue.UUID(runID),
		SubjectType:    db.EventSubjectTypeRun,
		SubjectID:      pgvalue.UUID(runID),
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

func (f *fakeStore) AppendRunEvent(_ context.Context, arg db.AppendRunEventParams) (db.AppendRunEventRow, error) {
	f.runEvent = arg
	event := db.EventHotPayload{
		Seq:       int64(len(f.events) + 1),
		OrgID:     arg.OrgID,
		RunID:     arg.RunID,
		Kind:      arg.Kind,
		Payload:   arg.Payload,
		CreatedAt: testTime(),
	}
	f.events = append(f.events, event)
	return db.AppendRunEventRow{
		Seq:       event.Seq,
		OrgID:     event.OrgID,
		RunID:     event.RunID,
		Kind:      event.Kind,
		Payload:   event.Payload,
		CreatedAt: event.CreatedAt,
	}, nil
}

func (f *fakeStore) ListSubjectEvents(_ context.Context, arg db.ListSubjectEventsParams) ([]db.EventHotPayload, error) {
	var events []db.EventHotPayload
	for _, event := range f.events {
		if arg.SubjectType == db.EventSubjectTypeRun && event.RunID == arg.SubjectID && event.Seq > arg.Seq {
			events = append(events, event)
		}
	}
	for _, event := range f.deploymentEvents {
		if arg.SubjectType == db.EventSubjectTypeDeployment && event.DeploymentID == arg.SubjectID && event.Seq > arg.Seq {
			events = append(events, event)
		}
	}
	if int32(len(events)) > arg.RowLimit {
		events = events[:arg.RowLimit]
	}
	return events, nil
}

func (f *fakeStore) AppendRunEventForExecution(_ context.Context, arg db.AppendRunEventForExecutionParams) (db.AppendRunEventForExecutionRow, error) {
	if f.sessionID != arg.RunLeaseID || f.executionWorkerInstanceID != arg.WorkerInstanceID || arg.CellID != dbtest.DefaultCellID {
		return db.AppendRunEventForExecutionRow{}, pgx.ErrNoRows
	}
	event := db.EventHotPayload{
		Seq:            int64(len(f.events) + 1),
		OrgID:          arg.OrgID,
		CellID:         arg.CellID,
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
		Seq:             event.Seq,
		OrgID:           event.OrgID,
		RunID:           event.RunID,
		RunLeaseID:      event.RunLeaseID,
		AttemptNumber:   event.AttemptNumber,
		Kind:            event.Kind,
		Payload:         event.Payload,
		RedactionClass:  event.RedactionClass,
		CreatedAt:       event.CreatedAt,
		SnapshotVersion: event.SnapshotVersion,
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
