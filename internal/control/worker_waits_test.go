package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestRunWaitFromCreateHotRunWaitCopiesOwnershipAndResumeFields(t *testing.T) {
	ownerRunID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	resumingAt := pgtype.Timestamptz{Time: time.Unix(123, 0), Valid: true}
	row := db.CreateHotRunWaitRow{
		OwnerRunID: ownerRunID,
		ResumingAt: resumingAt,
	}

	runWait := runWaitFromCreateHotRunWait(row)

	if runWait.OwnerRunID != ownerRunID {
		t.Fatalf("OwnerRunID = %v, want %v", runWait.OwnerRunID, ownerRunID)
	}
	if runWait.ResumingAt != resumingAt {
		t.Fatalf("ResumingAt = %v, want %v", runWait.ResumingAt, resumingAt)
	}
}

func TestCreateWorkerRunWaitRejectsMissingTokenBeforeParking(t *testing.T) {
	store := newRunWaitControlStore()
	store.tokenErr = pgx.ErrNoRows
	server := &Server{db: store}

	_, err := server.createWorkerRunWait(context.Background(), store.scope, tokenWaitRequest(t, uuid.Must(uuid.NewV7())))

	if !errors.Is(err, errTokenNotFound) {
		t.Fatalf("err = %v, want token_not_found", err)
	}
	if store.beginCalls != 0 || store.createHotRunWaitCalls != 0 {
		t.Fatalf("parking side effects before token validation: begin=%d create_hot_run_wait=%d", store.beginCalls, store.createHotRunWaitCalls)
	}
}

func TestCreateWorkerRunWaitRollsBackWhenTypedWaitCreateFails(t *testing.T) {
	store := newRunWaitControlStore()
	txStore := newRunWaitControlStore()
	txStore.createTokenWaitErr = errors.New("typed wait failed")
	store.txStore = txStore
	server := &Server{db: store}

	_, err := server.createWorkerRunWait(context.Background(), store.scope, tokenWaitRequest(t, pgvalue.MustUUIDValue(store.token.ID)))

	if err == nil || !strings.Contains(err.Error(), "typed wait failed") {
		t.Fatalf("err = %v, want typed wait failure", err)
	}
	if store.beginCalls != 1 {
		t.Fatalf("begin calls = %d, want 1", store.beginCalls)
	}
	if txStore.createHotRunWaitCalls != 1 || txStore.createTokenWaitCalls != 1 {
		t.Fatalf("tx side effects = create_hot_run_wait %d create_token_wait %d", txStore.createHotRunWaitCalls, txStore.createTokenWaitCalls)
	}
	if txStore.commitCalls != 0 || txStore.rollbackCalls != 1 {
		t.Fatalf("tx finalization = commit %d rollback %d, want rollback only", txStore.commitCalls, txStore.rollbackCalls)
	}
}

func TestCreateWorkerRunWaitRequiresWorkspaceCaptureBeforeCheckpointReady(t *testing.T) {
	store := newRunWaitControlStore()
	store.scope.DirtyGeneration = 1
	txStore := newRunWaitControlStore()
	store.txStore = txStore
	server := &Server{db: store}

	response, err := server.createWorkerRunWait(context.Background(), store.scope, streamWaitRequest(t, "approval"))

	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if response.RunWaitID == "" {
		t.Fatalf("response = %+v, want run wait handle without checkpoint id", response)
	}
	if store.beginCalls != 1 || txStore.createHotRunWaitCalls != 1 || txStore.createStreamWaitCalls != 1 || txStore.commitCalls != 1 {
		t.Fatalf("parking side effects = begin %d create_hot_run_wait %d create_stream_wait %d commit %d", store.beginCalls, txStore.createHotRunWaitCalls, txStore.createStreamWaitCalls, txStore.commitCalls)
	}
}

func TestCreateWorkerRunWaitRecordsCleanWorkspaceVersion(t *testing.T) {
	store := newRunWaitControlStore()
	txStore := newRunWaitControlStore()
	store.txStore = txStore
	server := &Server{db: store}

	response, err := server.createWorkerRunWait(context.Background(), store.scope, streamWaitRequest(t, "approval"))

	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if response.WorkspaceVersionID != pgvalue.UUIDString(store.scope.WorkspaceCurrentVersionID) {
		t.Fatalf("workspace version id = %q, want %s", response.WorkspaceVersionID, pgvalue.UUIDString(store.scope.WorkspaceCurrentVersionID))
	}
	if txStore.setRunWaitWorkspaceVersionCalls != 1 {
		t.Fatalf("set workspace version calls = %d, want 1", txStore.setRunWaitWorkspaceVersionCalls)
	}
}

func TestCreateWorkerRunWaitDoesNotTreatTimerDurationAsTimeout(t *testing.T) {
	store := newRunWaitControlStore()
	txStore := newRunWaitControlStore()
	store.txStore = txStore
	server := &Server{db: store}

	response, err := server.createWorkerRunWait(context.Background(), store.scope, timerWaitRequest(5))

	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if response.RunWaitID == "" {
		t.Fatalf("response = %+v, want run wait id", response)
	}
	if txStore.createHotRunWaitCalls != 1 || txStore.createTimerWaitCalls != 1 {
		t.Fatalf("side effects = create_hot_run_wait %d create_timer_wait %d, want one each", txStore.createHotRunWaitCalls, txStore.createTimerWaitCalls)
	}
	if txStore.createHotRunWaitParams.TimeoutAt.Valid {
		t.Fatalf("timer hot wait timeout_at = %+v, want unset", txStore.createHotRunWaitParams.TimeoutAt)
	}
	if !txStore.createTimerWaitParams.FireAt.Valid {
		t.Fatal("timer wait fire_at is unset")
	}
	if got := intervalDuration(txStore.createHotRunWaitParams.CheckpointDelay); got != 6*time.Second {
		t.Fatalf("timer checkpoint delay = %s, want 6s", got)
	}
	if response.CheckpointDelayMs != int64((6 * time.Second).Milliseconds()) {
		t.Fatalf("response checkpoint delay ms = %d, want 6000", response.CheckpointDelayMs)
	}
}

func TestCreateWorkerRunWaitKeepsTokenAndStreamHotForInteractiveWindow(t *testing.T) {
	for _, tc := range []struct {
		name        string
		buildWaitFn func(*testing.T, *runWaitControlStore) api.WorkerCreateRunWaitRequest
	}{
		{
			name: "token",
			buildWaitFn: func(t *testing.T, store *runWaitControlStore) api.WorkerCreateRunWaitRequest {
				return tokenWaitRequest(t, pgvalue.MustUUIDValue(store.token.ID))
			},
		},
		{
			name: "stream",
			buildWaitFn: func(t *testing.T, _ *runWaitControlStore) api.WorkerCreateRunWaitRequest {
				return streamWaitRequest(t, "approval")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newRunWaitControlStore()
			txStore := newRunWaitControlStore()
			store.txStore = txStore
			server := &Server{db: store}

			response, err := server.createWorkerRunWait(context.Background(), store.scope, tc.buildWaitFn(t, store))

			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got := intervalDuration(txStore.createHotRunWaitParams.CheckpointDelay); got != interactiveLiveWaitDelay {
				t.Fatalf("checkpoint delay = %s, want %s", got, interactiveLiveWaitDelay)
			}
			if response.CheckpointDelayMs != interactiveLiveWaitDelay.Milliseconds() {
				t.Fatalf("response checkpoint delay ms = %d, want %d", response.CheckpointDelayMs, interactiveLiveWaitDelay.Milliseconds())
			}
		})
	}
}

func TestCreateWorkerRunWaitParksLongTimersAfterDefaultDelay(t *testing.T) {
	store := newRunWaitControlStore()
	txStore := newRunWaitControlStore()
	store.txStore = txStore
	server := &Server{db: store}

	response, err := server.createWorkerRunWait(context.Background(), store.scope, timerWaitRequest(60))

	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := intervalDuration(txStore.createHotRunWaitParams.CheckpointDelay); got != defaultLiveWaitCheckpointDelay {
		t.Fatalf("checkpoint delay = %s, want %s", got, defaultLiveWaitCheckpointDelay)
	}
	if response.CheckpointDelayMs != defaultLiveWaitCheckpointDelay.Milliseconds() {
		t.Fatalf("response checkpoint delay ms = %d, want %d", response.CheckpointDelayMs, defaultLiveWaitCheckpointDelay.Milliseconds())
	}
}

func TestSelectWorkerRunWaitPolicy(t *testing.T) {
	shortTimeout := int32(30)
	longTimeout := int32(600)
	idleTimeout := int32(10)
	for _, tc := range []struct {
		name    string
		request api.WorkerCreateRunWaitRequest
		delay   time.Duration
		reason  workerRunWaitPolicyReason
	}{
		{
			name:    "token without timeout uses interactive window",
			request: api.WorkerCreateRunWaitRequest{Kind: api.WorkerRunWaitKindToken},
			delay:   interactiveLiveWaitDelay,
			reason:  workerRunWaitPolicyInteractiveHotWindow,
		},
		{
			name:    "token idle timeout caps interactive window",
			request: api.WorkerCreateRunWaitRequest{Kind: api.WorkerRunWaitKindToken, IdleTimeoutSeconds: &idleTimeout},
			delay:   10 * time.Second,
			reason:  workerRunWaitPolicyInteractiveIdleTimeout,
		},
		{
			name:    "token timeout within interactive window stays live until timeout",
			request: api.WorkerCreateRunWaitRequest{Kind: api.WorkerRunWaitKindToken, TimeoutSeconds: &shortTimeout},
			delay:   31 * time.Second,
			reason:  workerRunWaitPolicyInteractiveUntilTimeout,
		},
		{
			name:    "stream timeout within idle timeout stays live until timeout",
			request: api.WorkerCreateRunWaitRequest{Kind: api.WorkerRunWaitKindStream, TimeoutSeconds: new(int32(5)), IdleTimeoutSeconds: &idleTimeout},
			delay:   6 * time.Second,
			reason:  workerRunWaitPolicyInteractiveUntilTimeout,
		},
		{
			name:    "stream long timeout uses interactive window",
			request: api.WorkerCreateRunWaitRequest{Kind: api.WorkerRunWaitKindStream, TimeoutSeconds: &longTimeout},
			delay:   interactiveLiveWaitDelay,
			reason:  workerRunWaitPolicyInteractiveHotWindow,
		},
		{
			name:    "short timer stays live through fire time",
			request: api.WorkerCreateRunWaitRequest{Kind: api.WorkerRunWaitKindTimer, TimeoutSeconds: new(int32(3))},
			delay:   4 * time.Second,
			reason:  workerRunWaitPolicyShortTimerLiveUntilFire,
		},
		{
			name:    "long timer parks after default delay",
			request: api.WorkerCreateRunWaitRequest{Kind: api.WorkerRunWaitKindTimer, TimeoutSeconds: new(int32(60))},
			delay:   defaultLiveWaitCheckpointDelay,
			reason:  workerRunWaitPolicyLongTimerPark,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := selectWorkerRunWaitPolicy(tc.request)
			if policy.CheckpointDelay != tc.delay || policy.Reason != tc.reason {
				t.Fatalf("policy = %+v, want delay=%s reason=%s", policy, tc.delay, tc.reason)
			}
		})
	}
}

func TestWorkerMarkCheckpointReadyUsesWorkerCNIProfile(t *testing.T) {
	store := newRunWaitControlStore()
	store.scope.WorkerCniProfile = "helmr/v0"
	workerID := pgvalue.MustUUIDValue(store.scope.WorkerInstanceID)
	runID := pgvalue.MustUUIDValue(store.scope.RunID)
	runLeaseID := pgvalue.MustUUIDValue(store.scope.CurrentRunLeaseID)
	runWaitID := uuid.Must(uuid.NewV7())
	checkpointID := uuid.Must(uuid.NewV7())
	server := &Server{db: store}
	body, err := json.Marshal(api.WorkerCheckpointReadyRequest{
		Lease: api.WorkerRunLease{
			ID:               runLeaseID.String(),
			OrgID:            pgvalue.UUIDString(store.scope.OrgID),
			RunID:            runID.String(),
			WorkerInstanceID: workerID.String(),
			ProtocolVersion:  api.CurrentWorkerProtocolVersion,
			AttemptNumber:    1,
			ExpiresAt:        time.Now().Add(time.Minute),
		},
		WorkerCommandID: 101,
		RunWaitID:       runWaitID.String(),
		CheckpointID:    checkpointID.String(),
		Manifest:        checkpointManifest(),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/checkpoints/ready", strings.NewReader(string(body)))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, workerActor{WorkerInstanceID: workerID}))
	rec := httptest.NewRecorder()

	server.workerMarkCheckpointReady(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	if store.createReadyCheckpointCalls != 1 {
		t.Fatalf("create ready checkpoint calls = %d, want 1", store.createReadyCheckpointCalls)
	}
	if store.createReadyCheckpointParams.CniProfile != "helmr/v0" {
		t.Fatalf("checkpoint cni profile = %q, want helmr/v0", store.createReadyCheckpointParams.CniProfile)
	}
	if store.ackWorkerCommandForRunWaitCalls != 1 || store.ackWorkerCommandForRunWaitParams.ID != 101 {
		t.Fatalf("ack worker command calls = %d params = %+v, want command 101", store.ackWorkerCommandForRunWaitCalls, store.ackWorkerCommandForRunWaitParams)
	}
}

func TestWorkerMarkCheckpointReadyStoresRuntimeSubstrateArtifact(t *testing.T) {
	store := newRunWaitControlStore()
	workerID := pgvalue.MustUUIDValue(store.scope.WorkerInstanceID)
	runID := pgvalue.MustUUIDValue(store.scope.RunID)
	runLeaseID := pgvalue.MustUUIDValue(store.scope.CurrentRunLeaseID)
	runWaitID := uuid.Must(uuid.NewV7())
	checkpointID := uuid.Must(uuid.NewV7())
	runtimeSubstrateArtifactID := uuid.Must(uuid.NewV7())
	manifest := checkpointManifest()
	manifest.RecoveryPoint.Runtime.Substrate = &api.WorkerCheckpointRuntimeSubstrate{
		Digest:     "sha256:substrate-raw",
		Format:     "ext4",
		BuilderABI: "helmr.runtime-substrate.builder.v0",
		LayoutABI:  "helmr.runtime-substrate.layout.v0",
	}
	manifest.RuntimeState.RuntimeSubstrateArtifact = &api.WorkerRuntimeSubstrateArtifact{
		ID:                  runtimeSubstrateArtifactID.String(),
		DeploymentSandboxID: uuid.Must(uuid.NewV7()).String(),
		Artifact: api.CASObject{
			Digest:    "sha256:substrate-encrypted",
			SizeBytes: 1234,
			MediaType: cas.RuntimeSubstrateMediaType,
		},
		SubstrateDigest: "sha256:substrate-raw",
		Format:          "ext4",
		BuilderABI:      "helmr.runtime-substrate.builder.v0",
		LayoutABI:       "helmr.runtime-substrate.layout.v0",
		SizeBytes:       4096,
	}
	server := &Server{db: store}
	body, err := json.Marshal(api.WorkerCheckpointReadyRequest{
		Lease: api.WorkerRunLease{
			ID:               runLeaseID.String(),
			OrgID:            pgvalue.UUIDString(store.scope.OrgID),
			RunID:            runID.String(),
			WorkerInstanceID: workerID.String(),
			ProtocolVersion:  api.CurrentWorkerProtocolVersion,
			AttemptNumber:    1,
			ExpiresAt:        time.Now().Add(time.Minute),
		},
		WorkerCommandID: 103,
		RunWaitID:       runWaitID.String(),
		CheckpointID:    checkpointID.String(),
		Manifest:        manifest,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/checkpoints/ready", strings.NewReader(string(body)))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, workerActor{WorkerInstanceID: workerID}))
	rec := httptest.NewRecorder()

	server.workerMarkCheckpointReady(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	if createArtifactKindSeen(store.createArtifactParams, db.ArtifactKindRuntimeSubstrate) {
		t.Fatalf("create artifact params = %+v, did not expect runtime substrate artifact during checkpoint ready", store.createArtifactParams)
	}
	if got := pgvalue.MustUUIDValue(store.createReadyCheckpointParams.RuntimeSubstrateArtifactID); got != runtimeSubstrateArtifactID {
		t.Fatalf("runtime substrate artifact id = %s, want %s", got, runtimeSubstrateArtifactID)
	}
}

func TestWorkerMarkCheckpointReadyAcceptsCheckpointPhaseStats(t *testing.T) {
	store := newRunWaitControlStore()
	workerID := pgvalue.MustUUIDValue(store.scope.WorkerInstanceID)
	runID := pgvalue.MustUUIDValue(store.scope.RunID)
	runLeaseID := pgvalue.MustUUIDValue(store.scope.CurrentRunLeaseID)
	runWaitID := uuid.Must(uuid.NewV7())
	checkpointID := uuid.Must(uuid.NewV7())
	manifest := checkpointManifest()
	manifest.Phases = []api.WorkerCheckpointPhase{{
		Name:       "pack_scratch_filepack",
		DurationMs: 123,
		Role:       "scratch-disk",
		MediaType:  "application/vnd.helmr.checkpoint.scratch-disk.filepack+zstd",
		ErrorClass: "io",
		Filepack: &api.WorkerCheckpointFilepackStats{
			LogicalBytes:       1024,
			AllocatedBytes:     4096,
			SparseSupported:    new(true),
			SparseDataRanges:   1,
			SparseDataBytes:    512,
			ZeroChunksSkipped:  2,
			EncodedChunks:      1,
			CompressedBytes:    64,
			UnpackWrittenBytes: 512,
		},
	}}
	server := &Server{db: store}
	body, err := json.Marshal(api.WorkerCheckpointReadyRequest{
		Lease: api.WorkerRunLease{
			ID:               runLeaseID.String(),
			OrgID:            pgvalue.UUIDString(store.scope.OrgID),
			RunID:            runID.String(),
			WorkerInstanceID: workerID.String(),
			ProtocolVersion:  api.CurrentWorkerProtocolVersion,
			AttemptNumber:    1,
			ExpiresAt:        time.Now().Add(time.Minute),
		},
		WorkerCommandID: 102,
		RunWaitID:       runWaitID.String(),
		CheckpointID:    checkpointID.String(),
		Manifest:        manifest,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/checkpoints/ready", strings.NewReader(string(body)))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, workerActor{WorkerInstanceID: workerID}))
	rec := httptest.NewRecorder()

	server.workerMarkCheckpointReady(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	var persisted api.WorkerCheckpointManifest
	if err := json.Unmarshal(store.createReadyCheckpointParams.Manifest, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Phases) != 1 || persisted.Phases[0].Filepack == nil {
		t.Fatalf("persisted phases = %+v", persisted.Phases)
	}
	got := persisted.Phases[0]
	if got.Role != "scratch-disk" || got.MediaType == "" || got.ErrorClass != "io" || got.Filepack.LogicalBytes != 1024 ||
		got.Filepack.SparseSupported == nil || !*got.Filepack.SparseSupported {
		t.Fatalf("persisted phase = %+v", got)
	}
}

func TestValidateWorkerWorkspaceCaptureRejectsOversizedArtifacts(t *testing.T) {
	base := api.WorkerWorkspaceArtifact{
		Digest:     "sha256:capture",
		MediaType:  workspace.ArtifactMediaType,
		Encoding:   workspace.ArtifactEncoding,
		SizeBytes:  1,
		EntryCount: 1,
	}
	tooLarge := base
	tooLarge.SizeBytes = workspace.MaxArtifactArchiveBytes + 1
	if err := validateWorkerWorkspaceCapture(tooLarge); err == nil || !strings.Contains(err.Error(), "size_bytes exceeds max") {
		t.Fatalf("oversized capture err = %v, want max size rejection", err)
	}
	tooManyEntries := base
	tooManyEntries.EntryCount = workspace.MaxArtifactEntries + 1
	if err := validateWorkerWorkspaceCapture(tooManyEntries); err == nil || !strings.Contains(err.Error(), "entry_count exceeds max") {
		t.Fatalf("oversized entry count err = %v, want max entry rejection", err)
	}
}

func TestWorkerCreateTokenReturnsConflictWhenLeaseIsNotActive(t *testing.T) {
	store := newRunWaitControlStore()
	store.scopeErr = pgx.ErrNoRows
	workerID := pgvalue.MustUUIDValue(store.scope.WorkerInstanceID)
	server := &Server{db: store}
	body, err := json.Marshal(api.WorkerCreateTokenRequest{
		Lease: api.WorkerRunLease{
			ID:               pgvalue.UUIDString(store.scope.CurrentRunLeaseID),
			OrgID:            pgvalue.UUIDString(store.scope.OrgID),
			RunID:            pgvalue.UUIDString(store.scope.RunID),
			WorkerInstanceID: workerID.String(),
			ProtocolVersion:  api.CurrentWorkerProtocolVersion,
			AttemptNumber:    1,
			ExpiresAt:        time.Now().Add(time.Minute),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/tokens", strings.NewReader(string(body)))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, workerActor{WorkerInstanceID: workerID}))
	rec := httptest.NewRecorder()

	server.workerCreateToken(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s, want 409", rec.Code, rec.Body.String())
	}
}

func TestWorkerMarkCheckpointFailedScopesRunWaitToLease(t *testing.T) {
	store := newRunWaitControlStore()
	store.failRuntimeCheckpointAttemptErr = pgx.ErrNoRows
	workerID := pgvalue.MustUUIDValue(store.scope.WorkerInstanceID)
	runID := pgvalue.MustUUIDValue(store.scope.RunID)
	runLeaseID := pgvalue.MustUUIDValue(store.scope.CurrentRunLeaseID)
	runWaitID := uuid.Must(uuid.NewV7())
	checkpointID := uuid.Must(uuid.NewV7())
	server := &Server{db: store}
	body, err := json.Marshal(api.WorkerCheckpointFailedRequest{
		Lease: api.WorkerRunLease{
			ID:               runLeaseID.String(),
			OrgID:            pgvalue.UUIDString(store.scope.OrgID),
			RunID:            runID.String(),
			WorkerInstanceID: workerID.String(),
			ProtocolVersion:  api.CurrentWorkerProtocolVersion,
			AttemptNumber:    1,
			ExpiresAt:        time.Now().Add(time.Minute),
		},
		WorkerCommandID: 103,
		RunWaitID:       runWaitID.String(),
		CheckpointID:    checkpointID.String(),
		Error:           "checkpoint failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/run-waits/checkpoint-failed", strings.NewReader(string(body)))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, workerActor{WorkerInstanceID: workerID}))
	rec := httptest.NewRecorder()

	server.workerMarkCheckpointFailed(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s, want 409", rec.Code, rec.Body.String())
	}
	if store.failRuntimeCheckpointAttemptCalls != 1 {
		t.Fatalf("fail checkpoint attempt calls = %d, want 1", store.failRuntimeCheckpointAttemptCalls)
	}
	if pgvalue.MustUUIDValue(store.failRuntimeCheckpointAttemptParams.RunID) != runID {
		t.Fatalf("fail checkpoint attempt run id = %s, want %s", pgvalue.MustUUIDValue(store.failRuntimeCheckpointAttemptParams.RunID), runID)
	}
	if pgvalue.MustUUIDValue(store.failRuntimeCheckpointAttemptParams.RunWaitID) != runWaitID {
		t.Fatalf("fail checkpoint attempt run wait id = %s, want %s", pgvalue.MustUUIDValue(store.failRuntimeCheckpointAttemptParams.RunWaitID), runWaitID)
	}
	if pgvalue.MustUUIDValue(store.failRuntimeCheckpointAttemptParams.RuntimeCheckpointID) != checkpointID {
		t.Fatalf("fail checkpoint attempt checkpoint id = %s, want %s", pgvalue.MustUUIDValue(store.failRuntimeCheckpointAttemptParams.RuntimeCheckpointID), checkpointID)
	}
	if pgvalue.MustUUIDValue(store.failRuntimeCheckpointAttemptParams.RunLeaseID) != runLeaseID {
		t.Fatalf("fail checkpoint attempt run lease id = %s, want %s", pgvalue.MustUUIDValue(store.failRuntimeCheckpointAttemptParams.RunLeaseID), runLeaseID)
	}
	if pgvalue.MustUUIDValue(store.failRuntimeCheckpointAttemptParams.WorkerInstanceID) != workerID {
		t.Fatalf("fail checkpoint attempt worker id = %s, want %s", pgvalue.MustUUIDValue(store.failRuntimeCheckpointAttemptParams.WorkerInstanceID), workerID)
	}
	if store.failRuntimeCheckpointAttemptParams.ErrorMessage != "checkpoint failed" {
		t.Fatalf("fail checkpoint attempt error = %q, want checkpoint failed", store.failRuntimeCheckpointAttemptParams.ErrorMessage)
	}
	if store.ackWorkerCommandForRunWaitCalls != 0 {
		t.Fatalf("ack worker command calls = %d, want 0 after failed scope check", store.ackWorkerCommandForRunWaitCalls)
	}
}

func TestWorkerClaimRuntimeCheckpointWaitTreatsAdvancedWaitAsStaleWithoutActiveLease(t *testing.T) {
	store := newRunWaitControlStore()
	workerID := pgvalue.MustUUIDValue(store.scope.WorkerInstanceID)
	runID := pgvalue.MustUUIDValue(store.scope.RunID)
	runLeaseID := pgvalue.MustUUIDValue(store.scope.CurrentRunLeaseID)
	runWaitID := uuid.Must(uuid.NewV7())
	store.scopeErr = pgx.ErrNoRows
	store.runWaitByRun = db.RunWait{
		ID:                         pgvalue.UUID(runWaitID),
		OrgID:                      store.scope.OrgID,
		ProjectID:                  store.scope.ProjectID,
		EnvironmentID:              store.scope.EnvironmentID,
		RunID:                      store.scope.RunID,
		State:                      db.RunWaitStateCheckpointedWaiting,
		OwnerRunLeaseID:            store.scope.CurrentRunLeaseID,
		OwnerWorkerInstanceID:      store.scope.WorkerInstanceID,
		OwnerRunStateVersion:       pgtype.Int8{Int64: 7, Valid: true},
		RuntimeCheckpointID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		WorkspaceVersionID:         store.scope.WorkspaceCurrentVersionID,
		ActiveElapsedMsAtPark:      pgtype.Int8{Int64: 100, Valid: true},
		RuntimeCheckpointStartedAt: pgvalue.Timestamptz(time.Now()),
	}
	server := &Server{db: store}
	body, err := json.Marshal(api.WorkerCheckpointClaimRequest{
		Lease: api.WorkerRunLease{
			ID:               runLeaseID.String(),
			OrgID:            pgvalue.UUIDString(store.scope.OrgID),
			RunID:            runID.String(),
			WorkerInstanceID: workerID.String(),
			ProtocolVersion:  api.CurrentWorkerProtocolVersion,
			AttemptNumber:    1,
			ExpiresAt:        time.Now().Add(time.Minute),
		},
		RunWaitID: runWaitID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/checkpoints/claim", strings.NewReader(string(body)))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, workerActor{WorkerInstanceID: workerID}))
	rec := httptest.NewRecorder()

	server.workerClaimRuntimeCheckpointWait(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	var response api.WorkerCheckpointClaimResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "stale" || response.RunWaitID != runWaitID.String() {
		t.Fatalf("response = %+v, want stale checkpoint claim for %s", response, runWaitID)
	}
}

func TestWorkerMarkCheckpointReadyReplaysAcknowledgedReadyCheckpoint(t *testing.T) {
	store := newRunWaitControlStore()
	store.scopeErr = pgx.ErrNoRows
	store.acknowledgedReadyCheckpoint = true
	workerID := pgvalue.MustUUIDValue(store.scope.WorkerInstanceID)
	runID := pgvalue.MustUUIDValue(store.scope.RunID)
	runLeaseID := pgvalue.MustUUIDValue(store.scope.CurrentRunLeaseID)
	runWaitID := uuid.Must(uuid.NewV7())
	checkpointID := uuid.Must(uuid.NewV7())
	server := &Server{db: store}
	body, err := json.Marshal(api.WorkerCheckpointReadyRequest{
		Lease: api.WorkerRunLease{
			ID:               runLeaseID.String(),
			OrgID:            pgvalue.UUIDString(store.scope.OrgID),
			RunID:            runID.String(),
			WorkerInstanceID: workerID.String(),
			ProtocolVersion:  api.CurrentWorkerProtocolVersion,
			AttemptNumber:    1,
			ExpiresAt:        time.Now().Add(time.Minute),
		},
		WorkerCommandID: 101,
		RunWaitID:       runWaitID.String(),
		CheckpointID:    checkpointID.String(),
		Manifest:        checkpointManifest(),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/checkpoints/ready", strings.NewReader(string(body)))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, workerActor{WorkerInstanceID: workerID}))
	rec := httptest.NewRecorder()

	server.workerMarkCheckpointReady(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	if store.createReadyCheckpointCalls != 0 {
		t.Fatalf("create ready checkpoint calls = %d, want replay without create", store.createReadyCheckpointCalls)
	}
}

type runWaitControlStore struct {
	fakeStore

	scope  db.GetWorkerRunWaitScopeRow
	token  db.Token
	stream db.Stream

	scopeErr           error
	runWaitByRun       db.RunWait
	runWaitByRunErr    error
	tokenErr           error
	streamErr          error
	records            []db.StreamRecord
	createTokenWaitErr error

	txStore                               *runWaitControlStore
	beginCalls                            int
	createHotRunWaitCalls                 int
	createHotRunWaitParams                db.CreateHotRunWaitParams
	createTokenWaitCalls                  int
	createStreamWaitCalls                 int
	createTimerWaitCalls                  int
	createTimerWaitParams                 db.CreateTimerWaitParams
	setRunWaitWorkspaceVersionCalls       int
	failRuntimeCheckpointAttemptCalls     int
	failRuntimeCheckpointAttemptParams    db.FailRuntimeCheckpointAttemptParams
	failRuntimeCheckpointAttemptErr       error
	ackWorkerCommandForRunWaitCalls       int
	ackWorkerCommandForRunWaitParams      db.AcknowledgeWorkerCommandForRunWaitParams
	ackWorkerCommandForRunWaitErr         error
	commitCalls                           int
	rollbackCalls                         int
	createReadyCheckpointCalls            int
	createReadyCheckpointParams           db.CreateReadyRuntimeCheckpointForRunWaitParams
	createArtifactParams                  []db.CreateArtifactParams
	createRuntimeCheckpointArtifactParams []db.CreateRuntimeCheckpointArtifactParams
	acknowledgedReadyCheckpoint           bool
}

func newRunWaitControlStore() *runWaitControlStore {
	orgID := dbtest.DefaultOrgID
	projectID := uuid.Must(uuid.NewV7())
	environmentID := uuid.Must(uuid.NewV7())
	runID := uuid.Must(uuid.NewV7())
	sessionID := uuid.Must(uuid.NewV7())
	workspaceID := uuid.Must(uuid.NewV7())
	workspaceVersionID := uuid.Must(uuid.NewV7())
	leaseID := uuid.Must(uuid.NewV7())
	workerID := uuid.Must(uuid.NewV7())
	streamID := uuid.Must(uuid.NewV7())
	tokenID := uuid.Must(uuid.NewV7())
	return &runWaitControlStore{
		scope: db.GetWorkerRunWaitScopeRow{
			OrgID:                     pgvalue.UUID(orgID),
			ProjectID:                 pgvalue.UUID(projectID),
			EnvironmentID:             pgvalue.UUID(environmentID),
			RunID:                     pgvalue.UUID(runID),
			SessionID:                 pgvalue.UUID(sessionID),
			WorkspaceID:               pgvalue.UUID(workspaceID),
			CurrentRunLeaseID:         pgvalue.UUID(leaseID),
			WorkspaceCurrentVersionID: pgvalue.UUID(workspaceVersionID),
			WorkerInstanceID:          pgvalue.UUID(workerID),
			WorkerCniProfile:          "helmr/v0",
			DirtyGeneration:           0,
		},
		token: db.Token{
			ID:            pgvalue.UUID(tokenID),
			OrgID:         pgvalue.UUID(orgID),
			ProjectID:     pgvalue.UUID(projectID),
			EnvironmentID: pgvalue.UUID(environmentID),
			State:         db.TokenStatePending,
			TimeoutAt:     pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		},
		stream: db.Stream{
			ID:            pgvalue.UUID(streamID),
			OrgID:         pgvalue.UUID(orgID),
			ProjectID:     pgvalue.UUID(projectID),
			EnvironmentID: pgvalue.UUID(environmentID),
			SessionID:     pgvalue.UUID(sessionID),
			Name:          "approval",
			Direction:     db.StreamDirectionInput,
		},
	}
}

func (s *runWaitControlStore) BeginQuerier(context.Context) (db.Querier, controlTransaction, error) {
	s.beginCalls++
	txStore := s.txStore
	if txStore == nil {
		txStore = s
	}
	return txStore, runWaitControlTx{store: txStore}, nil
}

func (s *runWaitControlStore) GetWorkerRunWaitScope(context.Context, db.GetWorkerRunWaitScopeParams) (db.GetWorkerRunWaitScopeRow, error) {
	if s.scopeErr != nil {
		return db.GetWorkerRunWaitScopeRow{}, s.scopeErr
	}
	return s.scope, nil
}

func (s *runWaitControlStore) GetToken(context.Context, db.GetTokenParams) (db.Token, error) {
	if s.tokenErr != nil {
		return db.Token{}, s.tokenErr
	}
	return s.token, nil
}

func (s *runWaitControlStore) GetSessionStreamByName(context.Context, db.GetSessionStreamByNameParams) (db.Stream, error) {
	if s.streamErr != nil {
		return db.Stream{}, s.streamErr
	}
	return s.stream, nil
}

func (s *runWaitControlStore) ListStreamRecords(context.Context, db.ListStreamRecordsParams) ([]db.StreamRecord, error) {
	return s.records, nil
}

func (s *runWaitControlStore) CreateHotRunWait(_ context.Context, arg db.CreateHotRunWaitParams) (db.CreateHotRunWaitRow, error) {
	s.createHotRunWaitCalls++
	s.createHotRunWaitParams = arg
	return db.CreateHotRunWaitRow{
		ID:            arg.ID,
		OrgID:         arg.OrgID,
		ProjectID:     arg.ProjectID,
		EnvironmentID: arg.EnvironmentID,
		RunID:         arg.RunID,
		Kind:          db.RunWaitKind(arg.Kind),
		State:         db.RunWaitStateLiveWaiting,
	}, nil
}

func (s *runWaitControlStore) SetRunWaitWorkspaceVersion(_ context.Context, arg db.SetRunWaitWorkspaceVersionParams) (db.RunWait, error) {
	s.setRunWaitWorkspaceVersionCalls++
	return db.RunWait{
		ID:                 arg.ID,
		OrgID:              arg.OrgID,
		ProjectID:          arg.ProjectID,
		EnvironmentID:      arg.EnvironmentID,
		RunID:              arg.RunID,
		Kind:               db.RunWaitKindStream,
		State:              db.RunWaitStateCheckpointing,
		WorkspaceVersionID: arg.WorkspaceVersionID,
	}, nil
}

func (s *runWaitControlStore) CreateTokenWait(context.Context, db.CreateTokenWaitParams) (db.TokenWait, error) {
	s.createTokenWaitCalls++
	if s.createTokenWaitErr != nil {
		return db.TokenWait{}, s.createTokenWaitErr
	}
	return db.TokenWait{ID: pgvalue.UUID(uuid.Must(uuid.NewV7()))}, nil
}

func (s *runWaitControlStore) CreateStreamWait(context.Context, db.CreateStreamWaitParams) (db.StreamWait, error) {
	s.createStreamWaitCalls++
	return db.StreamWait{ID: pgvalue.UUID(uuid.Must(uuid.NewV7()))}, nil
}

func (s *runWaitControlStore) CreateTimerWait(_ context.Context, arg db.CreateTimerWaitParams) (db.TimerWait, error) {
	s.createTimerWaitCalls++
	s.createTimerWaitParams = arg
	return db.TimerWait{ID: pgvalue.UUID(uuid.Must(uuid.NewV7()))}, nil
}

func (s *runWaitControlStore) ResolveImmediateTokenWait(context.Context, db.ResolveImmediateTokenWaitParams) (db.ResolveImmediateTokenWaitRow, error) {
	return db.ResolveImmediateTokenWaitRow{}, pgx.ErrNoRows
}

func (s *runWaitControlStore) FailRuntimeCheckpointAttempt(_ context.Context, arg db.FailRuntimeCheckpointAttemptParams) (db.FailRuntimeCheckpointAttemptRow, error) {
	s.failRuntimeCheckpointAttemptCalls++
	s.failRuntimeCheckpointAttemptParams = arg
	if s.failRuntimeCheckpointAttemptErr != nil {
		return db.FailRuntimeCheckpointAttemptRow{}, s.failRuntimeCheckpointAttemptErr
	}
	return db.FailRuntimeCheckpointAttemptRow{
		ID:            arg.RunWaitID,
		OrgID:         arg.OrgID,
		ProjectID:     arg.ProjectID,
		EnvironmentID: arg.EnvironmentID,
		RunID:         arg.RunID,
		State:         db.RunWaitStateLiveWaiting,
	}, nil
}

func (s *runWaitControlStore) AcknowledgeWorkerCommandForRunWait(_ context.Context, arg db.AcknowledgeWorkerCommandForRunWaitParams) (db.WorkerCommand, error) {
	s.ackWorkerCommandForRunWaitCalls++
	s.ackWorkerCommandForRunWaitParams = arg
	if s.ackWorkerCommandForRunWaitErr != nil {
		return db.WorkerCommand{}, s.ackWorkerCommandForRunWaitErr
	}
	return db.WorkerCommand{
		ID:               arg.ID,
		OrgID:            arg.OrgID,
		RunID:            arg.RunID,
		RunWaitID:        arg.RunWaitID,
		RunLeaseID:       arg.RunLeaseID,
		WorkerInstanceID: arg.WorkerInstanceID,
		Kind:             arg.Kind,
		AcknowledgedAt:   pgvalue.Timestamptz(time.Now()),
	}, nil
}

func (s *runWaitControlStore) GetAcknowledgedReadyRuntimeCheckpointForRunWait(_ context.Context, arg db.GetAcknowledgedReadyRuntimeCheckpointForRunWaitParams) (pgtype.UUID, error) {
	if !s.acknowledgedReadyCheckpoint {
		return pgtype.UUID{}, pgx.ErrNoRows
	}
	return arg.RuntimeCheckpointID, nil
}

func (s *runWaitControlStore) UpsertCasObject(_ context.Context, arg db.UpsertCasObjectParams) (db.CasObject, error) {
	return db.CasObject{
		Digest:    arg.Digest,
		SizeBytes: arg.SizeBytes,
		MediaType: arg.MediaType,
	}, nil
}

func (s *runWaitControlStore) CreateArtifact(_ context.Context, arg db.CreateArtifactParams) (db.Artifact, error) {
	s.createArtifactParams = append(s.createArtifactParams, arg)
	return db.Artifact{
		ID:                        arg.ID,
		OrgID:                     arg.OrgID,
		ProjectID:                 arg.ProjectID,
		EnvironmentID:             arg.EnvironmentID,
		Digest:                    arg.Digest,
		Kind:                      arg.Kind,
		SizeBytes:                 arg.SizeBytes,
		MediaType:                 arg.MediaType,
		CreatedByWorkerInstanceID: arg.CreatedByWorkerInstanceID,
	}, nil
}

func (s *runWaitControlStore) CreateReadyRuntimeCheckpointForRunWait(_ context.Context, arg db.CreateReadyRuntimeCheckpointForRunWaitParams) (db.CreateReadyRuntimeCheckpointForRunWaitRow, error) {
	s.createReadyCheckpointCalls++
	s.createReadyCheckpointParams = arg
	return db.CreateReadyRuntimeCheckpointForRunWaitRow{
		ID:            arg.RuntimeCheckpointID,
		OrgID:         arg.OrgID,
		ProjectID:     arg.ProjectID,
		EnvironmentID: arg.EnvironmentID,
		RunID:         arg.RunID,
		CniProfile:    arg.CniProfile,
	}, nil
}

func (s *runWaitControlStore) CreateRuntimeCheckpointArtifact(_ context.Context, arg db.CreateRuntimeCheckpointArtifactParams) (db.RuntimeCheckpointArtifact, error) {
	s.createRuntimeCheckpointArtifactParams = append(s.createRuntimeCheckpointArtifactParams, arg)
	return db.RuntimeCheckpointArtifact{}, nil
}

func (s *runWaitControlStore) GetRunWait(_ context.Context, arg db.GetRunWaitParams) (db.RunWait, error) {
	return db.RunWait{
		ID:            arg.ID,
		OrgID:         arg.OrgID,
		ProjectID:     arg.ProjectID,
		EnvironmentID: arg.EnvironmentID,
		RunID:         s.scope.RunID,
		Kind:          "",
		State:         db.RunWaitStateCheckpointedWaiting,
	}, nil
}

func (s *runWaitControlStore) GetRunWaitByRun(_ context.Context, arg db.GetRunWaitByRunParams) (db.RunWait, error) {
	if s.runWaitByRunErr != nil {
		return db.RunWait{}, s.runWaitByRunErr
	}
	if s.runWaitByRun.ID.Valid {
		return s.runWaitByRun, nil
	}
	return db.RunWait{
		ID:                    arg.ID,
		OrgID:                 arg.OrgID,
		ProjectID:             s.scope.ProjectID,
		EnvironmentID:         s.scope.EnvironmentID,
		RunID:                 arg.RunID,
		Kind:                  "",
		State:                 db.RunWaitStateCheckpointedWaiting,
		OwnerRunLeaseID:       s.scope.CurrentRunLeaseID,
		OwnerWorkerInstanceID: s.scope.WorkerInstanceID,
	}, nil
}

type runWaitControlTx struct {
	store *runWaitControlStore
}

func (tx runWaitControlTx) Commit(context.Context) error {
	tx.store.commitCalls++
	return nil
}

func (tx runWaitControlTx) Rollback(context.Context) error {
	tx.store.rollbackCalls++
	return nil
}

func tokenWaitRequest(t *testing.T, tokenID uuid.UUID) api.WorkerCreateRunWaitRequest {
	t.Helper()
	params, err := json.Marshal(map[string]string{"token_id": tokenID.String()})
	if err != nil {
		t.Fatal(err)
	}
	return api.WorkerCreateRunWaitRequest{Kind: api.WorkerRunWaitKindToken, Params: params}
}

func streamWaitRequest(t *testing.T, stream string) api.WorkerCreateRunWaitRequest {
	t.Helper()
	params, err := json.Marshal(map[string]string{"stream": stream})
	if err != nil {
		t.Fatal(err)
	}
	return api.WorkerCreateRunWaitRequest{Kind: api.WorkerRunWaitKindStream, Params: params}
}

func timerWaitRequest(timeoutSeconds int32) api.WorkerCreateRunWaitRequest {
	return api.WorkerCreateRunWaitRequest{Kind: api.WorkerRunWaitKindTimer, TimeoutSeconds: &timeoutSeconds}
}

func intervalDuration(interval pgtype.Interval) time.Duration {
	return time.Duration(interval.Microseconds) * time.Microsecond
}

func createArtifactKindSeen(params []db.CreateArtifactParams, kind db.ArtifactKind) bool {
	for _, param := range params {
		if param.Kind == kind {
			return true
		}
	}
	return false
}

func checkpointManifest() api.WorkerCheckpointManifest {
	artifact := api.WorkerCheckpointArtifact{
		Digest:    "sha256:checkpoint",
		SizeBytes: 1,
		MediaType: "application/octet-stream",
	}
	return api.WorkerCheckpointManifest{
		RecoveryPoint: api.WorkerCheckpointRecoveryPoint{
			Runtime: api.WorkerCheckpointRuntime{
				Backend:         "firecracker",
				ID:              "sha256:runtime",
				Arch:            "amd64",
				ABI:             "helmr.firecracker.snapshot.v0",
				KernelDigest:    "sha256:kernel",
				InitramfsDigest: "sha256:initramfs",
				RootfsDigest:    "sha256:rootfs",
				ConfigDigest:    "sha256:config",
			},
		},
		RuntimeState: api.WorkerCheckpointRuntimeState{
			ConfigArtifact:      artifact,
			VMStateArtifact:     artifact,
			ScratchDiskArtifact: artifact,
		},
	}
}
