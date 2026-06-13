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
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWaitpointTimeoutRequiresDelayTimeout(t *testing.T) {
	if _, err := waitpointTimeout(db.WaitpointKindDelay, nil); err == nil {
		t.Fatal("delay timeout validation succeeded without timeout")
	}
}

func TestWaitpointTimeoutAllowsNonDelayWithoutTimeout(t *testing.T) {
	timeout, err := waitpointTimeout(db.WaitpointKindHuman, nil)
	if err != nil {
		t.Fatal(err)
	}
	if timeout.Valid {
		t.Fatalf("timeout = %+v, want invalid", timeout)
	}
}

func TestWaitpointTimeoutRejectsNonPositiveTimeout(t *testing.T) {
	zero := int32(0)
	if _, err := waitpointTimeout(db.WaitpointKindHuman, &zero); err == nil {
		t.Fatal("timeout validation succeeded with zero")
	}
}

func TestWaitpointTimeoutAcceptsPositiveTimeout(t *testing.T) {
	seconds := int32(30)
	timeout, err := waitpointTimeout(db.WaitpointKindDelay, &seconds)
	if err != nil {
		t.Fatal(err)
	}
	if !timeout.Valid || timeout.Int32 != seconds {
		t.Fatalf("timeout = %+v, want %d", timeout, seconds)
	}
}

func TestWaitpointRequestLinkedIDAllowsArbitraryJSON(t *testing.T) {
	for _, request := range []string{`[1,2,3]`, `"hello"`, `42`, `{"waitpoint_id":123}`} {
		id, ok, err := waitpointRequestLinkedID(db.WaitpointKindHuman, []byte(request))
		if err != nil {
			t.Fatalf("request %s error = %v", request, err)
		}
		if ok || id != uuid.Nil {
			t.Fatalf("request %s linked id = %s, ok = %v", request, id, ok)
		}
	}
}

func TestWaitpointRequestLinkedIDExtractsStringID(t *testing.T) {
	waitpointID := uuid.Must(uuid.NewV7())
	id, ok, err := waitpointRequestLinkedID(db.WaitpointKindHuman, []byte(`{"waitpoint_id":"`+waitpointID.String()+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !ok || id != waitpointID {
		t.Fatalf("linked id = %s, ok = %v", id, ok)
	}
}

func TestWaitpointRequestLinkedIDExtractsCamelCaseStringID(t *testing.T) {
	waitpointID := uuid.Must(uuid.NewV7())
	id, ok, err := waitpointRequestLinkedID(db.WaitpointKindHuman, []byte(`{"waitpointId":"`+waitpointID.String()+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !ok || id != waitpointID {
		t.Fatalf("linked id = %s, ok = %v", id, ok)
	}
}

func TestCreateRunIdempotencyHitIncludesPendingWaitpoint(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}})

	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:  "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
		Options: api.CreateRunOptions{IdempotencyKey: "deploy-prod"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create status = %d body=%s", rec.Code, rec.Body.String())
	}
	waitpointID := uuid.Must(uuid.NewV7())
	store.run.Status = db.RunStatusWaiting
	store.waitpoint = fakeWaitpoint{
		ID:          pgvalue.UUID(waitpointID),
		OrgID:       store.run.OrgID,
		RunID:       store.run.ID,
		Kind:        db.WaitpointKindHuman,
		DisplayText: "ship it",
		Status:      db.RunWaitStatusWaiting,
		RequestedAt: testTime(),
	}

	req = httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var second api.RunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.PendingWaitpoint == nil || second.PendingWaitpoint.WaitpointID != waitpointID.String() || second.PendingWaitpoint.DisplayText != "ship it" {
		t.Fatalf("pending wait = %+v", second.PendingWaitpoint)
	}
}

func TestListRunsIncludesPendingWaitpointDeliveries(t *testing.T) {
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	runWaitID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	waitpointID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	matchingDeliveryID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	ignoredDeliveryID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	createdAt := testTime()
	store := &fakeStore{
		run: db.Run{
			ID:               runID,
			OrgID:            pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:        testProjectID(),
			EnvironmentID:    testEnvironmentID(),
			DeploymentID:     testDeploymentID(),
			DeploymentTaskID: testDeploymentTaskID(),
			TaskID:           "deploy",
			Status:           db.RunStatusWaiting,
			CreatedAt:        testTime(),
			UpdatedAt:        testTime(),
		},
		waitpoint: fakeWaitpoint{
			ID:            waitpointID,
			RunWaitID:     runWaitID,
			OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:     testProjectID(),
			EnvironmentID: testEnvironmentID(),
			RunID:         runID,
			Kind:          db.WaitpointKindHuman,
			Request:       []byte(`{"prompt":"approve"}`),
			DisplayText:   "approve deploy",
			Status:        db.RunWaitStatusWaiting,
			CreatedAt:     createdAt,
			RequestedAt:   createdAt,
		},
		waitpointDeliveries: []db.WaitpointDelivery{
			{
				ID:            ignoredDeliveryID,
				OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
				RunID:         runID,
				RunWaitID:     runWaitID,
				WaitpointID:   pgvalue.UUID(uuid.Must(uuid.NewV7())),
				Channel:       "email",
				RecipientKind: "user",
				Recipient:     "ignored@example.test",
				Status:        db.WaitpointDeliveryStatusQueued,
				CreatedAt:     createdAt,
				UpdatedAt:     createdAt,
			},
			{
				ID:            matchingDeliveryID,
				OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
				RunID:         runID,
				RunWaitID:     runWaitID,
				WaitpointID:   waitpointID,
				Channel:       "email",
				RecipientKind: "user",
				Recipient:     "reviewer@example.test",
				Status:        db.WaitpointDeliveryStatusSent,
				CreatedAt:     pgtype.Timestamptz{Time: createdAt.Time.Add(time.Second), Valid: true},
				UpdatedAt:     createdAt,
			},
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}})
	req := httptest.NewRequest(http.MethodGet, "/api/runs?status=live", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var list api.ListRunsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Runs) != 1 || list.Runs[0].PendingWaitpoint == nil {
		t.Fatalf("list = %+v", list)
	}
	deliveries := list.Runs[0].PendingWaitpoint.Deliveries
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %+v", deliveries)
	}
	if deliveries[0].ID != pgvalue.MustUUIDValue(matchingDeliveryID).String() || deliveries[0].Recipient != "reviewer@example.test" || deliveries[0].Status != string(db.WaitpointDeliveryStatusSent) {
		t.Fatalf("delivery = %+v", deliveries[0])
	}
	if deliveries[0].ID == pgvalue.MustUUIDValue(ignoredDeliveryID).String() {
		t.Fatalf("included non-matching delivery: %+v", deliveries)
	}
}

func TestWorkerWaitpointLifecycle(t *testing.T) {
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
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}, WorkerTokenSecret: []byte("01234567890123456789012345678901"), WorkerTokenTTL: time.Hour})
	workerBearer := mintTestWorkerToken(t, server, "00000000-0000-0000-0000-000000000401")
	req := httptest.NewRequest(http.MethodPost, "/api/worker/sessions/lease", bytes.NewReader(testWorkerRunLeaseRequestBody(t)))
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
	startBody, err := json.Marshal(api.WorkerStartRequest{Lease: *claimResponse.Lease})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/worker/sessions/start", bytes.NewReader(startBody))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d body=%s", rec.Code, rec.Body.String())
	}

	timeout := int32(60)
	createBody, err := json.Marshal(api.WorkerCreateWaitpointRequest{
		Lease:          *claimResponse.Lease,
		CorrelationID:  "1",
		Kind:           api.WorkerWaitpointKindHuman,
		Request:        json.RawMessage(`{"message":"ship it"}`),
		DisplayText:    "ship it",
		TimeoutSeconds: &timeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/worker/sessions/waitpoints", bytes.NewReader(createBody))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create waitpoint status = %d body=%s", rec.Code, rec.Body.String())
	}
	var created api.WorkerCreateWaitpointResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/runs/"+claimResponse.Lease.RunID, nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get run status = %d body=%s", rec.Code, rec.Body.String())
	}
	var run api.RunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.PendingWaitpoint != nil {
		t.Fatalf("pending wait before checkpoint ready = %+v", run.PendingWaitpoint)
	}
	if store.run.Status != db.RunStatusRunning {
		t.Fatalf("run status before durable checkpoint = %s", store.run.Status)
	}

	readyBody, err := json.Marshal(api.WorkerCheckpointReadyRequest{
		Lease:        *claimResponse.Lease,
		RunWaitID:    created.RunWaitID,
		WaitpointID:  created.WaitpointID,
		CheckpointID: created.CheckpointID,
		Manifest:     testWorkerCheckpointManifest(claimResponse.Lease.RunID, created.WaitpointID, created.CheckpointID),
	})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/worker/sessions/checkpoints/ready", bytes.NewReader(readyBody))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("checkpoint ready status = %d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/runs/"+claimResponse.Lease.RunID, nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get run after checkpoint ready status = %d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.PendingWaitpoint == nil || run.PendingWaitpoint.Kind != "human" || run.PendingWaitpoint.WaitpointID != created.WaitpointID || run.PendingWaitpoint.DisplayText != "ship it" {
		t.Fatalf("pending wait = %+v", run.PendingWaitpoint)
	}
	if store.run.Status != db.RunStatusWaiting || store.run.CurrentSessionID.Valid {
		t.Fatalf("run after checkpoint ready = %+v", store.run)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/runs/"+claimResponse.Lease.RunID+"/waitpoints/"+created.WaitpointID+"/not-a-route", bytes.NewBufferString(`{"reason":"wrong route"}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("wrong-kind resolve status = %d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/waitpoints/"+created.WaitpointID+"/respond", bytes.NewBufferString(`{"value":{"action":"approve","reason":"ok"}}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("respond status = %d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/worker/sessions/lease", bytes.NewReader(testWorkerRunLeaseRequestBody(t)))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore claim status = %d body=%s", rec.Code, rec.Body.String())
	}
	var restoreClaim api.WorkerRunLeaseResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &restoreClaim); err != nil {
		t.Fatal(err)
	}
	if restoreClaim.Lease == nil || restoreClaim.Lease.ID == claimResponse.Lease.ID || restoreClaim.Run == nil || restoreClaim.Run.ID != claimResponse.Lease.RunID {
		t.Fatalf("restore claim = %+v", restoreClaim)
	}
	if restoreClaim.Run.Restore == nil || restoreClaim.Run.Restore.CheckpointID != created.CheckpointID || restoreClaim.Run.Restore.Waitpoint.RunWaitID != created.RunWaitID || restoreClaim.Run.Restore.Waitpoint.ID != created.WaitpointID || restoreClaim.Run.Restore.Waitpoint.ResumeKind != "completed" {
		t.Fatalf("restore payload = %+v", restoreClaim.Run.Restore)
	}
	restoreResolution := decodeObject(t, restoreClaim.Run.Restore.Waitpoint.ResumePayloadJSON)
	if _, ok := restoreResolution["principal"].(string); !ok {
		t.Fatalf("restore resolution payload = %+v", restoreResolution)
	}
	if _, err := time.Parse(time.RFC3339Nano, stringField(t, restoreResolution, "at")); err != nil {
		t.Fatalf("restore resolution at = %v", err)
	}
	if len(store.events) < 2 || store.events[len(store.events)-2].Kind != "waitpoint.requested" || store.events[len(store.events)-1].Kind != "waitpoint.resolved" {
		t.Fatalf("events = %+v", store.events)
	}
}

func TestResolveWaitpointPayloadsMatchAdapterResumeContract(t *testing.T) {
	tests := []struct {
		name               string
		waitpointKind      db.WaitpointKind
		action             string
		body               string
		wantResolutionKind string
		assertResolution   func(t *testing.T, payload map[string]any)
		assertEvent        func(t *testing.T, payload map[string]any)
	}{{
		name:               "human responded",
		waitpointKind:      db.WaitpointKindHuman,
		action:             "respond",
		body:               `{"value":{"action":"approve","reason":"looks good"}}`,
		wantResolutionKind: "completed",
		assertResolution: func(t *testing.T, payload map[string]any) {
			t.Helper()
			value, ok := payload["value"].(map[string]any)
			if _, principalOK := payload["principal"].(string); !ok || !principalOK || value["action"] != "approve" || value["reason"] != "looks good" {
				t.Fatalf("resolution payload = %+v", payload)
			}
			assertRFC3339NanoField(t, payload, "at")
		},
		assertEvent: func(t *testing.T, payload map[string]any) {
			t.Helper()
			result, ok := payload["result"].(map[string]any)
			if !ok || payload["kind"] != "human" || payload["resolution_kind"] != "completed" || result["action"] != "approve" {
				t.Fatalf("event payload = %+v", payload)
			}
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runID := uuid.Must(uuid.NewV7())
			waitpointID := uuid.Must(uuid.NewV7())
			store := &fakeStore{
				run: db.Run{
					ID:        pgvalue.UUID(runID),
					OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
					TaskID:    "deploy",
					Status:    db.RunStatusWaiting,
					CreatedAt: testTime(),
					UpdatedAt: testTime(),
				},
				waitpoint: fakeWaitpoint{
					ID:          pgvalue.UUID(waitpointID),
					OrgID:       pgvalue.UUID(dbtest.DefaultOrgID),
					RunID:       pgvalue.UUID(runID),
					Kind:        tt.waitpointKind,
					Status:      db.RunWaitStatusWaiting,
					RequestedAt: testTime(),
				},
			}
			server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}})
			req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/"+waitpointID.String()+"/respond", strings.NewReader(tt.body))
			req.Header.Set("authorization", "Bearer test-key")
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("resolve status = %d body=%s", rec.Code, rec.Body.String())
			}
			if store.waitpoint.ResolutionKind.String != tt.wantResolutionKind {
				t.Fatalf("resolution kind = %q", store.waitpoint.ResolutionKind.String)
			}
			tt.assertResolution(t, decodeObject(t, store.waitpoint.Resolution))
			if store.run.Status != db.RunStatusQueued || store.run.CurrentSessionID.Valid {
				t.Fatalf("run after resolve = %+v", store.run)
			}
			if len(store.events) != 1 || store.events[0].Kind != "waitpoint.resolved" {
				t.Fatalf("events = %+v", store.events)
			}
			eventPayload := decodeObject(t, store.events[0].Payload)
			if eventPayload["run_id"] != runID.String() || eventPayload["waitpoint_id"] != waitpointID.String() {
				t.Fatalf("event identity = %+v", eventPayload)
			}
			tt.assertEvent(t, eventPayload)
		})
	}
}

func TestRespondWaitpointReplayIsIdempotent(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	waitpointID := uuid.Must(uuid.NewV7())
	store := &fakeStore{
		run: db.Run{
			ID:        pgvalue.UUID(runID),
			OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
			TaskID:    "deploy",
			Status:    db.RunStatusWaiting,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
		waitpoint: fakeWaitpoint{
			ID:          pgvalue.UUID(waitpointID),
			OrgID:       pgvalue.UUID(dbtest.DefaultOrgID),
			RunID:       pgvalue.UUID(runID),
			Kind:        db.WaitpointKindHuman,
			Status:      db.RunWaitStatusWaiting,
			RequestedAt: testTime(),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}})
	for i, wantStatus := range []int{http.StatusNoContent, http.StatusAccepted} {
		req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/"+waitpointID.String()+"/respond", strings.NewReader(`{"value":{"action":"approve"}}`))
		req.Header.Set("authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != wantStatus {
			t.Fatalf("respond %d status = %d body=%s", i+1, rec.Code, rec.Body.String())
		}
	}
	if len(store.waitpointResponses) != 1 {
		t.Fatalf("waitpoint responses = %+v", store.waitpointResponses)
	}
	if len(store.events) != 1 {
		t.Fatalf("events = %+v", store.events)
	}
}

func TestRespondWaitpointRejectsNonRespondableKindInResolvePath(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	waitpointID := uuid.Must(uuid.NewV7())
	store := &fakeStore{
		run: db.Run{
			ID:        pgvalue.UUID(runID),
			OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
			TaskID:    "deploy",
			Status:    db.RunStatusWaiting,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
		waitpoint: fakeWaitpoint{
			ID:          pgvalue.UUID(waitpointID),
			OrgID:       pgvalue.UUID(dbtest.DefaultOrgID),
			RunID:       pgvalue.UUID(runID),
			Kind:        db.WaitpointKindDelay,
			Status:      db.RunWaitStatusWaiting,
			RequestedAt: testTime(),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}})
	req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/"+waitpointID.String()+"/respond", strings.NewReader(`{"value":{"action":"approve"}}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("resolve status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.waitpointResponses) != 0 {
		t.Fatalf("waitpoint responses = %+v", store.waitpointResponses)
	}
}

func TestResolveWaitpointReturnsAcceptedWhenRunWaitIsNotResuming(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	waitpointID := uuid.Must(uuid.NewV7())
	store := &fakeStore{
		run: db.Run{
			ID:        pgvalue.UUID(runID),
			OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
			TaskID:    "deploy",
			Status:    db.RunStatusWaiting,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
		waitpoint: fakeWaitpoint{
			ID:          pgvalue.UUID(waitpointID),
			OrgID:       pgvalue.UUID(dbtest.DefaultOrgID),
			RunID:       pgvalue.UUID(runID),
			Kind:        db.WaitpointKindHuman,
			Status:      db.RunWaitStatusWaiting,
			RequestedAt: testTime(),
		},
		resolveStatus: db.RunWaitStatusWaiting,
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}})
	req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/"+waitpointID.String()+"/respond", strings.NewReader(`{"value":{"action":"approve"},"external_subject":"reviewer@example.test","metadata":{"source":"api"}}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("resolve status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.waitpointResponses) != 1 {
		t.Fatalf("responses = %+v", store.waitpointResponses)
	}
	response := store.waitpointResponses[0]
	if response.ExternalSubject.String != "reviewer@example.test" || string(response.Metadata) != `{"source":"api"}` {
		t.Fatalf("response audit fields = external_subject:%+v metadata:%s", response.ExternalSubject, response.Metadata)
	}
	if store.waitpoint.Status != db.RunWaitStatusWaiting || store.run.Status != db.RunStatusWaiting || len(store.events) != 0 {
		t.Fatalf("waitpoint=%+v run=%+v events=%+v", store.waitpoint, store.run, store.events)
	}
}

func fakeWaitpointRow(waitpoint fakeWaitpoint) db.GetPendingWaitpointForRunRow {
	return db.GetPendingWaitpointForRunRow{
		ID:             waitpoint.ID,
		RunWaitID:      waitpointRunWaitID(waitpoint),
		OrgID:          waitpoint.OrgID,
		RunID:          waitpoint.RunID,
		SessionID:      waitpoint.SessionID,
		CheckpointID:   waitpoint.CheckpointID,
		CorrelationID:  waitpoint.CorrelationID,
		Kind:           waitpoint.Kind,
		Request:        waitpoint.Request,
		DisplayText:    waitpoint.DisplayText,
		TimeoutSeconds: waitpoint.TimeoutSeconds,
		PolicyName:     waitpoint.PolicyName,
		PolicySnapshot: waitpoint.PolicySnapshot,
		Status:         waitpoint.Status,
		ResolutionKind: waitpoint.ResolutionKind,
		Resolution:     waitpoint.Resolution,
		CreatedAt:      waitpoint.CreatedAt,
		RequestedAt:    waitpoint.RequestedAt,
		ResolvedAt:     waitpoint.ResolvedAt,
	}
}

func fakeWaitpointListRow(waitpoint fakeWaitpoint) db.ListPendingWaitpointsForRunsRow {
	return db.ListPendingWaitpointsForRunsRow{
		ID:             waitpoint.ID,
		RunWaitID:      waitpointRunWaitID(waitpoint),
		OrgID:          waitpoint.OrgID,
		RunID:          waitpoint.RunID,
		SessionID:      waitpoint.SessionID,
		CheckpointID:   waitpoint.CheckpointID,
		CorrelationID:  waitpoint.CorrelationID,
		Kind:           waitpoint.Kind,
		Request:        waitpoint.Request,
		DisplayText:    waitpoint.DisplayText,
		TimeoutSeconds: waitpoint.TimeoutSeconds,
		PolicyName:     waitpoint.PolicyName,
		PolicySnapshot: waitpoint.PolicySnapshot,
		Status:         waitpoint.Status,
		ResolutionKind: waitpoint.ResolutionKind,
		Resolution:     waitpoint.Resolution,
		CreatedAt:      waitpoint.CreatedAt,
		RequestedAt:    waitpoint.RequestedAt,
		ResolvedAt:     waitpoint.ResolvedAt,
	}
}

func waitpointRunWaitID(waitpoint fakeWaitpoint) pgtype.UUID {
	if waitpoint.RunWaitID.Valid {
		return waitpoint.RunWaitID
	}
	return waitpoint.ID
}

func (f *fakeStore) CreateWaitpointForExecution(_ context.Context, arg db.CreateWaitpointForExecutionParams) (db.CreateWaitpointForExecutionRow, error) {
	if f.sessionID != arg.SessionID || f.executionWorkerInstanceID != arg.WorkerInstanceID {
		return db.CreateWaitpointForExecutionRow{}, pgx.ErrNoRows
	}
	f.waitpoint = fakeWaitpoint{
		ID:             arg.ID,
		RunWaitID:      arg.RunWaitID,
		OrgID:          arg.OrgID,
		RunID:          arg.RunID,
		SessionID:      arg.SessionID,
		CheckpointID:   arg.CheckpointID,
		CorrelationID:  arg.CorrelationID,
		Kind:           arg.Kind,
		Request:        arg.Request,
		DisplayText:    arg.DisplayText,
		TimeoutSeconds: arg.TimeoutSeconds,
		PolicyName:     arg.PolicyName,
		PolicySnapshot: arg.PolicySnapshot,
		Status:         db.RunWaitStatusOpening,
		RequestedAt:    testTime(),
	}
	return db.CreateWaitpointForExecutionRow{
		ID:             f.waitpoint.ID,
		RunWaitID:      waitpointRunWaitID(f.waitpoint),
		OrgID:          f.waitpoint.OrgID,
		RunID:          f.waitpoint.RunID,
		SessionID:      f.waitpoint.SessionID,
		CheckpointID:   f.waitpoint.CheckpointID,
		CorrelationID:  f.waitpoint.CorrelationID,
		Kind:           f.waitpoint.Kind,
		Request:        f.waitpoint.Request,
		DisplayText:    f.waitpoint.DisplayText,
		TimeoutSeconds: f.waitpoint.TimeoutSeconds,
		PolicyName:     f.waitpoint.PolicyName,
		PolicySnapshot: f.waitpoint.PolicySnapshot,
		Status:         f.waitpoint.Status,
		ResolutionKind: f.waitpoint.ResolutionKind,
		Resolution:     f.waitpoint.Resolution,
		RequestedAt:    f.waitpoint.RequestedAt,
		ResolvedAt:     f.waitpoint.ResolvedAt,
	}, nil
}

func (f *fakeStore) GetPendingWaitpointForRun(_ context.Context, arg db.GetPendingWaitpointForRunParams) (db.GetPendingWaitpointForRunRow, error) {
	if f.waitpoint.ID.Valid && f.waitpoint.OrgID == arg.OrgID && f.waitpoint.RunID == arg.RunID && f.waitpoint.Status == db.RunWaitStatusWaiting {
		return fakeWaitpointRow(f.waitpoint), nil
	}
	return db.GetPendingWaitpointForRunRow{}, pgx.ErrNoRows
}

func (f *fakeStore) ListPendingWaitpointsForRuns(_ context.Context, arg db.ListPendingWaitpointsForRunsParams) ([]db.ListPendingWaitpointsForRunsRow, error) {
	if !f.waitpoint.ID.Valid || f.waitpoint.OrgID != arg.OrgID || f.waitpoint.Status != db.RunWaitStatusWaiting {
		return nil, nil
	}
	for _, runID := range arg.RunIds {
		if f.waitpoint.RunID == runID {
			return []db.ListPendingWaitpointsForRunsRow{fakeWaitpointListRow(f.waitpoint)}, nil
		}
	}
	return nil, nil
}

func (f *fakeStore) GetWaitpointForResponseTokenCreation(_ context.Context, arg db.GetWaitpointForResponseTokenCreationParams) (db.GetWaitpointForResponseTokenCreationRow, error) {
	if f.waitpoint.ID.Valid && f.waitpoint.OrgID == arg.OrgID && f.waitpoint.ID == arg.WaitpointID && f.waitpoint.Status == db.RunWaitStatusWaiting {
		return db.GetWaitpointForResponseTokenCreationRow{ID: f.waitpoint.ID, OrgID: f.waitpoint.OrgID, ProjectID: f.waitpoint.ProjectID, EnvironmentID: f.waitpoint.EnvironmentID, Kind: f.waitpoint.Kind}, nil
	}
	return db.GetWaitpointForResponseTokenCreationRow{}, pgx.ErrNoRows
}

func (f *fakeStore) GetWaitpointForRespond(_ context.Context, arg db.GetWaitpointForRespondParams) (db.Waitpoint, error) {
	if !f.waitpoint.ID.Valid || f.waitpoint.OrgID != arg.OrgID || f.waitpoint.ID != arg.WaitpointID {
		return db.Waitpoint{}, pgx.ErrNoRows
	}
	projectID := f.waitpoint.ProjectID
	if !projectID.Valid {
		projectID = testProjectID()
	}
	environmentID := f.waitpoint.EnvironmentID
	if !environmentID.Valid {
		environmentID = testEnvironmentID()
	}
	return db.Waitpoint{
		ID:            f.waitpoint.ID,
		OrgID:         f.waitpoint.OrgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Kind:          f.waitpoint.Kind,
		Status:        db.WaitpointStatusPending,
		Request:       f.waitpoint.Request,
		DisplayText:   f.waitpoint.DisplayText,
		CreatedAt:     f.waitpoint.CreatedAt,
	}, nil
}

func (f *fakeStore) ListWaitpointDeliveries(context.Context, db.ListWaitpointDeliveriesParams) ([]db.WaitpointDelivery, error) {
	return nil, nil
}

func (f *fakeStore) ListWaitpointDeliveriesForRunWaits(_ context.Context, arg db.ListWaitpointDeliveriesForRunWaitsParams) ([]db.WaitpointDelivery, error) {
	runWaitIDs := make(map[pgtype.UUID]struct{}, len(arg.RunWaitIds))
	for _, runWaitID := range arg.RunWaitIds {
		runWaitIDs[runWaitID] = struct{}{}
	}
	var deliveries []db.WaitpointDelivery
	for _, delivery := range f.waitpointDeliveries {
		if delivery.OrgID != arg.OrgID {
			continue
		}
		if _, ok := runWaitIDs[delivery.RunWaitID]; !ok {
			continue
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, nil
}

func (f *fakeStore) ResolveWaitpoint(_ context.Context, arg db.ResolveWaitpointParams) (db.ResolveWaitpointRow, error) {
	if !f.waitpoint.ID.Valid || f.waitpoint.OrgID != arg.OrgID || f.waitpoint.ID != arg.ID || f.waitpoint.Kind != arg.Kind {
		return db.ResolveWaitpointRow{}, pgx.ErrNoRows
	}
	if !f.waitpoint.ResolutionKind.Valid {
		if f.waitpoint.Status != db.RunWaitStatusWaiting {
			return db.ResolveWaitpointRow{}, pgx.ErrNoRows
		}
		f.waitpoint.ResolutionKind = arg.ResolutionKind
		f.waitpoint.Output = arg.Output
		f.waitpoint.Resolution = arg.Resolution
		f.waitpoint.ResolvedAt = testTime()
	}
	return db.ResolveWaitpointRow{
		ID:             f.waitpoint.ID,
		OrgID:          f.waitpoint.OrgID,
		ProjectID:      f.waitpoint.ProjectID,
		EnvironmentID:  f.waitpoint.EnvironmentID,
		Kind:           f.waitpoint.Kind,
		Request:        f.waitpoint.Request,
		DisplayText:    f.waitpoint.DisplayText,
		Status:         db.WaitpointStatusCompleted,
		ResolutionKind: f.waitpoint.ResolutionKind,
		Resolution:     f.waitpoint.Resolution,
		CompletedAt:    testTime(),
		UpdatedAt:      testTime(),
	}, nil
}

func (f *fakeStore) UnblockRunWaitsForWaitpoint(_ context.Context, arg db.UnblockRunWaitsForWaitpointParams) ([]db.UnblockRunWaitsForWaitpointRow, error) {
	if !f.waitpoint.ID.Valid || f.waitpoint.OrgID != arg.OrgID || f.waitpoint.ID != arg.WaitpointID || f.waitpoint.Status != db.RunWaitStatusWaiting || !f.waitpoint.ResolutionKind.Valid {
		return nil, nil
	}
	if f.resolveStatus == db.RunWaitStatusWaiting {
		return nil, nil
	}
	f.waitpoint.Status = db.RunWaitStatusResuming
	f.run.Status = db.RunStatusQueued
	f.run.CurrentSessionID = pgtype.UUID{}
	f.run.UpdatedAt = testTime()
	payload, _ := json.Marshal(map[string]any{
		"run_id":          pgvalue.MustUUIDValue(f.waitpoint.RunID).String(),
		"waitpoint_id":    pgvalue.MustUUIDValue(f.waitpoint.ID).String(),
		"kind":            string(f.waitpoint.Kind),
		"resolution_kind": f.waitpoint.ResolutionKind.String,
		"result":          json.RawMessage(f.waitpoint.Output),
	})
	f.events = append(f.events, db.Event{Seq: int64(len(f.events) + 1), OrgID: arg.OrgID, RunID: f.waitpoint.RunID, Kind: "waitpoint.resolved", Payload: payload, CreatedAt: testTime()})
	return []db.UnblockRunWaitsForWaitpointRow{{ID: f.waitpoint.ID, RunWaitID: waitpointRunWaitID(f.waitpoint), OrgID: f.waitpoint.OrgID, RunID: f.waitpoint.RunID, Status: f.waitpoint.Status}}, nil
}

func (f *fakeStore) RecordWaitpointResponse(_ context.Context, arg db.RecordWaitpointResponseParams) (db.RecordWaitpointResponseRow, error) {
	if !f.waitpoint.ID.Valid || f.waitpoint.OrgID != arg.OrgID || f.waitpoint.ID != arg.WaitpointID || f.waitpoint.Kind != arg.Kind {
		return db.RecordWaitpointResponseRow{}, pgx.ErrNoRows
	}
	for _, existing := range f.waitpointResponses {
		if existing.ResponseKey == arg.ResponseKey {
			if existing.RequestHash != arg.RequestHash {
				return db.RecordWaitpointResponseRow{}, pgx.ErrNoRows
			}
			return fakeWaitpointResponseRow(f.waitpoint, existing), nil
		}
	}
	if f.waitpoint.Status != db.RunWaitStatusWaiting {
		return db.RecordWaitpointResponseRow{}, pgx.ErrNoRows
	}
	f.waitpointResponses = append(f.waitpointResponses, arg)
	return fakeWaitpointResponseRow(f.waitpoint, arg), nil
}

func fakeWaitpointResponseRow(waitpoint fakeWaitpoint, arg db.RecordWaitpointResponseParams) db.RecordWaitpointResponseRow {
	return db.RecordWaitpointResponseRow{
		ID: arg.ID, OrgID: arg.OrgID, ProjectID: waitpoint.ProjectID, EnvironmentID: waitpoint.EnvironmentID, WaitpointID: arg.WaitpointID,
		ResponseKey: arg.ResponseKey, RequestHash: arg.RequestHash, Action: arg.Action, ResolutionKind: arg.ResolutionKind,
		Resolution: arg.Resolution, EventPayload: arg.EventPayload, CompletedByPrincipal: arg.CompletedByPrincipal,
		CompletedVia: arg.CompletedVia, ExternalSubject: arg.ExternalSubject, Metadata: arg.Metadata,
		CreatedAt: testTime(), UpdatedAt: testTime(),
	}
}

func (f *fakeStore) RecordAndResolveWaitpoint(ctx context.Context, record db.RecordWaitpointResponseParams, resolve db.ResolveWaitpointParams) ([]db.UnblockRunWaitsForWaitpointRow, error) {
	if _, err := f.RecordWaitpointResponse(ctx, record); err != nil {
		return nil, err
	}
	if _, err := f.ResolveWaitpoint(ctx, resolve); err != nil {
		return nil, err
	}
	return f.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{OrgID: resolve.OrgID, WaitpointID: resolve.ID})
}

func (f *fakeStore) ExpireDuePendingWaitpoints(context.Context, pgtype.UUID) error {
	if f.waitpoint.ID.Valid && f.waitpoint.Status == db.RunWaitStatusWaiting && f.waitpoint.TimeoutSeconds.Valid && f.run.Status == db.RunStatusWaiting && !f.run.CurrentSessionID.Valid {
		if !testTime().Time.Before(f.waitpoint.RequestedAt.Time.Add(time.Duration(f.waitpoint.TimeoutSeconds.Int32) * time.Second)) {
			f.waitpoint.Status = db.RunWaitStatusResuming
			f.waitpoint.ResolutionKind = pgtype.Text{String: "timed_out", Valid: true}
			f.waitpoint.Resolution = []byte(`{"at":"2026-05-08T12:00:00Z"}`)
			f.waitpoint.ResolvedAt = testTime()
			f.run.Status = db.RunStatusQueued
		}
	}
	return nil
}
