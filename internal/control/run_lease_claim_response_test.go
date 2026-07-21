package control

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func TestProjectRunLeaseClaimResponseOpensSecretsAfterVerifyingCapability(t *testing.T) {
	authority, projection, keys := validRunLeaseClaimResponse(t)
	opener := &recordingSecretDeliveryOpener{
		materials: []secret.DeliveryMaterial{{
			PlacementKind: "env", PlacementTarget: "TOKEN", Value: []byte("secret"),
		}},
	}
	response, err := projectRunLeaseClaimResponse(
		authority,
		[]secret.DeliveryEnvelope{{PlacementKind: "env", PlacementTarget: "TOKEN"}},
		projection,
		claimResponseBuildPolicy(),
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
		authority,
		nil,
		projection,
		claimResponseBuildPolicy(),
		opener,
		keys,
	); err == nil {
		t.Fatal("mismatched Workspace capability was accepted")
	}
	if opener.calls != 0 {
		t.Fatalf("Secrets opened before Workspace capability verification: %d", opener.calls)
	}
}

func TestRunLeaseClaimResponseKeepsWorkspaceAuthorityInReceipt(t *testing.T) {
	authority, projection, keys := validRunLeaseClaimResponse(t)
	response, err := projectRunLeaseClaimResponse(
		authority,
		nil,
		projection,
		claimResponseBuildPolicy(),
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
	if len(decoded.Workspace) != 1 || decoded.Workspace["write_capability"] == nil {
		t.Fatalf("workspace attachment = %s", raw)
	}
}

func validRunLeaseClaimResponse(
	t *testing.T,
) (runLeaseClaimResponseAuthority, runLeaseClaimProjection, workspace.FencingKeys) {
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

	keys, err := workspace.NewFencingKeys(
		"sha256:c57461e4ce9af0ed10b8b704cdc10537834475e528e4591d295857177987ee03",
		map[string][]byte{
			"sha256:c57461e4ce9af0ed10b8b704cdc10537834475e528e4591d295857177987ee03": make(
				[]byte,
				workspace.FencingKeySize,
			),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	physical.workspaceLease.FencingKeyFingerprint = keys.ActiveFingerprint().Bytes()
	capability, err := deriveWorkspaceCapabilityInput(keys, physical.workspaceLease)
	if err != nil {
		t.Fatal(err)
	}
	physical.workspaceLease.FencingTokenHash = capability.Hash

	runtime := claimResponseRuntimeDescriptor()
	runtimeDigest, err := deployment.RuntimeDigestBytes(runtime.Digest)
	if err != nil {
		t.Fatal(err)
	}
	projection := runLeaseClaimProjection{
		program: db.GetDeploymentProgramAuthorityRow{
			DeploymentID:               run.DeploymentID,
			EnvironmentID:              run.EnvironmentID,
			DeploymentVersion:          "v42",
			ProgramRuntimeDigest:       runtimeDigest,
			ProgramArchitecture:        pgvalue.Text(string(deployment.ArchitectureX8664)),
			ProgramCodeDigest:          validDigest('a'),
			ProgramCodeSizeBytes:       100,
			ProgramCodeMediaType:       "application/vnd.helmr.program-code.v0+erofs",
			ProgramDependencyDigest:    validDigest('b'),
			ProgramDependencySizeBytes: 200,
			ProgramDependencyMediaType: "application/vnd.helmr.program-dependencies.v0+erofs",
			BuildContractVersion:       deployment.ProgramBuildContractVersion,
		},
		definition: definition,
	}
	return runLeaseClaimResponseAuthority{
		mode:           runLeaseClaimFresh,
		run:            run,
		attempt:        attempt,
		runtime:        physical.runtime,
		networkSlot:    physical.networkSlot,
		runLease:       physical.runLease,
		workspace:      physical.workspace,
		workspaceMount: physical.workspaceMount,
		workspaceLease: physical.workspaceLease,
	}, projection, keys
}

func deriveWorkspaceCapabilityInput(
	keys workspace.FencingKeys,
	lease db.WorkspaceLease,
) (workspace.FencingCapability, error) {
	fingerprint, err := workspace.FencingKeyFingerprintFromBytes(
		lease.FencingKeyFingerprint,
	)
	if err != nil {
		return workspace.FencingCapability{}, err
	}
	return keys.Derive(fingerprint, workspace.FenceInput{
		LeaseID:                uuid.UUID(lease.ID.Bytes),
		WorkspaceID:            uuid.UUID(lease.WorkspaceID.Bytes),
		OwnershipGeneration:    lease.OwnershipGeneration,
		WriterGeneration:       lease.WriterGeneration,
		MountFencingGeneration: lease.MountFencingGeneration,
	})
}

func claimResponseRuntimeDescriptor() deployment.RuntimeDescriptor {
	return deployment.RuntimeDescriptor{
		Architecture:      deployment.ArchitectureX8664,
		Digest:            "sha256:" + strings.Repeat("9", 64),
		FormatVersion:     deployment.RuntimeDescriptorFormatVersion,
		MediaType:         deployment.RuntimeArtifactMediaType,
		RuntimeAPIVersion: deployment.RuntimeAPIVersion,
		SizeBytes:         4096,
	}
}

func claimResponseBuildPolicy() *deployment.BuildPolicy {
	runtimeDescriptor, err := deployment.CanonicalRuntimeDescriptor(
		claimResponseRuntimeDescriptor(),
	)
	if err != nil {
		panic(err)
	}
	toolchain := deployment.Toolchain{
		Architecture:         deployment.ArchitectureX8664,
		FormatVersion:        deployment.ToolchainFormatVersion,
		ManagedRuntimeDigest: claimResponseRuntimeDescriptor().Digest,
		ToolchainClosure: deployment.ManagerArtifact{
			Digest:    "sha256:" + strings.Repeat("7", 64),
			MediaType: deployment.ToolchainMediaType,
			SizeBytes: 1,
		},
	}
	toolchainDescriptor, err := deployment.CanonicalToolchain(toolchain)
	if err != nil {
		panic(err)
	}
	toolchainDigest, err := deployment.StandardToolchainDigest(toolchain)
	if err != nil {
		panic(err)
	}
	raw := []byte(
		`{"current":{"us-east-1":{"buildContractVersion":"helmr.program-build.v0","runtimeDigest":"` +
			claimResponseRuntimeDescriptor().Digest +
			`","standardToolchainDigest":"` + toolchainDigest +
			`"}},"formatVersion":0,"runtimes":[` + string(runtimeDescriptor) +
			`],"toolchains":[` + string(toolchainDescriptor) + `]}`,
	)
	policy, err := deployment.ParseBuildPolicy(raw)
	if err != nil {
		panic(err)
	}
	return policy
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
