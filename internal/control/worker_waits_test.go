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
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
)

func TestCreateWorkerRunWaitRejectsMissingTokenBeforeParking(t *testing.T) {
	store := newRunWaitControlStore()
	store.tokenErr = pgx.ErrNoRows
	server := &Server{db: store}

	_, err := server.createWorkerRunWait(context.Background(), store.scope, tokenWaitRequest(t, uuid.Must(uuid.NewV7())))

	if !errors.Is(err, errTokenNotFound) {
		t.Fatalf("err = %v, want token_not_found", err)
	}
	if store.beginCalls != 0 || store.createRunWaitCalls != 0 {
		t.Fatalf("parking side effects before token validation: begin=%d create_run_wait=%d", store.beginCalls, store.createRunWaitCalls)
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
	if txStore.createRunWaitCalls != 1 || txStore.createTokenWaitCalls != 1 {
		t.Fatalf("tx side effects = create_run_wait %d create_token_wait %d", txStore.createRunWaitCalls, txStore.createTokenWaitCalls)
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
	if response.CheckpointID == "" || response.RunWaitID == "" {
		t.Fatalf("response = %+v, want run wait and checkpoint handles", response)
	}
	if !response.CaptureWorkspace {
		t.Fatalf("capture workspace = false, want true for dirty workspace")
	}
	if store.beginCalls != 1 || txStore.createRunWaitCalls != 1 || txStore.createStreamWaitCalls != 1 || txStore.commitCalls != 1 {
		t.Fatalf("parking side effects = begin %d create_run_wait %d create_stream_wait %d commit %d", store.beginCalls, txStore.createRunWaitCalls, txStore.createStreamWaitCalls, txStore.commitCalls)
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
	if response.CaptureWorkspace {
		t.Fatalf("capture workspace = true, want false for clean workspace")
	}
	if txStore.setRunWaitWorkspaceVersionCalls != 1 {
		t.Fatalf("set workspace version calls = %d, want 1", txStore.setRunWaitWorkspaceVersionCalls)
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
		RunWaitID:    runWaitID.String(),
		CheckpointID: checkpointID.String(),
		Manifest:     checkpointManifest(),
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
	store.failParkingRunWaitErr = pgx.ErrNoRows
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
		RunWaitID:    runWaitID.String(),
		CheckpointID: checkpointID.String(),
		Error:        "checkpoint failed",
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
	if store.failParkingRunWaitCalls != 1 {
		t.Fatalf("fail parking calls = %d, want 1", store.failParkingRunWaitCalls)
	}
	if pgvalue.MustUUIDValue(store.failParkingRunWaitParams.RunID) != runID {
		t.Fatalf("fail parking run id = %s, want %s", pgvalue.MustUUIDValue(store.failParkingRunWaitParams.RunID), runID)
	}
	if pgvalue.MustUUIDValue(store.failParkingRunWaitParams.RunWaitID) != runWaitID {
		t.Fatalf("fail parking run wait id = %s, want %s", pgvalue.MustUUIDValue(store.failParkingRunWaitParams.RunWaitID), runWaitID)
	}
	if pgvalue.MustUUIDValue(store.failParkingRunWaitParams.RuntimeCheckpointID) != checkpointID {
		t.Fatalf("fail parking checkpoint id = %s, want %s", pgvalue.MustUUIDValue(store.failParkingRunWaitParams.RuntimeCheckpointID), checkpointID)
	}
}

type runWaitControlStore struct {
	fakeStore

	scope  db.GetWorkerRunWaitScopeRow
	token  db.Token
	stream db.Stream

	scopeErr           error
	tokenErr           error
	streamErr          error
	records            []db.StreamRecord
	createTokenWaitErr error

	txStore                         *runWaitControlStore
	beginCalls                      int
	createRunWaitCalls              int
	createTokenWaitCalls            int
	createStreamWaitCalls           int
	createTimerWaitCalls            int
	setRunWaitWorkspaceVersionCalls int
	failParkingRunWaitCalls         int
	failParkingRunWaitParams        db.FailParkingRunWaitParams
	failParkingRunWaitErr           error
	commitCalls                     int
	rollbackCalls                   int
	createReadyCheckpointCalls      int
	createReadyCheckpointParams     db.CreateReadyRuntimeCheckpointForRunWaitParams
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

func (s *runWaitControlStore) CreateRunWait(_ context.Context, arg db.CreateRunWaitParams) (db.RunWait, error) {
	s.createRunWaitCalls++
	return db.RunWait{
		ID:            arg.ID,
		OrgID:         arg.OrgID,
		ProjectID:     arg.ProjectID,
		EnvironmentID: arg.EnvironmentID,
		RunID:         arg.RunID,
		Kind:          db.RunWaitKind(arg.Kind),
		State:         db.RunWaitStateParking,
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
		State:              db.RunWaitStateParking,
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

func (s *runWaitControlStore) CreateTimerWait(context.Context, db.CreateTimerWaitParams) (db.TimerWait, error) {
	s.createTimerWaitCalls++
	return db.TimerWait{ID: pgvalue.UUID(uuid.Must(uuid.NewV7()))}, nil
}

func (s *runWaitControlStore) ResolveImmediateTokenWait(context.Context, db.ResolveImmediateTokenWaitParams) (db.ResolveImmediateTokenWaitRow, error) {
	return db.ResolveImmediateTokenWaitRow{}, pgx.ErrNoRows
}

func (s *runWaitControlStore) FailParkingRunWait(_ context.Context, arg db.FailParkingRunWaitParams) (db.RunWait, error) {
	s.failParkingRunWaitCalls++
	s.failParkingRunWaitParams = arg
	if s.failParkingRunWaitErr != nil {
		return db.RunWait{}, s.failParkingRunWaitErr
	}
	return db.RunWait{
		ID:            arg.RunWaitID,
		OrgID:         arg.OrgID,
		ProjectID:     arg.ProjectID,
		EnvironmentID: arg.EnvironmentID,
		RunID:         arg.RunID,
		State:         db.RunWaitStateFailed,
	}, nil
}

func (s *runWaitControlStore) UpsertCasObject(_ context.Context, arg db.UpsertCasObjectParams) (db.CasObject, error) {
	return db.CasObject{
		Digest:    arg.Digest,
		SizeBytes: arg.SizeBytes,
		MediaType: arg.MediaType,
	}, nil
}

func (s *runWaitControlStore) CreateArtifact(_ context.Context, arg db.CreateArtifactParams) (db.Artifact, error) {
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

func (s *runWaitControlStore) CreateRuntimeCheckpointArtifact(context.Context, db.CreateRuntimeCheckpointArtifactParams) (db.RuntimeCheckpointArtifact, error) {
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
		State:         db.RunWaitStateWaiting,
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
