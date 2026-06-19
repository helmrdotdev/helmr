package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func TestCancelRunReturnsAppliedOperationAfterNoRowsRace(t *testing.T) {
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeStore{
		run: db.Run{
			ID:               runID,
			OrgID:            pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:        testProjectID(),
			EnvironmentID:    testEnvironmentID(),
			DeploymentID:     testDeploymentID(),
			DeploymentTaskID: testDeploymentTaskID(),
			TaskID:           "deploy",
			Status:           db.RunStatusCancelled,
			CreatedAt:        testTime(),
			UpdatedAt:        testTime(),
		},
		cancelRunErr: pgx.ErrNoRows,
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})

	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+pgvalue.MustUUIDValue(runID).String()+"/cancel", strings.NewReader(`{"reason":"stop"}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.cancelRunCalls != 1 {
		t.Fatalf("cancel calls = %d, want 1", store.cancelRunCalls)
	}
	var response struct {
		Run       api.RunResponse          `json:"run"`
		Operation api.RunOperationResponse `json:"operation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Run.Status != string(db.RunStatusCancelled) || response.Operation.Status != string(db.RunOperationStatusApplied) {
		t.Fatalf("response status = run %q operation %q, want cancelled/applied", response.Run.Status, response.Operation.Status)
	}
}

func TestCancelRunRejectsMismatchedIdempotencyRequest(t *testing.T) {
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	operationID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeStore{
		run: db.Run{
			ID:               runID,
			OrgID:            pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:        testProjectID(),
			EnvironmentID:    testEnvironmentID(),
			DeploymentID:     testDeploymentID(),
			DeploymentTaskID: testDeploymentTaskID(),
			TaskID:           "deploy",
			Status:           db.RunStatusRunning,
			CreatedAt:        testTime(),
			UpdatedAt:        testTime(),
		},
		runOperation: db.RunOperation{
			ID:             operationID,
			OrgID:          pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:      testProjectID(),
			EnvironmentID:  testEnvironmentID(),
			RunID:          runID,
			Kind:           db.RunOperationKindCancel,
			Status:         db.RunOperationStatusRequested,
			Request:        []byte(`{"reason":"stop","idempotency_key":"cancel-key"}`),
			IdempotencyKey: "cancel-key",
			CreatedAt:      testTime(),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})

	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+pgvalue.MustUUIDValue(runID).String()+"/cancel", strings.NewReader(`{"reason":"stop","force":true,"idempotency_key":"cancel-key"}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("cancel status = %d body=%s, want conflict", rec.Code, rec.Body.String())
	}
	if store.cancelRunCalls != 0 {
		t.Fatalf("cancel calls = %d, want 0", store.cancelRunCalls)
	}
}

func TestCreateRunIdempotencyReplayBypassesRemovedQueueValidation(t *testing.T) {
	orgID := pgvalue.UUID(dbtest.DefaultOrgID)
	store := &fakeStore{deploymentTasks: []db.DeploymentTask{{
		ID:                   testDeploymentTaskID(),
		OrgID:                orgID,
		ProjectID:            testProjectID(),
		EnvironmentID:        testEnvironmentID(),
		DeploymentID:         testDeploymentID(),
		TaskID:               "deploy",
		FilePath:             "tasks/deploy.ts",
		ExportName:           "deploy",
		HandlerEntrypoint:    "tasks/deploy.ts#deploy",
		BundleArtifactID:     testArtifactID(),
		RequestedMilliCpu:    2000,
		RequestedMemoryMib:   2048,
		SecretDeclarations:   []byte("[]"),
		ResourceRequirements: []byte("{}"),
		QueueName:            "reports",
		MaxDurationSeconds:   300,
		CreatedAt:            testTime(),
	}}}
	runEnqueuer := &fakeRunEnqueuer{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}, RunEnqueuer: runEnqueuer, EventStream: newTestEventStream(t)})

	bodyBytes, err := json.Marshal(api.TaskStartRequest{
		Payload: json.RawMessage(`{"env":"prod"}`),
		Options: api.TaskStartOptions{
			Queue:          &api.RunQueueOption{Name: "reports"},
			IdempotencyKey: "deploy-prod",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var first api.TaskStartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	store.deploymentTasks[0].QueueName = "default"

	req = httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var second api.TaskStartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.Run.ID != first.Run.ID || !second.IsCached {
		t.Fatalf("second response = %+v first=%+v", second, first)
	}
	if runEnqueuer.count != 1 || len(store.events) != 1 {
		t.Fatalf("events=%d enqueues=%d", len(store.events), runEnqueuer.count)
	}
}

func (f *fakeStore) CreateRunOperation(_ context.Context, arg db.CreateRunOperationParams) (db.RunOperation, error) {
	if f.runOperation.ID.Valid && f.runOperation.OrgID == arg.OrgID && f.runOperation.RunID == arg.RunID && f.runOperation.Kind == arg.Kind && f.runOperation.IdempotencyKey == arg.IdempotencyKey && arg.IdempotencyKey != "" {
		return f.runOperation, nil
	}
	f.runOperation = db.RunOperation{
		ID:             arg.ID,
		OrgID:          arg.OrgID,
		ProjectID:      arg.ProjectID,
		EnvironmentID:  arg.EnvironmentID,
		RunID:          arg.RunID,
		Kind:           arg.Kind,
		Status:         db.RunOperationStatusRequested,
		ActorKind:      arg.ActorKind,
		ActorID:        arg.ActorID,
		ApiKeyID:       arg.ApiKeyID,
		Reason:         arg.Reason,
		Request:        arg.Request,
		Result:         []byte(`{}`),
		IdempotencyKey: arg.IdempotencyKey,
		CreatedAt:      testTime(),
	}
	return f.runOperation, nil
}

func (f *fakeStore) GetRunOperation(_ context.Context, arg db.GetRunOperationParams) (db.RunOperation, error) {
	if f.runOperation.ID != arg.ID || f.runOperation.OrgID != arg.OrgID {
		return db.RunOperation{}, pgx.ErrNoRows
	}
	return f.runOperation, nil
}

func (f *fakeStore) CancelRun(_ context.Context, arg db.CancelRunParams) (db.CancelRunRow, error) {
	f.cancelRunCalls++
	if f.cancelRunErr != nil {
		f.runOperation.Status = db.RunOperationStatusApplied
		f.runOperation.Result = []byte(`{"status":"cancelled"}`)
		f.runOperation.AppliedAt = testTime()
		return db.CancelRunRow{}, f.cancelRunErr
	}
	f.run.Status = db.RunStatusCancelled
	if f.run.ExecutionStatus == db.RunExecutionStatusExecuting && !arg.Force {
		f.run.ExecutionStatus = db.RunExecutionStatusPendingCancel
	} else {
		f.run.ExecutionStatus = db.RunExecutionStatusFinished
	}
	f.runOperation.Status = db.RunOperationStatusApplied
	f.runOperation.Result = []byte(`{"status":"cancelled"}`)
	f.runOperation.AppliedAt = testTime()
	return db.CancelRunRow{
		ID:                   f.run.ID,
		OrgID:                f.run.OrgID,
		ProjectID:            f.run.ProjectID,
		EnvironmentID:        f.run.EnvironmentID,
		DeploymentID:         fakeRunDeploymentID(f.run),
		DeploymentTaskID:     fakeRunDeploymentTaskID(f.run),
		TaskSessionID:        fakeRunTaskSessionID(f.run),
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
