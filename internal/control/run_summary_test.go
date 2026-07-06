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
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCreateGetAndListRun(t *testing.T) {
	store := &fakeStore{
		currentDeploymentTaskSecretDeclarations: []byte(`[{"name":"API_KEY","env":"API_KEY"}]`),
	}
	runEnqueuer := &fakeRunEnqueuer{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{values: api.ResolvedSecrets{"API_KEY": []byte("secret-value")}}, RunEnqueuer: runEnqueuer})

	bodyBytes, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.run.TaskID != "deploy" {
		t.Fatalf("stored task = %s", store.run.TaskID)
	}
	if string(store.createRun.Payload) != `{"env":"prod"}` {
		t.Fatalf("payload = %s", store.createRun.Payload)
	}
	if store.createRun.MaxActiveDurationMs != 300_000 {
		t.Fatalf("max duration = %d", store.createRun.MaxActiveDurationMs)
	}
	if store.currentDeploymentTaskCalls != 0 {
		t.Fatalf("current deployment task calls = %d, want 0 for run placement", store.currentDeploymentTaskCalls)
	}
	if store.getDeploymentTaskCalls != 1 {
		t.Fatalf("deployment task calls = %d, want 1 for generation-scoped run placement", store.getDeploymentTaskCalls)
	}
	if store.runEvent.Kind != "run.created" {
		t.Fatalf("run event kind = %s", store.runEvent.Kind)
	}
	var eventPayload struct {
		TaskID             string          `json:"task_id"`
		Payload            json.RawMessage `json:"payload"`
		MaxDurationSeconds int32           `json:"max_duration_seconds"`
		SecretNames        []string        `json:"secret_names"`
	}
	if err := json.Unmarshal(store.runEvent.Payload, &eventPayload); err != nil {
		t.Fatalf("run event payload decode: %v", err)
	}
	if eventPayload.TaskID != "deploy" || string(eventPayload.Payload) != `{"env":"prod"}` || eventPayload.MaxDurationSeconds != 300 {
		t.Fatalf("run event payload = %+v", eventPayload)
	}
	if len(eventPayload.SecretNames) != 1 || eventPayload.SecretNames[0] != "API_KEY" {
		t.Fatalf("run event secret names = %+v", eventPayload.SecretNames)
	}
	if runEnqueuer.orgID != store.run.OrgID || runEnqueuer.runID != store.run.ID {
		t.Fatalf("enqueued org=%+v run=%+v, want org=%+v run=%+v", runEnqueuer.orgID, runEnqueuer.runID, store.run.OrgID, store.run.ID)
	}

	var startResponse api.SessionStartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &startResponse); err != nil {
		t.Fatal(err)
	}
	created := startResponse.Run
	if created.DeploymentID != pgvalue.MustUUIDValue(testDeploymentID()).String() || created.DeploymentTaskID != pgvalue.MustUUIDValue(testDeploymentTaskID()).String() {
		t.Fatalf("created deployment pin = %s/%s", created.DeploymentID, created.DeploymentTaskID)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/runs/"+created.ID, nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.listRuns.StatusFilter != "live" || store.listRuns.RowLimit != 100 {
		t.Fatalf("list params = %+v", store.listRuns)
	}
	var list api.ListRunsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Runs) != 1 || list.Runs[0].ID != created.ID {
		t.Fatalf("list = %+v", list)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/runs/counts", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("counts status = %d body=%s", rec.Code, rec.Body.String())
	}
	var counts api.RunCountsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &counts); err != nil {
		t.Fatal(err)
	}
	if counts.Queued != 1 || counts.Running != 0 || counts.Failed != 0 {
		t.Fatalf("counts = %+v", counts)
	}

	if store.countScopedRuns.ProjectID != testProjectID() || store.countScopedRuns.EnvironmentID != testEnvironmentID() {
		t.Fatalf("scoped count params = %+v", store.countScopedRuns)
	}
}

func TestGetRunRejectsWrongWorkerGroupRoute(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	store := &fakeStore{
		recordPlacementUnavailable: true,
		run: db.Run{
			ID:               pgvalue.UUID(runID),
			OrgID:            pgvalue.UUID(dbtest.DefaultOrgID),
			WorkerGroupID:    "us-east-1-worker-group-2",
			ProjectID:        testProjectID(),
			EnvironmentID:    testEnvironmentID(),
			DeploymentID:     testDeploymentID(),
			DeploymentTaskID: testDeploymentTaskID(),
			SessionID:        pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000602")),
			TaskID:           "deploy",
			Status:           db.RunStatusRunning,
			CreatedAt:        testTime(),
			UpdatedAt:        testTime(),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})

	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+runID.String(), nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestListRunsQuery(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}})
	runID := uuid.Must(uuid.NewV7())
	store.run = db.Run{
		ID:               pgvalue.UUID(runID),
		OrgID:            pgvalue.UUID(dbtest.DefaultOrgID),
		ProjectID:        testProjectID(),
		EnvironmentID:    testEnvironmentID(),
		DeploymentID:     testDeploymentID(),
		DeploymentTaskID: testDeploymentTaskID(),
		SessionID:        pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000602")),
		TaskID:           "deploy",
		Status:           db.RunStatusSucceeded,
		CreatedAt:        testTime(),
		UpdatedAt:        testTime(),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/runs?status=all&limit=25", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.listRuns.StatusFilter != "all" || store.listRuns.RowLimit != 25 {
		t.Fatalf("list params = %+v", store.listRuns)
	}
}

func TestAPIKeyListRunsUsesActorEnvironmentScope(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{
		kind:          auth.ActorKindAPIKey,
		projectID:     testProjectIDString(),
		environmentID: testEnvironmentIDString(),
		permissions:   []auth.Permission{auth.PermissionRunsRead},
	}, Secrets: fakeSecrets{}},
	)
	runID := uuid.Must(uuid.NewV7())
	store.run = db.Run{
		ID:               pgvalue.UUID(runID),
		OrgID:            pgvalue.UUID(dbtest.DefaultOrgID),
		ProjectID:        testProjectID(),
		EnvironmentID:    testEnvironmentID(),
		DeploymentID:     testDeploymentID(),
		DeploymentTaskID: testDeploymentTaskID(),
		SessionID:        pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000602")),
		TaskID:           "deploy",
		Status:           db.RunStatusSucceeded,
		CreatedAt:        testTime(),
		UpdatedAt:        testTime(),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/runs?status=all&limit=25", nil)
	req.Header.Set("authorization", "Bearer machine-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.listScopedRuns.ProjectID != testProjectID() || store.listScopedRuns.EnvironmentID != testEnvironmentID() {
		t.Fatalf("scoped list params = %+v", store.listScopedRuns)
	}
}

func TestListRunsQueryRejectsLeasedStatus(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/runs?status=leased", nil)

	if _, _, err := listRunsQuery(req); err == nil {
		t.Fatal("listRunsQuery accepted leased status")
	}
}

func TestListRunsRunningFilterReturnsLeasedAsPublicRunning(t *testing.T) {
	for _, tt := range []struct {
		name string
		path string
	}{
		{name: "org", path: "/api/runs?status=running"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runID := uuid.Must(uuid.NewV7())
			store := &fakeStore{
				run: db.Run{
					ID:               pgvalue.UUID(runID),
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
			}
			server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set("authorization", "Bearer test-key")
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			if store.listRuns.StatusFilter != "running" {
				t.Fatalf("list status filter = %q, want running", store.listRuns.StatusFilter)
			}
			var list api.ListRunsResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
				t.Fatal(err)
			}
			if len(list.Runs) != 1 || list.Runs[0].ID != runID.String() || list.Runs[0].Status != "running" {
				t.Fatalf("list = %+v", list)
			}
		})
	}
}

func TestRunResponseMapsLeasedToRunning(t *testing.T) {
	response := runResponse(runSummary{
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		ProjectID:        testProjectID(),
		EnvironmentID:    testEnvironmentID(),
		DeploymentID:     testDeploymentID(),
		DeploymentTaskID: testDeploymentTaskID(),
		SessionID:        pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000602")),
		TaskID:           "deploy",
		Status:           db.RunStatusRunning,
		CreatedAt:        testTime(),
		UpdatedAt:        testTime(),
	})

	if response.Status != "running" {
		t.Fatalf("status = %q, want running", response.Status)
	}
}

func TestScopedRunCountsResponseMapsRunningCount(t *testing.T) {
	counts := scopedRunCountsResponse(db.CountScopedRunsByStatusRow{Queued: 2, Running: 5})
	if counts.Queued != 2 || counts.Running != 5 {
		t.Fatalf("counts = %+v", counts)
	}

	body, err := json.Marshal(counts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "leased") {
		t.Fatalf("counts leaked leased field: %s", body)
	}

}

func fakeRunProjectID(run db.Run) pgtype.UUID {
	if run.ProjectID.Valid {
		return run.ProjectID
	}
	return testProjectID()
}

func fakeRunEnvironmentID(run db.Run) pgtype.UUID {
	if run.EnvironmentID.Valid {
		return run.EnvironmentID
	}
	return testEnvironmentID()
}

func fakeRunSessionID(run db.Run) pgtype.UUID {
	if run.SessionID.Valid {
		return run.SessionID
	}
	return pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000601"))
}

func fakeRunWorkerGroupID(run db.Run) string {
	if run.WorkerGroupID != "" {
		return run.WorkerGroupID
	}
	return dbtest.DefaultWorkerGroupID
}

func (f *fakeStore) GetRunSummary(_ context.Context, arg db.GetRunSummaryParams) (db.Run, error) {
	if f.run.ID != arg.ID {
		return db.Run{}, pgx.ErrNoRows
	}
	return db.Run{
		ID:               f.run.ID,
		OrgID:            f.run.OrgID,
		WorkerGroupID:    fakeRunWorkerGroupID(f.run),
		ProjectID:        fakeRunProjectID(f.run),
		EnvironmentID:    fakeRunEnvironmentID(f.run),
		DeploymentID:     fakeRunDeploymentID(f.run),
		DeploymentTaskID: fakeRunDeploymentTaskID(f.run),
		SessionID:        fakeRunSessionID(f.run),
		TaskID:           f.run.TaskID,
		Status:           f.run.Status,
		ExitCode:         f.run.ExitCode,
		Output:           f.run.Output,
		CreatedAt:        f.run.CreatedAt,
		UpdatedAt:        f.run.UpdatedAt,
	}, nil
}

func (f *fakeStore) ListScopedRunSummaries(_ context.Context, arg db.ListScopedRunSummariesParams) ([]db.Run, error) {
	f.listScopedRuns = arg
	f.listRuns = fakeListRunsParams{
		StatusFilter: arg.StatusFilter,
		RowLimit:     arg.RowLimit,
	}
	if !f.run.ID.Valid || f.run.ProjectID != arg.ProjectID || f.run.EnvironmentID != arg.EnvironmentID {
		return nil, nil
	}
	return []db.Run{{
		ID:               f.run.ID,
		OrgID:            f.run.OrgID,
		WorkerGroupID:    fakeRunWorkerGroupID(f.run),
		ProjectID:        f.run.ProjectID,
		EnvironmentID:    f.run.EnvironmentID,
		DeploymentID:     fakeRunDeploymentID(f.run),
		DeploymentTaskID: fakeRunDeploymentTaskID(f.run),
		SessionID:        fakeRunSessionID(f.run),
		TaskID:           f.run.TaskID,
		Status:           f.run.Status,
		ExitCode:         f.run.ExitCode,
		Output:           f.run.Output,
		CreatedAt:        f.run.CreatedAt,
		UpdatedAt:        f.run.UpdatedAt,
	}}, nil
}

func (f *fakeStore) CountScopedRunsByStatus(_ context.Context, arg db.CountScopedRunsByStatusParams) (db.CountScopedRunsByStatusRow, error) {
	f.countScopedRuns = arg
	var counts db.CountScopedRunsByStatusRow
	if !f.run.ID.Valid || f.run.ProjectID != arg.ProjectID || f.run.EnvironmentID != arg.EnvironmentID {
		return counts, nil
	}
	switch f.run.Status {
	case db.RunStatusQueued:
		counts.Queued++
	case db.RunStatusRunning:
		counts.Running++
	case db.RunStatusWaiting:
		counts.Waiting++
	case db.RunStatusSucceeded:
		counts.Succeeded++
	case db.RunStatusFailed:
		counts.Failed++
	case db.RunStatusCancelled:
		counts.Cancelled++
	}
	return counts, nil
}
