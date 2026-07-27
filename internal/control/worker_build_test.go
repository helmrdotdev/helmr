package control

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
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/imagebuild"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
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

func newDeploymentBuildCompletionFixture(
	t *testing.T,
) (*Server, *deploymentBuildCompletionStore, *http.Request, string) {
	t.Helper()
	policy := claimResponseBuildPolicy()
	target, err := policy.Current("us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	runtimeDigest, err := deployment.RuntimeDigestBytes(target.Runtime.Digest)
	if err != nil {
		t.Fatal(err)
	}
	toolchainDigest, err := deployment.SHA256DigestBytes(target.StandardToolchainDigest)
	if err != nil {
		t.Fatal(err)
	}
	managerDigestString := "sha256:" + strings.Repeat("8", sha256.Size*2)
	managerDigest, err := deployment.SHA256DigestBytes(managerDigestString)
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	lockfile := []byte("lockfileVersion = 1")
	for name, body := range map[string][]byte{
		"package.json": []byte(`{"packageManager":"bun@1.3.11"}`),
		"bun.lock":     lockfile,
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

	imageDigest := "sha256:" + strings.Repeat("d", sha256.Size*2)
	result := deployment.BuildResult{
		FormatVersion: deployment.BuildResultFormatVersion,
		Outcome:       deployment.BuildOutcomeSucceeded,
		Succeeded: &deployment.BuildSucceeded{
			Plan: deployment.BuildPlan{
				FormatVersion: deployment.BuildPlanFormatVersion,
				Definitions: []deployment.DefinitionInput{{
					Kind:       deployment.DefinitionKindWorkspace,
					DeclaredID: "repo",
					Workspace: &deployment.WorkspaceInputManifest{
						ImageBuild: imagebuild.Build{
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
						},
						Resources: deployment.ResourcesManifest{
							MilliCPU:         1,
							MemoryMiB:        1,
							EphemeralDiskMiB: 1,
						},
						Network: deployment.NetworkManifest{
							Internet:  false,
							DenyCIDRs: []string{},
						},
						Architecture: deployment.ArchitectureX8664,
					},
				}},
				Queues: []deployment.QueueInput{},
			},
			Provenance: deployment.BuildProvenance{
				Architecture:         deployment.ArchitectureX8664,
				BuildContractVersion: target.BuildContractVersion,
				Manager: deployment.ProgramManager{
					Digest:  managerDigestString,
					Name:    deployment.PackageManagerBun,
					Version: "1.3.11",
				},
				RuntimeDigest:           target.Runtime.Digest,
				StandardToolchainDigest: target.StandardToolchainDigest,
				Submitted: deployment.ProgramSubmittedSource{
					LockfileDigest: lockfileDigest,
					LockfileName:   "bun.lock",
					SourceDigest:   source.Digest,
				},
			},
			WorkspaceImages: []deployment.WorkspaceImage{{
				DeclaredID: "repo",
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

	orgID := pgvalue.UUID(uuid.New())
	projectID := pgvalue.UUID(uuid.New())
	environmentID := pgvalue.UUID(uuid.New())
	deploymentID := pgvalue.UUID(uuid.New())
	buildLeaseID := pgvalue.UUID(uuid.New())
	workerInstanceID := uuid.New()
	authority := db.GetDeploymentBuildCompletionAuthorityRow{
		State:                        db.DeploymentBuildLeaseStateRunning,
		ExpiresAt:                    pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		DeploymentStatus:             db.DeploymentStatusBuilding,
		CurrentBuildLeaseID:          buildLeaseID,
		BuildArchitecture:            string(deployment.ArchitectureX8664),
		BuildRuntimeDigest:           runtimeDigest,
		BuildStandardToolchainDigest: toolchainDigest,
		BuildManagerName:             string(deployment.PackageManagerBun),
		BuildManagerVersion:          "1.3.11",
		BuildManagerDigest:           managerDigest,
		BuildContractVersion:         target.BuildContractVersion,
		DeploymentSourceArtifactID:   pgvalue.UUID(uuid.New()),
		DeploymentSourceDigest:       source.Digest,
		DeploymentSourceSizeBytes:    source.SizeBytes,
		DeploymentSourceMediaType:    archive.SourceMediaType,
	}
	store := &deploymentBuildCompletionStore{
		authority: authority,
		locked: db.LockDeploymentBuildTerminalFenceRow{
			OrgID:                        orgID,
			ProjectID:                    projectID,
			EnvironmentID:                environmentID,
			DeploymentID:                 deploymentID,
			State:                        authority.State,
			ExpiresAt:                    authority.ExpiresAt,
			DeploymentStatus:             authority.DeploymentStatus,
			CurrentBuildLeaseID:          authority.CurrentBuildLeaseID,
			BuildArchitecture:            authority.BuildArchitecture,
			BuildRuntimeDigest:           authority.BuildRuntimeDigest,
			BuildStandardToolchainDigest: authority.BuildStandardToolchainDigest,
			BuildManagerName:             authority.BuildManagerName,
			BuildManagerVersion:          authority.BuildManagerVersion,
			BuildManagerDigest:           authority.BuildManagerDigest,
			BuildContractVersion:         authority.BuildContractVersion,
			DeploymentSourceArtifactID:   authority.DeploymentSourceArtifactID,
			DeploymentSourceDigest:       authority.DeploymentSourceDigest,
			DeploymentSourceSizeBytes:    authority.DeploymentSourceSizeBytes,
			DeploymentSourceMediaType:    authority.DeploymentSourceMediaType,
		},
		deploymentID: deploymentID,
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
		Lease  api.WorkerDeploymentBuildLease `json:"lease"`
		Result json.RawMessage                `json:"result"`
	}{
		Lease: api.WorkerDeploymentBuildLease{
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
		"/api/worker/deployments/complete",
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
	inTransaction bool
	committed     bool
	completed     int
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
) (db.Querier, controlTransaction, error) {
	store.inTransaction = true
	return store, deploymentBuildCompletionTransaction{store: store}, nil
}

func (store *deploymentBuildCompletionStore) LockDeploymentBuildWorkerCertification(
	context.Context,
	db.LockDeploymentBuildWorkerCertificationParams,
) (db.LockDeploymentBuildWorkerCertificationRow, error) {
	return db.LockDeploymentBuildWorkerCertificationRow{
		RuntimeArch: pgtype.Text{
			String: store.authority.BuildArchitecture,
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
	return db.Artifact{ID: pgvalue.UUID(uuid.New())}, nil
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
