package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/imagebuild"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkerCompleteDeploymentBuildReadsCASBeforeTransaction(t *testing.T) {
	server, store, request, _ := newDeploymentBuildCompletionFixture(t)
	response := httptest.NewRecorder()

	server.workerCompleteDeploymentBuild(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if !store.committed || store.completed != 1 {
		t.Fatalf("committed = %t completed = %d", store.committed, store.completed)
	}
}

func TestWorkerCompleteDeploymentBuildRejectsAuthorityDrift(t *testing.T) {
	server, store, request, _ := newDeploymentBuildCompletionFixture(t)
	store.locked.BuildManagerVersion = "1.3.12"
	response := httptest.NewRecorder()

	server.workerCompleteDeploymentBuild(response, request)

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if store.committed || store.completed != 0 {
		t.Fatalf("committed = %t completed = %d", store.committed, store.completed)
	}
}

func TestWorkerCompleteDeploymentBuildReplaysConcurrentCompletion(
	t *testing.T,
) {
	server, store, request, fingerprint := newDeploymentBuildCompletionFixture(t)
	buildCAS := server.cas.(*deploymentBuildCompletionCAS)
	buildCAS.getHook = func() {
		store.locked.State = db.DeploymentBuildLeaseStateSucceeded
		store.locked.TerminalRequestFingerprint = pgtype.Text{
			String: fingerprint,
			Valid:  true,
		}
	}
	buildCAS.getErr = errors.New("CAS unavailable")
	response := httptest.NewRecorder()

	server.workerCompleteDeploymentBuild(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if !store.committed || store.completed != 0 {
		t.Fatalf("committed = %t completed = %d", store.committed, store.completed)
	}
}

func TestValidateCompletedWorkspaceImageOperationsRejectsAuthorityMismatch(t *testing.T) {
	tests := []struct {
		name   string
		change func(*deploymentBuildCompletionStore, *deployment.BuildResult)
		mode   imagebuild.CacheMode
	}{
		{
			name: "claim slot",
			change: func(store *deploymentBuildCompletionStore, _ *deployment.BuildResult) {
				store.imageClaim.SlotHash[0] ^= 0xff
			},
			mode: imagebuild.CachePrefer,
		},
		{
			name: "physical attempt",
			change: func(_ *deploymentBuildCompletionStore, result *deployment.BuildResult) {
				result.Succeeded.WorkspaceImages[0].Operation.AttemptID = uuid.Must(uuid.NewV7()).String()
			},
			mode: imagebuild.CachePrefer,
		},
		{
			name:   "Deployment cache mode",
			change: func(*deploymentBuildCompletionStore, *deployment.BuildResult) {},
			mode:   imagebuild.CacheBypass,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, store, request, _ := newDeploymentBuildCompletionFixture(t)
			var completion struct {
				Result json.RawMessage `json:"result"`
			}
			if err := json.NewDecoder(request.Body).Decode(&completion); err != nil {
				t.Fatal(err)
			}
			result, err := deployment.ParseBuildResult(completion.Result)
			if err != nil {
				t.Fatal(err)
			}
			test.change(store, &result)
			err = validateCompletedWorkspaceImageOperations(
				t.Context(),
				store,
				pgvalue.MustUUIDValue(store.locked.EnvironmentID),
				pgvalue.MustUUIDValue(store.locked.CurrentBuildLeaseID),
				1,
				test.mode,
				result.Succeeded.WorkspaceImages,
			)
			if err == nil {
				t.Fatal("mismatched Workspace image operation was accepted")
			}
		})
	}
}

func newDeploymentBuildCompletionFixture(
	t *testing.T,
) (*Server, *deploymentBuildCompletionStore, *http.Request, string) {
	t.Helper()
	policy := controlPlaneBuildPolicy(t)
	runtimeDigestString := "sha256:" + strings.Repeat("9", sha256.Size*2)
	toolchainDigestString := "sha256:" + strings.Repeat("7", sha256.Size*2)
	runtimeDigest, err := deployment.RuntimeDigestBytes(runtimeDigestString)
	if err != nil {
		t.Fatal(err)
	}
	toolchainDigest, err := deployment.SHA256DigestBytes(toolchainDigestString)
	if err != nil {
		t.Fatal(err)
	}
	managerDigestString := "sha256:" + strings.Repeat("8", sha256.Size*2)
	managerDigest, err := deployment.SHA256DigestBytes(managerDigestString)
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	lockfile := []byte(
		`{"configVersion":1,"lockfileVersion":1,"packages":{},"workspaces":{"":{"name":"test"}}}`,
	)
	for name, body := range map[string][]byte{
		"helmr.config.ts": []byte(`export default { dirs: ["tasks"] }`),
		"package.json": []byte(
			`{"devEngines":{"runtime":{"name":"node","version":"24.16.0"}},"packageManager":"bun@1.3.11","type":"module"}`,
		),
		"bun.lock": lockfile,
	} {
		if err := os.WriteFile(filepath.Join(root, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	source, cleanup, err := archive.CreateTarWithOptions(
		root,
		t.TempDir(),
		archive.TarOptions{CanonicalSource: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	sourceBody, err := os.ReadFile(source.Path)
	if err != nil {
		t.Fatal(err)
	}
	lockfileHash := sha256.Sum256(lockfile)
	lockfileDigest := "sha256:" + hex.EncodeToString(lockfileHash[:])

	orgID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	projectID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	environmentID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	deploymentID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	buildLeaseID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	workerInstanceID := uuid.Must(uuid.NewV7())
	operationID := uuid.Must(uuid.NewV7())
	attemptID := uuid.Must(uuid.NewV7())
	imagePlan := imagebuild.Build{
		FormatVersion: imagebuild.FormatVersion,
		Root:          "repo",
		Images: []imagebuild.Spec{{
			Key: "repo",
			Platform: imagebuild.Platform{
				OS:           "linux",
				Architecture: string(deployment.ArchitectureX8664),
			},
			Steps: []imagebuild.Step{{
				From: &imagebuild.From{Ref: "alpine:3.23"},
			}},
		}},
	}
	planDigest, err := imagebuild.Digest(imagePlan, string(deployment.ArchitectureX8664))
	if err != nil {
		t.Fatal(err)
	}
	requestFingerprint := controlPlaneDigest("Workspace image request")
	resolutionSetDigest := imagebuild.ResolutionSetDigest([]imagebuild.RegistryBinding{})
	imageDigest := "sha256:" + strings.Repeat("d", sha256.Size*2)
	result := deployment.BuildResult{
		FormatVersion: deployment.BuildResultFormatVersion,
		Outcome:       deployment.BuildOutcomeSucceeded,
		Succeeded: &deployment.BuildSucceeded{
			Plan: deployment.BuildPlan{
				FormatVersion: deployment.BuildPlanFormatVersion,
				Definitions: []deployment.DefinitionInput{{
					Kind:       deployment.DefinitionKindSandbox,
					DeclaredID: "repo",
					Sandbox: &deployment.SandboxInputManifest{
						ImageBuild: imagePlan,
						Resources: deployment.ResourcesManifest{
							MilliCPU:  1,
							MemoryMiB: 1,
						},
					},
				}},
				Queues: []deployment.QueueInput{},
			},
			Provenance: deployment.BuildProvenance{
				Architecture:         deployment.ArchitectureX8664,
				BuildContractVersion: deployment.ProgramBuildContractVersion,
				Config: deployment.ProgramConfig{
					EvaluatorAPIVersion: deployment.ConfigEvaluatorAPIVersion,
					SourceDigest:        controlPlaneDigest("config source"),
					ResultDigest:        controlPlaneDigest("config result"),
				},
				Manager: deployment.ProgramManager{
					Digest:  managerDigestString,
					Name:    deployment.PackageManagerBun,
					Version: "1.3.11",
				},
				RuntimeDigest:   runtimeDigestString,
				ToolchainDigest: toolchainDigestString,
				Submitted: deployment.ProgramSubmittedSource{
					LockfileDigest: lockfileDigest,
					LockfileName:   "bun.lock",
					SourceDigest:   source.Digest,
				},
			},
			WorkspaceImages: []deployment.WorkspaceImage{{
				DeclaredID: "repo",
				Operation: deployment.WorkspaceImageOperationEvidence{
					BuildLeaseID:         pgvalue.UUIDString(buildLeaseID),
					BuildLeaseGeneration: 1,
					DeclarationSlot:      "repo",
					OperationID:          operationID.String(),
					RequestFingerprint:   requestFingerprint,
					AttemptID:            attemptID.String(),
					PlanDigest:           planDigest,
					ResolutionSetDigest:  resolutionSetDigest,
					RequestedCacheMode:   imagebuild.CachePrefer,
				},
				Artifact: deployment.WorkspaceImageArtifact{
					Digest:       imageDigest,
					SizeBytes:    1,
					MediaType:    deployment.WorkspaceImageArtifactMediaType,
					Architecture: deployment.ArchitectureX8664,
				},
			}},
		},
	}
	rawResult, err := deployment.CanonicalBuildResult(result)
	if err != nil {
		t.Fatal(err)
	}
	requestFingerprintBytes, err := deployment.SHA256DigestBytes(requestFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	slotHash, err := idempotency.WorkspaceImageBuildSlotHash(
		pgvalue.MustUUIDValue(environmentID),
		pgvalue.MustUUIDValue(buildLeaseID),
		1,
		"repo",
	)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := json.Marshal(workspaceImageOperationReceipt{
		BuildLeaseID:         pgvalue.UUIDString(buildLeaseID),
		BuildLeaseGeneration: 1,
		DeclarationSlot:      "repo",
		OperationID:          operationID.String(),
		AttemptID:            attemptID.String(),
		RequestFingerprint:   requestFingerprint,
		PlanDigest:           planDigest,
		ResolutionSetDigest:  resolutionSetDigest,
		RequestedCacheMode:   imagebuild.CachePrefer,
		Result: imagebuild.GuestResult{
			ExecutionABI: imagebuild.ExecutionABI,
			Outcome:      imagebuild.GuestSucceeded,
			OCIDigest:    imageDigest,
			OCISizeBytes: 1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	imageClaim := db.IdempotencyClaim{
		ID:                 pgvalue.UUID(operationID),
		EnvironmentID:      environmentID,
		Operation:          "workspace.image.build",
		SlotHash:           slotHash[:],
		RequestFingerprint: requestFingerprintBytes,
		State:              "completed",
		Receipt:            receipt,
	}

	authority := db.GetDeploymentBuildCompletionAuthorityRow{
		State:                      db.DeploymentBuildLeaseStateRunning,
		ExpiresAt:                  pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		DeploymentStatus:           db.DeploymentStatusBuilding,
		CurrentBuildLeaseID:        buildLeaseID,
		BuildNodeVersion:           "24.16.0",
		BuildRuntimeDigest:         runtimeDigest,
		BuildToolchainDigest:       toolchainDigest,
		BuildManagerName:           string(deployment.PackageManagerBun),
		BuildManagerVersion:        "1.3.11",
		BuildManagerDigest:         managerDigest,
		BuildContractVersion:       deployment.ProgramBuildContractVersion,
		ImageCacheMode:             string(imagebuild.CachePrefer),
		DeploymentSourceArtifactID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		DeploymentSourceDigest:     source.Digest,
		DeploymentSourceSizeBytes:  source.SizeBytes,
		DeploymentSourceMediaType:  archive.SourceMediaType,
	}
	store := &deploymentBuildCompletionStore{
		authority: authority,
		locked: db.LockDeploymentBuildTerminalFenceRow{
			OrgID:                      orgID,
			ProjectID:                  projectID,
			EnvironmentID:              environmentID,
			DeploymentID:               deploymentID,
			State:                      authority.State,
			ExpiresAt:                  authority.ExpiresAt,
			DeploymentStatus:           authority.DeploymentStatus,
			CurrentBuildLeaseID:        authority.CurrentBuildLeaseID,
			BuildNodeVersion:           authority.BuildNodeVersion,
			BuildRuntimeDigest:         authority.BuildRuntimeDigest,
			BuildToolchainDigest:       authority.BuildToolchainDigest,
			BuildManagerName:           authority.BuildManagerName,
			BuildManagerVersion:        authority.BuildManagerVersion,
			BuildManagerDigest:         authority.BuildManagerDigest,
			BuildContractVersion:       authority.BuildContractVersion,
			ImageCacheMode:             authority.ImageCacheMode,
			DeploymentSourceArtifactID: authority.DeploymentSourceArtifactID,
			DeploymentSourceDigest:     authority.DeploymentSourceDigest,
			DeploymentSourceSizeBytes:  authority.DeploymentSourceSizeBytes,
			DeploymentSourceMediaType:  authority.DeploymentSourceMediaType,
		},
		deploymentID: deploymentID,
		imageClaim:   imageClaim,
	}
	server := &Server{
		db: store,
		cas: &deploymentBuildCompletionCAS{
			store:  store,
			source: sourceBody,
			object: cas.Object{
				Digest:    imageDigest,
				SizeBytes: 1,
				MediaType: deployment.WorkspaceImageArtifactMediaType,
			},
		},
		buildPolicy: policy,
	}
	worker := workerActor{
		WorkerInstanceID: workerInstanceID,
		WorkerGroupID:    "build",
		WorkerEpoch:      1,
		ProtocolVersion:  "test",
	}
	body, err := json.Marshal(struct {
		Lease  workerapi.DeploymentBuildLease `json:"lease"`
		Result json.RawMessage                `json:"result"`
	}{
		Lease: workerapi.DeploymentBuildLease{
			ID:                    pgvalue.UUIDString(buildLeaseID),
			OrgID:                 pgvalue.UUIDString(orgID),
			ProjectID:             pgvalue.UUIDString(projectID),
			EnvironmentID:         pgvalue.UUIDString(environmentID),
			DeploymentID:          pgvalue.UUIDString(deploymentID),
			WorkerGroupID:         worker.WorkerGroupID,
			WorkerInstanceID:      worker.WorkerInstanceID.String(),
			WorkerEpoch:           worker.WorkerEpoch,
			LeaseSequence:         1,
			WorkerProtocolVersion: worker.ProtocolVersion,
		},
		Result: rawResult,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		"POST",
		"/api/worker/v0/build/deployments/complete",
		bytes.NewReader(body),
	)
	request = request.WithContext(
		context.WithValue(request.Context(), workerContextKey{}, worker),
	)
	return server, store, request, deploymentBuildResultFingerprint(rawResult)
}

type deploymentBuildCompletionStore struct {
	db.Querier
	authority     db.GetDeploymentBuildCompletionAuthorityRow
	locked        db.LockDeploymentBuildTerminalFenceRow
	deploymentID  pgtype.UUID
	imageClaim    db.IdempotencyClaim
	inTransaction bool
	committed     bool
	completed     int
}

func (store *deploymentBuildCompletionStore) LockCompletedWorkspaceImageOperation(
	_ context.Context,
	params db.LockCompletedWorkspaceImageOperationParams,
) (db.IdempotencyClaim, error) {
	if params.EnvironmentID != store.imageClaim.EnvironmentID ||
		params.ImageOperationID != store.imageClaim.ID {
		return db.IdempotencyClaim{}, pgx.ErrNoRows
	}
	return store.imageClaim, nil
}

func (store *deploymentBuildCompletionStore) GetDeploymentBuildTerminalResult(
	context.Context,
	db.GetDeploymentBuildTerminalResultParams,
) (db.GetDeploymentBuildTerminalResultRow, error) {
	return db.GetDeploymentBuildTerminalResultRow{}, pgx.ErrNoRows
}

func (store *deploymentBuildCompletionStore) GetDeploymentBuildCompletionAuthority(
	context.Context,
	db.GetDeploymentBuildCompletionAuthorityParams,
) (db.GetDeploymentBuildCompletionAuthorityRow, error) {
	if store.inTransaction {
		return db.GetDeploymentBuildCompletionAuthorityRow{}, errors.New(
			"completion authority read inside transaction",
		)
	}
	return store.authority, nil
}

func (store *deploymentBuildCompletionStore) BeginQuerier(
	context.Context,
) (db.Querier, transaction, error) {
	store.inTransaction = true
	return store, deploymentBuildCompletionTransaction{store: store}, nil
}

func (store *deploymentBuildCompletionStore) LockDeploymentBuildWorkerAuthority(
	context.Context,
	db.LockDeploymentBuildWorkerAuthorityParams,
) (db.LockDeploymentBuildWorkerAuthorityRow, error) {
	return db.LockDeploymentBuildWorkerAuthorityRow{
		RuntimeArch: pgtype.Text{
			String: string(deployment.ArchitectureX8664),
			Valid:  true,
		},
	}, nil
}

func (store *deploymentBuildCompletionStore) LockDeploymentBuildTerminalFence(
	context.Context,
	db.LockDeploymentBuildTerminalFenceParams,
) (db.LockDeploymentBuildTerminalFenceRow, error) {
	return store.locked, nil
}

func (store *deploymentBuildCompletionStore) UpsertCasObject(
	context.Context,
	db.UpsertCasObjectParams,
) (db.CasObject, error) {
	return db.CasObject{}, nil
}

func (store *deploymentBuildCompletionStore) CreateArtifact(
	context.Context,
	db.CreateArtifactParams,
) (db.Artifact, error) {
	return db.Artifact{ID: pgvalue.UUID(uuid.Must(uuid.NewV7()))}, nil
}

func (store *deploymentBuildCompletionStore) CreateDeploymentDefinition(
	context.Context,
	db.CreateDeploymentDefinitionParams,
) (db.DeploymentDefinition, error) {
	return db.DeploymentDefinition{}, nil
}

func (store *deploymentBuildCompletionStore) CompleteDeploymentBuild(
	context.Context,
	db.CompleteDeploymentBuildParams,
) (db.CompleteDeploymentBuildRow, error) {
	store.completed++
	return db.CompleteDeploymentBuildRow{
		ID:            store.deploymentID,
		OrgID:         store.locked.OrgID,
		ProjectID:     store.locked.ProjectID,
		EnvironmentID: store.locked.EnvironmentID,
		Status:        db.DeploymentStatusDeployed,
	}, nil
}

func (store *deploymentBuildCompletionStore) AppendDeploymentEvent(
	context.Context,
	db.AppendDeploymentEventParams,
) (db.AppendDeploymentEventRow, error) {
	return db.AppendDeploymentEventRow{}, nil
}

type deploymentBuildCompletionTransaction struct {
	store *deploymentBuildCompletionStore
}

func (tx deploymentBuildCompletionTransaction) Commit(context.Context) error {
	tx.store.inTransaction = false
	tx.store.committed = true
	return nil
}

func (tx deploymentBuildCompletionTransaction) Rollback(context.Context) error {
	tx.store.inTransaction = false
	return nil
}

type deploymentBuildCompletionCAS struct {
	store   *deploymentBuildCompletionStore
	source  []byte
	object  cas.Object
	getHook func()
	getErr  error
}

func (store *deploymentBuildCompletionCAS) Stat(
	_ context.Context,
	digest string,
) (cas.Object, error) {
	if store.store.inTransaction {
		return cas.Object{}, errors.New("CAS stat started inside transaction")
	}
	if digest != store.object.Digest {
		return cas.Object{}, errors.New("unexpected CAS stat")
	}
	return store.object, nil
}

func (store *deploymentBuildCompletionCAS) Get(
	_ context.Context,
	digest string,
) (io.ReadCloser, error) {
	if store.store.inTransaction {
		return nil, errors.New("CAS read started inside transaction")
	}
	if digest != store.store.authority.DeploymentSourceDigest {
		return nil, errors.New("unexpected CAS get")
	}
	if store.getHook != nil {
		store.getHook()
	}
	if store.getErr != nil {
		return nil, store.getErr
	}
	return io.NopCloser(bytes.NewReader(store.source)), nil
}

func (*deploymentBuildCompletionCAS) Put(
	context.Context,
	string,
	io.Reader,
) (cas.Object, error) {
	return cas.Object{}, errors.New("unexpected CAS put")
}

func (*deploymentBuildCompletionCAS) Stage(
	context.Context,
	string,
) (cas.Stage, error) {
	return nil, errors.New("unexpected CAS stage")
}

func (*deploymentBuildCompletionCAS) Delete(context.Context, string) error {
	return errors.New("unexpected CAS delete")
}
