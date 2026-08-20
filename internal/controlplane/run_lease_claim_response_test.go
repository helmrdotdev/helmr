package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestProjectRunLeaseClaimResponseOpensSecretsAfterVerifyingCapability(t *testing.T) {
	authority, projection, keys := validRunLeaseClaimResponse(t)
	opener := &recordingSecretDeliveryOpener{
		materials: []secret.DeliveryMaterial{{
			PlacementKind: "env", PlacementTarget: "TOKEN", Value: []byte("secret"),
		}},
	}
	response, err := projectRunLeaseClaimResponse(
		context.Background(),
		authority,
		[]secret.DeliveryEnvelope{{PlacementKind: "env", PlacementTarget: "TOKEN"}},
		projection,
		claimResponsePlatformStore{},
		opener,
		keys,
	)
	if err != nil {
		t.Fatal(err)
	}
	if opener.calls != 1 ||
		response.Execution.Fresh == nil ||
		response.Workspace.WriteCapability == "" ||
		len(response.Secrets) != 1 ||
		response.Secrets[0].Env == nil ||
		response.Secrets[0].Env.Name != "TOKEN" {
		t.Fatalf("response = %#v, Secret opens = %d", response, opener.calls)
	}

	authority.workspaceLease.FencingTokenHash = "sha256:" + strings.Repeat("0", 64)
	opener.calls = 0
	if _, err := projectRunLeaseClaimResponse(
		context.Background(),
		authority,
		nil,
		projection,
		claimResponsePlatformStore{},
		opener,
		keys,
	); err == nil {
		t.Fatal("mismatched Workspace capability was accepted")
	}
	if opener.calls != 0 {
		t.Fatalf("Secrets opened before Workspace capability verification: %d", opener.calls)
	}
}

func TestRunLeaseClaimResponseKeepsWorkspaceAuthorityInAssignment(t *testing.T) {
	authority, projection, keys := validRunLeaseClaimResponse(t)
	response, err := projectRunLeaseClaimResponse(
		context.Background(),
		authority,
		nil,
		projection,
		claimResponsePlatformStore{},
		&recordingSecretDeliveryOpener{},
		keys,
	)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Lease     map[string]json.RawMessage `json:"lease"`
		Workspace map[string]json.RawMessage `json:"workspace"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"workspace_id",
		"workspace_mount_id",
		"workspace_lease_id",
		"base_workspace_version_id",
		"ownership_generation",
		"writer_generation",
		"mount_fencing_generation",
	} {
		if _, ok := decoded.Lease[field]; !ok {
			t.Fatalf("lease does not contain %q: %s", field, raw)
		}
	}
	if len(decoded.Workspace) != 2 || decoded.Workspace["write_capability"] == nil ||
		decoded.Workspace["reset_target"] == nil {
		t.Fatalf("workspace attachment = %s", raw)
	}
}

func TestRunLeaseClaimProjectionLoadsOnlyLockedWorkspaceLeaseBase(t *testing.T) {
	authority, projection, _ := validRunLeaseClaimResponse(t)
	store := &runLeaseClaimStore{
		program: projection.program, definition: projection.definition,
		resetTarget: projection.resetTarget,
	}

	if _, err := loadRunLeaseClaimProjection(context.Background(), store, authority); err != nil {
		t.Fatal(err)
	}
	if store.resetTargetParams.OrgID != authority.run.OrgID ||
		store.resetTargetParams.ProjectID != authority.run.ProjectID ||
		store.resetTargetParams.EnvironmentID != authority.run.EnvironmentID ||
		store.resetTargetParams.WorkspaceID != authority.workspace.ID ||
		store.resetTargetParams.VersionID != authority.workspaceLease.BaseVersionID {
		t.Fatalf("reset target lookup = %+v", store.resetTargetParams)
	}
}

func TestRestoreRunLeaseClaimDoesNotOpenSecrets(t *testing.T) {
	authority, projection, keys := validRunLeaseClaimResponse(t)
	authority.mode = runLeaseClaimRestore
	authority.attempt.EntrypointEnteredAt.Valid = true
	authority.runWait = db.RunWait{
		ID: pgvalue.UUID(uuid.New()), ConditionState: db.WaitStateCompleted,
		ConditionTerminalAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		ResumeAttachID:      pgvalue.UUID(uuid.New()), ResumeRequestVersion: 2,
	}
	authority.checkpoint = db.RunCheckpoint{
		ID: pgvalue.UUID(uuid.New()), RunID: authority.run.ID,
		AttemptNumber: authority.attempt.Number,
		State:         db.RunCheckpointStateReady,
	}
	authority.checkpoint.RestoreManifest = testCheckpointManifest(
		t,
		authority.checkpoint.ID,
		authority.run.ID,
		authority.attempt.Number,
		authority.runWait.ID,
	)
	authority.runtime.RestoreCheckpointID = authority.checkpoint.ID
	projection.checkpointArtifacts = []db.ListRunCheckpointArtifactAuthorityRow{
		{Role: db.RunCheckpointArtifactRoleRuntimeConfig, Ordinal: 0, Digest: validDigest('a'), SizeBytes: 8, MediaType: "application/example"},
		{Role: db.RunCheckpointArtifactRoleVMState, Ordinal: 0, Digest: validDigest('b'), SizeBytes: 4, MediaType: "application/example"},
		{Role: db.RunCheckpointArtifactRoleMemory, Ordinal: 0, Digest: validDigest('c'), SizeBytes: 16, MediaType: "application/example"},
		{Role: db.RunCheckpointArtifactRoleScratchDisk, Ordinal: 0, Digest: validDigest('d'), SizeBytes: 12, MediaType: "application/example"},
	}
	response, err := projectRunLeaseClaimResponse(
		context.Background(),
		authority,
		[]secret.DeliveryEnvelope{{PlacementKind: "env", PlacementTarget: "TOKEN"}},
		projection,
		claimResponsePlatformStore{},
		nil,
		keys,
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.Secrets == nil || len(response.Secrets) != 0 || response.Execution.Restore == nil {
		t.Fatalf("response = %#v", response)
	}
}

func validRunLeaseClaimResponse(
	t *testing.T,
) (runLeaseClaimResponseAuthority, runLeaseClaimProjection, workspace.FencingKey) {
	t.Helper()
	physical := validRunLeaseProjectionAuthority()
	run, attempt, definition := validTaskProgramStart(t, deployment.SchemaKindNone)
	run.ID = physical.run.ID
	run.WorkspaceID = physical.workspace.ID
	run.BaseWorkspaceVersionID = physical.workspaceMount.MaterializedVersionID
	run.MaxActiveDurationMs = physical.run.MaxActiveDurationMs
	run.ActiveElapsedMs = physical.run.ActiveElapsedMs
	attempt.RunID = run.ID
	attempt.WorkspaceID = run.WorkspaceID
	attempt.BaseWorkspaceVersionID = run.BaseWorkspaceVersionID
	physical.runLease.RunID = run.ID
	physical.runLease.WorkspaceID = run.WorkspaceID
	physical.runLease.AttemptNumber = attempt.Number
	physical.workspaceLease.WorkspaceID = run.WorkspaceID

	key, err := workspace.NewFencingKey(make([]byte, workspace.FencingKeySize))
	if err != nil {
		t.Fatal(err)
	}
	capability, err := deriveWorkspaceCapabilityInput(key, physical.workspaceLease)
	if err != nil {
		t.Fatal(err)
	}
	physical.workspaceLease.FencingTokenHash = capability.Hash

	runtime := claimResponseRuntimeDescriptor()
	projection := runLeaseClaimProjection{
		program: db.GetDeploymentProgramAuthorityRow{
			DeploymentID:             run.DeploymentID,
			EnvironmentID:            run.EnvironmentID,
			DeploymentVersion:        "v42",
			RuntimeArtifactDigest:    runtime.Digest,
			ProgramArtifactDigest:    validDigest('a'),
			ProgramArtifactSizeBytes: 100,
			ProgramArtifactMediaType: deployment.ProgramArtifactMediaType,
			ProgramIndexDigest:       validDigestBytes(t, 'b'),
		},
		definition:  definition,
		resetTarget: validWorkspaceResetTargetAuthority(physical),
	}
	return runLeaseClaimResponseAuthority{
		mode:           runLeaseClaimFresh,
		run:            run,
		attempt:        attempt,
		runtime:        physical.runtime,
		runLease:       physical.runLease,
		workspace:      physical.workspace,
		workspaceMount: physical.workspaceMount,
		workspaceLease: physical.workspaceLease,
	}, projection, key
}

func deriveWorkspaceCapabilityInput(
	key workspace.FencingKey,
	lease db.WorkspaceLease,
) (workspace.FencingCapability, error) {
	return key.Derive(workspace.FenceInput{
		LeaseID:                uuid.UUID(lease.ID.Bytes),
		WorkspaceID:            uuid.UUID(lease.WorkspaceID.Bytes),
		OwnershipGeneration:    lease.OwnershipGeneration,
		WriterGeneration:       lease.WriterGeneration,
		MountFencingGeneration: lease.MountFencingGeneration,
	})
}

func claimResponseRuntimeDescriptor() deployment.RuntimeDescriptor {
	return deployment.RuntimeDescriptor{
		Architecture:    deployment.ArchitectureX8664,
		Digest:          "sha256:" + strings.Repeat("9", 64),
		FormatVersion:   deployment.RuntimeDescriptorFormatVersion,
		MediaType:       deployment.RuntimeArtifactMediaType,
		RuntimeContract: deployment.RuntimeContract,
		SizeBytes:       4096,
	}
}

type claimResponsePlatformStore struct{}

func (claimResponsePlatformStore) Stat(
	_ context.Context,
	digest string,
) (cas.Object, error) {
	runtime := claimResponseRuntimeDescriptor()
	if digest != runtime.Digest {
		return cas.Object{}, errors.New("object not found")
	}
	return cas.Object{
		Digest: digest, SizeBytes: runtime.SizeBytes, MediaType: runtime.MediaType,
	}, nil
}

func (claimResponsePlatformStore) Get(
	context.Context,
	string,
) (io.ReadCloser, error) {
	return nil, errors.New("unexpected object read")
}

type recordingSecretDeliveryOpener struct {
	materials []secret.DeliveryMaterial
	calls     int
}

func (opener *recordingSecretDeliveryOpener) OpenDeliveries(
	uuid.UUID,
	[]secret.DeliveryEnvelope,
) ([]secret.DeliveryMaterial, error) {
	opener.calls++
	return opener.materials, nil
}

var _ SecretDeliveryOpener = (*recordingSecretDeliveryOpener)(nil)
