package control

import (
	"archive/tar"
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
	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkerLeaseRejectsPlacementCapabilities(t *testing.T) {
	server := &Server{log: discardTestLogger(), db: &workerContractStore{}}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/lease", strings.NewReader(`{"capabilities":{"supports_run":true}}`))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(uuid.Must(uuid.NewV7()))))
	rec := httptest.NewRecorder()

	server.workerLease(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerStartRejectsLeaseFromAnotherWorkerEpoch(t *testing.T) {
	leaseWorkerID := uuid.Must(uuid.NewV7())
	requestWorkerID := uuid.Must(uuid.NewV7())
	request := api.WorkerStartRequest{Lease: finalWorkerRunLease(leaseWorkerID)}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{log: discardTestLogger(), db: &workerContractStore{}}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/start", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(requestWorkerID)))
	rec := httptest.NewRecorder()

	server.workerStart(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerRunEntrypointRejectsUnknownFields(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	server := &Server{log: discardTestLogger(), db: &workerContractStore{}}
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/worker/leases/entrypoint",
		strings.NewReader(`{"lease":{},"entrypoint_kind":"task","entrypoint_declared_id":"compile","unknown":true}`),
	)
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(workerID)))
	rec := httptest.NewRecorder()

	server.workerEnterRunEntrypoint(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerRunEntrypointRejectsInvalidEntrypoint(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	lease := finalWorkerRunLease(workerID)
	body, err := json.Marshal(api.WorkerRunEntrypointRequest{
		Lease: api.WorkerRunLeaseReceipt{
			ID:            lease.ID,
			LeaseSequence: lease.LeaseSequence,
		},
		EntrypointKind:       "workflow",
		EntrypointDeclaredID: "compile",
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{log: discardTestLogger(), db: &workerContractStore{}}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/entrypoint", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(workerID)))
	rec := httptest.NewRecorder()

	server.workerEnterRunEntrypoint(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerReleaseRejectsUnknownFields(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	lease := finalWorkerRunLease(workerID)
	body, err := json.Marshal(map[string]any{
		"lease":             lease,
		"result":            map[string]any{"kind": "completed", "active_duration_ms": 1},
		"unknown_authority": "removed-authority",
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{log: discardTestLogger(), db: &workerContractStore{}}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/release", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(workerID)))
	rec := httptest.NewRecorder()

	server.workerRelease(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerReleaseRetriesCanonicalTerminalResponseAfterResponseLoss(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	lease := finalWorkerRunLease(workerID)
	result := api.WorkerReleaseResult{Kind: "completed", ActiveDurationMs: 42}
	fingerprint, err := terminalRequestFingerprint("run.release", result)
	if err != nil {
		t.Fatal(err)
	}
	store := &workerResponseLossStore{runTerminal: db.GetRunLeaseTerminalResultRow{
		RunStatus:                  db.RunStatusSucceeded,
		TerminalRequestFingerprint: pgtype.Text{String: fingerprint, Valid: true},
	}}
	requestBody, err := json.Marshal(api.WorkerReleaseRequest{Lease: lease, Result: result})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{log: discardTestLogger(), db: store}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/release", bytes.NewReader(requestBody))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(workerID)))
	rec := httptest.NewRecorder()

	server.workerRelease(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"succeeded"`) {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.releaseCalls != 0 {
		t.Fatalf("release side effects repeated %d times", store.releaseCalls)
	}
	store.runTerminal.TerminalRequestFingerprint.String = "sha256:different-terminal-payload"
	req = httptest.NewRequest(http.MethodPost, "/api/worker/leases/release", bytes.NewReader(requestBody))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(workerID)))
	rec = httptest.NewRecorder()
	server.workerRelease(rec, req)
	if rec.Code != http.StatusConflict || store.releaseCalls != 0 {
		t.Fatalf("different terminal payload status=%d side_effects=%d body=%s", rec.Code, store.releaseCalls, rec.Body.String())
	}
}

func TestWorkerStartRetriesCanonicalStartedResponseAfterResponseLoss(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	lease := finalWorkerRunLease(workerID)
	expiresAt := time.Now().Add(5 * time.Minute).UTC()
	store := &workerResponseLossStore{startedRun: db.RunLease{
		State: db.RunLeaseStateRunning, ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}}
	body, err := json.Marshal(api.WorkerStartRequest{Lease: lease})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{log: discardTestLogger(), db: store}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/start", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(workerID)))
	rec := httptest.NewRecorder()

	server.workerStart(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"running"`) {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.startCalls != 0 {
		t.Fatalf("start mutation repeated %d times", store.startCalls)
	}
}

func TestWorkerBuildTerminalRetriesAfterResponseLoss(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	lease := finalWorkerBuildLease(workerID)
	lease.ExpiresAt = time.Now().Add(-time.Minute)
	result, err := deployment.CanonicalBuildResult(deployment.BuildResult{
		FormatVersion: deployment.BuildResultFormatVersion,
		Outcome:       deployment.BuildOutcomeFailed,
		Failed: &deployment.BuildFailed{Error: deployment.BuildError{
			ReasonCode: deployment.BuildFailureInvalidPlan,
			Message:    "deterministic build failure",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	completeFingerprint := deploymentBuildResultFingerprint(result)
	store := &workerResponseLossStore{buildTerminal: db.GetDeploymentBuildTerminalResultRow{
		State:                      db.DeploymentBuildLeaseStateFailed,
		TerminalRequestFingerprint: pgtype.Text{String: completeFingerprint, Valid: true},
	}}
	server := &Server{log: discardTestLogger(), db: store}
	body, err := json.Marshal(struct {
		Lease  api.WorkerDeploymentBuildLease `json:"lease"`
		Result json.RawMessage                `json:"result"`
	}{Lease: lease, Result: result})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/deployment-builds/complete", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(workerID)))
	rec := httptest.NewRecorder()

	server.workerCompleteDeploymentBuild(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"failed"`) {
		t.Fatalf("completion retry status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.completeCalls != 0 || store.failCalls != 0 {
		t.Fatalf("build side effects repeated: complete=%d fail=%d", store.completeCalls, store.failCalls)
	}

	reason := "insufficient_capacity"
	rejectFingerprint, err := terminalRequestFingerprint("deployment_build.reject", struct {
		ReasonCode string          `json:"reason_code"`
		Error      json.RawMessage `json:"error,omitempty"`
	}{ReasonCode: reason})
	if err != nil {
		t.Fatal(err)
	}
	store.buildTerminal = db.GetDeploymentBuildTerminalResultRow{
		State:                      db.DeploymentBuildLeaseStateRejected,
		TerminalRequestFingerprint: pgtype.Text{String: rejectFingerprint, Valid: true},
	}
	body, err = json.Marshal(api.WorkerDeploymentBuildRejectRequest{Lease: lease, ReasonCode: reason})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/worker/deployment-builds/reject", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(workerID)))
	rec = httptest.NewRecorder()

	server.workerRejectDeploymentBuild(rec, req)

	if rec.Code != http.StatusNoContent || store.rejectCalls != 0 {
		t.Fatalf("reject retry status=%d side_effects=%d body=%s", rec.Code, store.rejectCalls, rec.Body.String())
	}
}

func TestWorkerBuildInvalidOutputTerminalizesLease(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	lease := finalWorkerBuildLease(workerID)
	raw := json.RawMessage(
		`{"error":{"message":"bad","reasonCode":"invalid_plan"},"formatVersion":0,"outcome":"failed","outcome":"failed"}`,
	)
	store := &invalidBuildOutputStore{
		locked: db.LockDeploymentBuildTerminalFenceRow{
			State:                db.DeploymentBuildLeaseStateRunning,
			DeploymentStatus:     db.DeploymentStatusBuilding,
			CurrentBuildLeaseID:  pgvalue.UUID(uuid.MustParse(lease.ID)),
			ExpiresAt:            pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
			BuildArchitecture:    string(deployment.ArchitectureX8664),
			BuildContractVersion: deployment.ProgramBuildContractVersion,
		},
	}
	body, err := json.Marshal(struct {
		Lease  api.WorkerDeploymentBuildLease `json:"lease"`
		Result json.RawMessage                `json:"result"`
	}{
		Lease:  lease,
		Result: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{log: discardTestLogger(), db: store}
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/worker/deployment-builds/complete",
		bytes.NewReader(body),
	)
	req = req.WithContext(context.WithValue(
		req.Context(),
		workerContextKey{},
		finalWorkerActor(workerID),
	))
	rec := httptest.NewRecorder()

	server.workerCompleteDeploymentBuild(rec, req)

	if rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"status":"failed"`) {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.failCalls != 1 {
		t.Fatalf("fail calls = %d", store.failCalls)
	}
	if !store.failure.ReasonCode.Valid ||
		store.failure.ReasonCode.String != "output_invalid" {
		t.Fatalf("reason = %+v", store.failure.ReasonCode)
	}
	if store.failure.TerminalRequestFingerprint == "" {
		t.Fatal("terminal request fingerprint was not recorded")
	}
	if !store.tx.committed || store.tx.rolledBack {
		t.Fatalf(
			"transaction committed=%v rolled_back=%v",
			store.tx.committed,
			store.tx.rolledBack,
		)
	}
}

func TestWorkerBuildInstallsSuccessfulAuthority(t *testing.T) {
	for _, programBacked := range []bool{true, false} {
		name := "workspace-only"
		if programBacked {
			name = "program-backed"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newSuccessfulBuildFixture(t, programBacked)
			server := &Server{
				log:          discardTestLogger(),
				db:           fixture.store,
				cas:          fixture.cas,
				buildPolicy:  fixture.policy,
				managerStore: fixture.manager,
			}

			rec := completeWorkerBuild(
				t,
				server,
				fixture.workerID,
				fixture.lease,
				fixture.result,
			)

			if rec.Code != http.StatusOK ||
				!strings.Contains(rec.Body.String(), `"status":"deployed"`) {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			if !fixture.store.completed {
				t.Fatal("deployment was not completed")
			}
			if len(fixture.store.definitions) !=
				len(fixture.result.Succeeded.Plan.Definitions) {
				t.Fatalf(
					"definitions = %d, want %d",
					len(fixture.store.definitions),
					len(fixture.result.Succeeded.Plan.Definitions),
				)
			}
			if len(fixture.store.completion.QueueConfig) == 0 {
				t.Fatal("queue configuration was not installed")
			}
			if programBacked {
				if !fixture.store.completion.ProgramCodeArtifactID.Valid ||
					!fixture.store.completion.ProgramDependencyArtifactID.Valid ||
					!fixture.store.completion.ProgramArchitecture.Valid {
					t.Fatalf("program authority = %+v", fixture.store.completion)
				}
			} else if fixture.store.completion.ProgramCodeArtifactID.Valid ||
				fixture.store.completion.ProgramDependencyArtifactID.Valid ||
				fixture.store.completion.ProgramArchitecture.Valid {
				t.Fatalf(
					"workspace-only build installed Program authority: %+v",
					fixture.store.completion,
				)
			}
		})
	}
}

func TestWorkerBuildRejectsCASMismatchBeforeAuthorityWrites(t *testing.T) {
	fixture := newSuccessfulBuildFixture(t, false)
	image := fixture.result.Succeeded.WorkspaceImages[0].Artifact
	fixture.cas.objects[image.Digest] = cas.Object{
		Digest:    image.Digest,
		SizeBytes: image.SizeBytes + 1,
		MediaType: image.MediaType,
	}
	server := &Server{
		log:          discardTestLogger(),
		db:           fixture.store,
		cas:          fixture.cas,
		buildPolicy:  fixture.policy,
		managerStore: fixture.manager,
	}

	rec := completeWorkerBuild(
		t,
		server,
		fixture.workerID,
		fixture.lease,
		fixture.result,
	)

	if rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"status":"failed"`) {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if fixture.store.completed ||
		len(fixture.store.casObjects) != 0 ||
		len(fixture.store.artifacts) != 0 ||
		len(fixture.store.definitions) != 0 {
		t.Fatalf(
			"authority writes after CAS mismatch: completed=%t cas=%d artifacts=%d definitions=%d",
			fixture.store.completed,
			len(fixture.store.casObjects),
			len(fixture.store.artifacts),
			len(fixture.store.definitions),
		)
	}
}

func TestDeploymentBuildResultFingerprintBindsRawBytes(t *testing.T) {
	first := []byte(`{"formatVersion":0}`)
	second := []byte("{\n\"formatVersion\":0}")
	if deploymentBuildResultFingerprint(first) != deploymentBuildResultFingerprint(first) {
		t.Fatal("fingerprint is not stable")
	}
	if deploymentBuildResultFingerprint(first) == deploymentBuildResultFingerprint(second) {
		t.Fatal("fingerprint did not bind exact raw bytes")
	}
}

func TestBoundedWorkerMessagePayload(t *testing.T) {
	payload, err := boundedWorkerMessagePayload(
		strings.Repeat(`"\`, 16<<10),
		"deployment build failed",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) > maxDeploymentBuildTerminalErrorPayloadBytes {
		t.Fatalf("payload bytes = %d", len(payload))
	}
	var decoded workerMessagePayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(decoded.Message) == "" {
		t.Fatal("bounded message is blank")
	}
}

func TestWorkerBuildDeliveryFailureRetriesStoredOutcome(t *testing.T) {
	for _, status := range []db.DeploymentStatus{
		db.DeploymentStatusBuilding,
		db.DeploymentStatusDeployed,
	} {
		t.Run(string(status), func(t *testing.T) {
			workerID := uuid.Must(uuid.NewV7())
			lease := finalWorkerBuildLease(workerID)
			deploymentID := uuid.MustParse(lease.DeploymentID)
			store := &workerResponseLossStore{deliveryFailure: db.FailDeploymentBuildDeliveryRow{
				State:              db.DeploymentBuildLeaseStateLost,
				TerminalReasonCode: pgtype.Text{String: string(api.WorkerDeploymentBuildDeliveryProgramVerifierFailed), Valid: true},
				TerminalAt:         pgtype.Timestamptz{Time: time.Now(), Valid: true},
				LeaseSequence:      lease.LeaseSequence,
				DeploymentID:       pgtype.UUID{Bytes: deploymentID, Valid: true},
				DeploymentStatus:   status,
				Replayed:           true,
			}}
			requestBody, err := json.Marshal(api.WorkerDeploymentBuildDeliveryFailureRequest{
				Lease: lease, ReasonCode: api.WorkerDeploymentBuildDeliveryProgramVerifierFailed,
			})
			if err != nil {
				t.Fatal(err)
			}
			server := &Server{log: discardTestLogger(), db: store}
			req := httptest.NewRequest(http.MethodPost, "/api/worker/deployments/delivery-failed", bytes.NewReader(requestBody))
			req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(workerID)))
			rec := httptest.NewRecorder()

			server.workerDeploymentBuildDeliveryFailed(rec, req)

			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"`+string(status)+`"`) {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			if store.deliveryFailureCalls != 1 {
				t.Fatalf("delivery failure calls = %d", store.deliveryFailureCalls)
			}
		})
	}
}

func TestWorkerBuildDeliveryFailureRejectsInvalidReasonAndFence(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	lease := finalWorkerBuildLease(workerID)
	server := &Server{log: discardTestLogger(), db: &workerResponseLossStore{}}
	for _, test := range []struct {
		name       string
		mutate     func(*api.WorkerDeploymentBuildDeliveryFailureRequest)
		wantStatus int
	}{
		{
			name: "reason",
			mutate: func(request *api.WorkerDeploymentBuildDeliveryFailureRequest) {
				request.ReasonCode = "worker_reported_failure"
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "protocol",
			mutate: func(request *api.WorkerDeploymentBuildDeliveryFailureRequest) {
				request.Lease.WorkerProtocolVersion = "other"
			},
			wantStatus: http.StatusConflict,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := api.WorkerDeploymentBuildDeliveryFailureRequest{Lease: lease, ReasonCode: api.WorkerDeploymentBuildDeliveryProgramVerifierFailed}
			test.mutate(&request)
			body, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/worker/deployments/delivery-failed", bytes.NewReader(body))
			req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(workerID)))
			rec := httptest.NewRecorder()

			server.workerDeploymentBuildDeliveryFailed(rec, req)

			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestWorkerBuildDeliveryFailureRejectsUnknownFields(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	lease := finalWorkerBuildLease(workerID)
	body, err := json.Marshal(map[string]any{
		"lease":      lease,
		"reasonCode": api.WorkerDeploymentBuildDeliveryProgramVerifierFailed,
		"diagnostic": "must remain in worker logs",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &workerResponseLossStore{}
	server := &Server{log: discardTestLogger(), db: store}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/deployments/delivery-failed", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(workerID)))
	rec := httptest.NewRecorder()

	server.workerDeploymentBuildDeliveryFailed(rec, req)

	if rec.Code != http.StatusBadRequest || store.deliveryFailureCalls != 0 {
		t.Fatalf("status=%d delivery_calls=%d body=%s", rec.Code, store.deliveryFailureCalls, rec.Body.String())
	}
}

func finalWorkerRunLease(workerID uuid.UUID) api.WorkerRunLease {
	return api.WorkerRunLease{
		ID: uuid.Must(uuid.NewV7()).String(), OrgID: uuid.Must(uuid.NewV7()).String(), RunID: uuid.Must(uuid.NewV7()).String(),
		WorkerGroupID: "group-1", WorkerInstanceID: workerID.String(), WorkerEpoch: 2, LeaseSequence: 3, SnapshotVersion: 4,
		RuntimeInstanceID: uuid.Must(uuid.NewV7()).String(), NetworkSlotID: uuid.Must(uuid.NewV7()).String(), NetworkSlotGeneration: 5,
		ProtocolVersion: api.CurrentWorkerProtocolVersion, AttemptNumber: 1,
	}
}

func finalWorkerBuildLease(workerID uuid.UUID) api.WorkerDeploymentBuildLease {
	return api.WorkerDeploymentBuildLease{
		ID: uuid.Must(uuid.NewV7()).String(), OrgID: uuid.Must(uuid.NewV7()).String(),
		ProjectID: uuid.Must(uuid.NewV7()).String(), EnvironmentID: uuid.Must(uuid.NewV7()).String(),
		DeploymentID: uuid.Must(uuid.NewV7()).String(), WorkerGroupID: "group-1", WorkerInstanceID: workerID.String(),
		WorkerEpoch: 2, LeaseSequence: 1, WorkerProtocolVersion: api.CurrentWorkerProtocolVersion,
		ExpiresAt: time.Now().Add(time.Minute), RequestedWorkloadDiskBytes: 1, RequestedScratchBytes: 1,
		RequestedCPUMillis: 1000, RequestedMemoryBytes: 1 << 30, RequestedBuildExecutors: 1,
	}
}

func finalWorkerActor(workerID uuid.UUID) workerActor {
	return workerActor{WorkerInstanceID: workerID, WorkerGroupID: "group-1", WorkerEpoch: 2, ProtocolVersion: api.CurrentWorkerProtocolVersion}
}

type successfulBuildFixture struct {
	workerID uuid.UUID
	lease    api.WorkerDeploymentBuildLease
	result   deployment.BuildResult
	store    *successfulBuildStore
	cas      *fakeCAS
	policy   *deployment.BuildPolicy
	manager  ManagerResolver
}

func newSuccessfulBuildFixture(
	t *testing.T,
	programBacked bool,
) successfulBuildFixture {
	t.Helper()
	workerID := uuid.Must(uuid.NewV7())
	lease := finalWorkerBuildLease(workerID)
	source := deploymentSourceTar(
		t,
		[]tar.Header{
			{Name: "bun.lock", Mode: 0o644, Size: int64(len("[lockfile]\n"))},
			{
				Name: "package.json",
				Mode: 0o644,
				Size: int64(len(`{"packageManager":"bun@1.3.10"}`)),
			},
		},
		[]string{"[lockfile]\n", `{"packageManager":"bun@1.3.10"}`},
	)
	sourceDigest := sha256sum.DigestBytes(source)
	selection, err := deployment.InspectSource(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	policy := testBuildPolicy()
	target, err := policy.Current("us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	capsule := controlManagerCapsule()
	capsuleDigest, err := deployment.ManagerCapsuleDigest(capsule)
	if err != nil {
		t.Fatal(err)
	}
	provenance := deployment.BuildProvenance{
		Architecture:         target.Runtime.Architecture,
		BuildContractVersion: target.BuildContractVersion,
		Manager: deployment.ProgramManager{
			CapsuleDigest: capsuleDigest,
			Name:          selection.Manager.Name,
			Version:       selection.Manager.Version,
		},
		RuntimeDigest:           target.Runtime.Digest,
		StandardToolchainDigest: target.StandardToolchainDigest,
		Submitted: deployment.ProgramSubmittedSource{
			LockfileDigest: selection.LockfileDigest,
			LockfileName:   selection.LockfileName,
			SourceDigest:   sourceDigest,
		},
	}
	plan := successfulBuildPlan(programBacked)
	var receipt *deployment.ProgramReceipt
	objects := map[string]cas.Object{}
	if programBacked {
		code := deployment.ProgramDescriptor{
			Digest:    "sha256:" + strings.Repeat("a", 64),
			SizeBytes: 4096,
			MediaType: deployment.ProgramCodeArtifactMediaType,
		}
		dependencies := deployment.ProgramDescriptor{
			Digest:    "sha256:" + strings.Repeat("b", 64),
			SizeBytes: 4096,
			MediaType: deployment.ProgramDependencyArtifactMediaType,
		}
		receipt = &deployment.ProgramReceipt{
			FormatVersion: deployment.ProgramReceiptFormatVersion,
			Code:          code,
			Dependencies:  dependencies,
			Index: deployment.ProgramIndex{
				Architecture:         provenance.Architecture,
				BuildContractVersion: provenance.BuildContractVersion,
				Declarations: []deployment.ProgramDeclaration{{
					Kind:       deployment.DeclarationKindTask,
					DeclaredID: "build",
					Slots:      []deployment.DeclarationSlot{deployment.DeclarationSlotHandler},
				}},
				DependenciesDigest:      dependencies.Digest,
				FormatVersion:           deployment.ProgramIndexFormatVersion,
				Manager:                 provenance.Manager,
				RuntimeAPIVersion:       deployment.RuntimeAPIVersion,
				RuntimeDigest:           provenance.RuntimeDigest,
				StandardToolchainDigest: provenance.StandardToolchainDigest,
				Submitted:               provenance.Submitted,
			},
		}
		objects[code.Digest] = cas.Object{
			Digest: code.Digest, SizeBytes: code.SizeBytes, MediaType: code.MediaType,
		}
		objects[dependencies.Digest] = cas.Object{
			Digest:    dependencies.Digest,
			SizeBytes: dependencies.SizeBytes,
			MediaType: dependencies.MediaType,
		}
	}
	workspaceImages := []deployment.WorkspaceImage{}
	if !programBacked {
		image := deployment.WorkspaceImageArtifact{
			Digest:       "sha256:" + strings.Repeat("c", 64),
			SizeBytes:    4096,
			MediaType:    deployment.WorkspaceImageArtifactMediaType,
			Architecture: target.Runtime.Architecture,
		}
		workspaceImages = append(workspaceImages, deployment.WorkspaceImage{
			DeclaredID: "repo",
			Artifact:   image,
		})
		objects[image.Digest] = cas.Object{
			Digest: image.Digest, SizeBytes: image.SizeBytes, MediaType: image.MediaType,
		}
	}
	result := deployment.BuildResult{
		FormatVersion: deployment.BuildResultFormatVersion,
		Outcome:       deployment.BuildOutcomeSucceeded,
		Succeeded: &deployment.BuildSucceeded{
			Plan:            plan,
			Provenance:      provenance,
			ProgramReceipt:  receipt,
			WorkspaceImages: workspaceImages,
		},
	}
	if err := deployment.ValidateBuildResultContract(result); err != nil {
		t.Fatal(err)
	}
	runtimeDigest, err := deployment.RuntimeDigestBytes(target.Runtime.Digest)
	if err != nil {
		t.Fatal(err)
	}
	toolchainDigest, err := deployment.SHA256DigestBytes(
		target.StandardToolchainDigest,
	)
	if err != nil {
		t.Fatal(err)
	}
	store := &successfulBuildStore{
		locked: db.LockDeploymentBuildTerminalFenceRow{
			ID:                           pgvalue.UUID(uuid.MustParse(lease.ID)),
			OrgID:                        pgvalue.UUID(uuid.MustParse(lease.OrgID)),
			ProjectID:                    pgvalue.UUID(uuid.MustParse(lease.ProjectID)),
			EnvironmentID:                pgvalue.UUID(uuid.MustParse(lease.EnvironmentID)),
			DeploymentID:                 pgvalue.UUID(uuid.MustParse(lease.DeploymentID)),
			State:                        db.DeploymentBuildLeaseStateRunning,
			DeploymentStatus:             db.DeploymentStatusBuilding,
			CurrentBuildLeaseID:          pgvalue.UUID(uuid.MustParse(lease.ID)),
			ExpiresAt:                    pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
			BuildArchitecture:            string(target.Runtime.Architecture),
			BuildRuntimeDigest:           runtimeDigest,
			BuildStandardToolchainDigest: toolchainDigest,
			BuildContractVersion:         target.BuildContractVersion,
			DeploymentSourceDigest:       sourceDigest,
		},
		certification: db.LockDeploymentBuildWorkerCertificationRow{
			RuntimeArch: pgtype.Text{
				String: string(target.Runtime.Architecture),
				Valid:  true,
			},
		},
	}
	return successfulBuildFixture{
		workerID: workerID,
		lease:    lease,
		result:   result,
		store:    store,
		cas: &fakeCAS{
			objects: objects,
			bodies:  map[string][]byte{sourceDigest: source},
		},
		policy: policy,
		manager: managerResolverFunc(func(
			context.Context,
			deployment.ManagerSelector,
		) (deployment.ManagerCapsule, error) {
			return capsule, nil
		}),
	}
}

func successfulBuildPlan(programBacked bool) deployment.BuildPlan {
	if programBacked {
		return deployment.BuildPlan{
			FormatVersion: deployment.BuildPlanFormatVersion,
			Definitions: []deployment.DefinitionInput{{
				Kind:       deployment.DefinitionKindTask,
				DeclaredID: "build",
				Task: &deployment.TaskManifest{
					Payload: deployment.SchemaManifest{Kind: deployment.SchemaKindNone},
					Run: deployment.RunManifest{
						Queue:         "task/build",
						MaxDurationMs: 5000,
						Retry:         deployment.RetryManifest{Enabled: false},
					},
				},
			}},
			Queues: []deployment.QueueInput{{Name: "task/build"}},
		}
	}
	return deployment.BuildPlan{
		FormatVersion: deployment.BuildPlanFormatVersion,
		Definitions: []deployment.DefinitionInput{{
			Kind:       deployment.DefinitionKindWorkspace,
			DeclaredID: "repo",
			Workspace: &deployment.WorkspaceInputManifest{
				ImageBuild: builder.ImageBuild{
					FormatVersion: builder.ImageBuildFormatVersion,
					Root:          "repo",
					Images: []builder.ImageSpec{{
						Key: "repo",
						Platform: builder.ImagePlatform{
							OS:           "linux",
							Architecture: "x86_64",
						},
						Steps: []builder.ImageStep{{
							From: &builder.ImageFrom{Ref: "alpine:3.23"},
						}},
					}},
				},
				Resources: deployment.ResourcesManifest{
					MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 8192,
				},
				Network: deployment.NetworkManifest{
					Internet: true, DenyCIDRs: []string{},
				},
				Architecture: deployment.ArchitectureX8664,
			},
		}},
		Queues: []deployment.QueueInput{},
	}
}

func completeWorkerBuild(
	t *testing.T,
	server *Server,
	workerID uuid.UUID,
	lease api.WorkerDeploymentBuildLease,
	result deployment.BuildResult,
) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := deployment.CanonicalBuildResult(result)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(struct {
		Lease  api.WorkerDeploymentBuildLease `json:"lease"`
		Result json.RawMessage                `json:"result"`
	}{Lease: lease, Result: raw})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/worker/deployments/complete",
		bytes.NewReader(body),
	)
	request = request.WithContext(context.WithValue(
		request.Context(),
		workerContextKey{},
		finalWorkerActor(workerID),
	))
	response := httptest.NewRecorder()
	server.workerCompleteDeploymentBuild(response, request)
	return response
}

func discardTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type workerContractStore struct{ db.Querier }

type workerResponseLossStore struct {
	db.Querier
	runTerminal          db.GetRunLeaseTerminalResultRow
	startedRun           db.RunLease
	buildTerminal        db.GetDeploymentBuildTerminalResultRow
	startCalls           int
	releaseCalls         int
	completeCalls        int
	failCalls            int
	rejectCalls          int
	deliveryFailure      db.FailDeploymentBuildDeliveryRow
	deliveryFailureCalls int
}

type invalidBuildOutputStore struct {
	db.Querier
	locked    db.LockDeploymentBuildTerminalFenceRow
	failure   db.FailDeploymentBuildParams
	failCalls int
	tx        testControlTx
}

type successfulBuildStore struct {
	db.Querier
	locked        db.LockDeploymentBuildTerminalFenceRow
	certification db.LockDeploymentBuildWorkerCertificationRow
	casObjects    []db.UpsertCasObjectParams
	artifacts     []db.CreateArtifactParams
	definitions   []db.CreateDeploymentDefinitionParams
	completion    db.CompleteDeploymentBuildParams
	failure       db.FailDeploymentBuildParams
	completed     bool
	tx            testControlTx
}

func (s *successfulBuildStore) BeginQuerier(
	context.Context,
) (db.Querier, controlTransaction, error) {
	return s, &s.tx, nil
}

func (s *successfulBuildStore) GetDeploymentBuildTerminalResult(
	context.Context,
	db.GetDeploymentBuildTerminalResultParams,
) (db.GetDeploymentBuildTerminalResultRow, error) {
	return db.GetDeploymentBuildTerminalResultRow{}, pgx.ErrNoRows
}

func (s *successfulBuildStore) LockDeploymentBuildWorkerCertification(
	context.Context,
	db.LockDeploymentBuildWorkerCertificationParams,
) (db.LockDeploymentBuildWorkerCertificationRow, error) {
	return s.certification, nil
}

func (s *successfulBuildStore) LockDeploymentBuildTerminalFence(
	context.Context,
	db.LockDeploymentBuildTerminalFenceParams,
) (db.LockDeploymentBuildTerminalFenceRow, error) {
	return s.locked, nil
}

func (s *successfulBuildStore) UpsertCasObject(
	_ context.Context,
	params db.UpsertCasObjectParams,
) (db.CasObject, error) {
	s.casObjects = append(s.casObjects, params)
	return db.CasObject{
		OrgID:     params.OrgID,
		Digest:    params.Digest,
		SizeBytes: params.SizeBytes,
		MediaType: params.MediaType,
	}, nil
}

func (s *successfulBuildStore) CreateArtifact(
	_ context.Context,
	params db.CreateArtifactParams,
) (db.Artifact, error) {
	s.artifacts = append(s.artifacts, params)
	return db.Artifact{
		ID:                        params.ID,
		OrgID:                     params.OrgID,
		ProjectID:                 params.ProjectID,
		EnvironmentID:             params.EnvironmentID,
		Digest:                    params.Digest,
		Kind:                      params.Kind,
		SizeBytes:                 params.SizeBytes,
		MediaType:                 params.MediaType,
		CreatedByWorkerInstanceID: params.CreatedByWorkerInstanceID,
	}, nil
}

func (s *successfulBuildStore) CreateDeploymentDefinition(
	_ context.Context,
	params db.CreateDeploymentDefinitionParams,
) (db.DeploymentDefinition, error) {
	s.definitions = append(s.definitions, params)
	return db.DeploymentDefinition{
		ID:                    params.ID,
		EnvironmentID:         params.EnvironmentID,
		DeploymentID:          params.DeploymentID,
		Kind:                  params.Kind,
		DeclaredID:            params.DeclaredID,
		ManifestVersion:       params.ManifestVersion,
		Manifest:              params.Manifest,
		ManifestDigest:        params.ManifestDigest,
		WorkspaceArchitecture: params.WorkspaceArchitecture,
		ArtifactID:            params.ArtifactID,
	}, nil
}

func (s *successfulBuildStore) CompleteDeploymentBuild(
	_ context.Context,
	params db.CompleteDeploymentBuildParams,
) (db.CompleteDeploymentBuildRow, error) {
	s.completed = true
	s.completion = params
	return db.CompleteDeploymentBuildRow{
		ID:            params.ID,
		OrgID:         params.OrgID,
		ProjectID:     s.locked.ProjectID,
		EnvironmentID: s.locked.EnvironmentID,
		Status:        db.DeploymentStatusDeployed,
	}, nil
}

func (s *successfulBuildStore) FailDeploymentBuild(
	_ context.Context,
	params db.FailDeploymentBuildParams,
) (db.FailDeploymentBuildRow, error) {
	s.failure = params
	return db.FailDeploymentBuildRow{
		ID:            params.ID,
		OrgID:         params.OrgID,
		ProjectID:     s.locked.ProjectID,
		EnvironmentID: s.locked.EnvironmentID,
		Status:        db.DeploymentStatusFailed,
	}, nil
}

func (s *successfulBuildStore) AppendDeploymentEvent(
	context.Context,
	db.AppendDeploymentEventParams,
) (db.AppendDeploymentEventRow, error) {
	return db.AppendDeploymentEventRow{}, nil
}

func (s *invalidBuildOutputStore) BeginQuerier(
	context.Context,
) (db.Querier, controlTransaction, error) {
	return s, &s.tx, nil
}

func (s *invalidBuildOutputStore) GetDeploymentBuildTerminalResult(
	context.Context,
	db.GetDeploymentBuildTerminalResultParams,
) (db.GetDeploymentBuildTerminalResultRow, error) {
	return db.GetDeploymentBuildTerminalResultRow{}, pgx.ErrNoRows
}

func (s *invalidBuildOutputStore) LockDeploymentBuildWorkerCertification(
	context.Context,
	db.LockDeploymentBuildWorkerCertificationParams,
) (db.LockDeploymentBuildWorkerCertificationRow, error) {
	return db.LockDeploymentBuildWorkerCertificationRow{}, nil
}

func (s *invalidBuildOutputStore) LockDeploymentBuildTerminalFence(
	context.Context,
	db.LockDeploymentBuildTerminalFenceParams,
) (db.LockDeploymentBuildTerminalFenceRow, error) {
	return s.locked, nil
}

func (s *invalidBuildOutputStore) FailDeploymentBuild(
	_ context.Context,
	params db.FailDeploymentBuildParams,
) (db.FailDeploymentBuildRow, error) {
	s.failCalls++
	s.failure = params
	return db.FailDeploymentBuildRow{
		ID:            params.ID,
		OrgID:         params.OrgID,
		ProjectID:     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Status:        db.DeploymentStatusFailed,
	}, nil
}

func (s *invalidBuildOutputStore) AppendDeploymentEvent(
	context.Context,
	db.AppendDeploymentEventParams,
) (db.AppendDeploymentEventRow, error) {
	return db.AppendDeploymentEventRow{}, nil
}

func (s *workerResponseLossStore) GetRunLeaseTerminalResult(context.Context, db.GetRunLeaseTerminalResultParams) (db.GetRunLeaseTerminalResultRow, error) {
	return s.runTerminal, nil
}

func (s *workerResponseLossStore) GetStartedRunLease(context.Context, db.GetStartedRunLeaseParams) (db.RunLease, error) {
	return s.startedRun, nil
}

func (s *workerResponseLossStore) StartRunLease(context.Context, db.StartRunLeaseParams) (db.StartRunLeaseRow, error) {
	s.startCalls++
	return db.StartRunLeaseRow{}, pgx.ErrNoRows
}

func (s *workerResponseLossStore) GetDeploymentBuildTerminalResult(context.Context, db.GetDeploymentBuildTerminalResultParams) (db.GetDeploymentBuildTerminalResultRow, error) {
	return s.buildTerminal, nil
}

func (s *workerResponseLossStore) ReleaseRunLease(context.Context, db.ReleaseRunLeaseParams) (db.ReleaseRunLeaseRow, error) {
	s.releaseCalls++
	return db.ReleaseRunLeaseRow{}, pgx.ErrNoRows
}

func (s *workerResponseLossStore) CompleteDeploymentBuild(context.Context, db.CompleteDeploymentBuildParams) (db.CompleteDeploymentBuildRow, error) {
	s.completeCalls++
	return db.CompleteDeploymentBuildRow{}, pgx.ErrNoRows
}

func (s *workerResponseLossStore) FailDeploymentBuild(context.Context, db.FailDeploymentBuildParams) (db.FailDeploymentBuildRow, error) {
	s.failCalls++
	return db.FailDeploymentBuildRow{}, pgx.ErrNoRows
}

func (s *workerResponseLossStore) RejectDeploymentBuildLease(context.Context, db.RejectDeploymentBuildLeaseParams) (db.RejectDeploymentBuildLeaseRow, error) {
	s.rejectCalls++
	return db.RejectDeploymentBuildLeaseRow{}, pgx.ErrNoRows
}

func (s *workerResponseLossStore) FailDeploymentBuildDelivery(context.Context, db.FailDeploymentBuildDeliveryParams) (db.FailDeploymentBuildDeliveryRow, error) {
	s.deliveryFailureCalls++
	return s.deliveryFailure, nil
}
