package controlplane

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/secret"
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
		len(response.ProgramStart) == 0 ||
		response.Computer.WriteCapability == "" ||
		len(response.Secrets) != 1 ||
		response.Secrets[0].Env == nil ||
		response.Secrets[0].Env.Name != "TOKEN" {
		t.Fatalf("response = %#v, Secret opens = %d", response, opener.calls)
	}

	authority.runtime.WriterTokenHash = make([]byte, 32)
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
		t.Fatal("mismatched Computer capability was accepted")
	}
	if opener.calls != 0 {
		t.Fatalf("Secrets opened before Computer capability verification: %d", opener.calls)
	}
}

func TestRunLeaseClaimResponseKeepsComputerAuthorityInAssignment(t *testing.T) {
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
		Lease    map[string]json.RawMessage `json:"lease"`
		Computer map[string]json.RawMessage `json:"computer"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"computer_id",
		"base_computer_disk_version_id",
		"writer_generation",
	} {
		if _, ok := decoded.Lease[field]; !ok {
			t.Fatalf("lease does not contain %q: %s", field, raw)
		}
	}
	if len(decoded.Computer) != 2 || decoded.Computer["write_capability"] == nil ||
		decoded.Computer["target"] == nil {
		t.Fatalf("computer attachment = %s", raw)
	}
}

func TestRunLeaseClaimProjectionLoadsOnlyLockedAttemptBase(t *testing.T) {
	authority, projection, _ := validRunLeaseClaimResponse(t)
	store := &runLeaseProjectionStore{
		program: projection.program, definition: projection.definition,
		resetTarget: projection.resetTarget,
	}

	if _, err := loadRunLeaseClaimProjection(context.Background(), store, authority); err != nil {
		t.Fatal(err)
	}
	if store.resetTargetParams.OrgID != authority.run.OrgID ||
		store.resetTargetParams.ProjectID != authority.run.ProjectID ||
		store.resetTargetParams.EnvironmentID != authority.run.EnvironmentID ||
		store.resetTargetParams.ComputerID != authority.computer.ID ||
		store.resetTargetParams.VersionID != authority.attempt.BaseComputerDiskVersionID {
		t.Fatalf("reset target lookup = %+v", store.resetTargetParams)
	}
}

func validRunLeaseClaimResponse(
	t *testing.T,
) (runLeaseClaimResponseAuthority, runLeaseClaimProjection, computer.FencingKey) {
	t.Helper()
	physical := validRunLeaseProjectionAuthority()
	run, attempt, definition := validTaskProgramStart(t, definition.SchemaKindNone)
	run.ID = physical.run.ID
	run.ComputerID = physical.computer.ID
	run.BaseComputerDiskVersionID = physical.run.BaseComputerDiskVersionID
	run.MaxActiveDurationMs = physical.run.MaxActiveDurationMs
	run.ActiveElapsedMs = physical.run.ActiveElapsedMs
	attempt.RunID = run.ID
	attempt.ComputerID = run.ComputerID
	attempt.BaseComputerDiskVersionID = run.BaseComputerDiskVersionID
	physical.runLease.RunID = run.ID
	physical.runLease.ComputerID = run.ComputerID
	physical.runLease.AttemptNumber = attempt.Number
	physical.runtime.EnvironmentID = run.EnvironmentID

	key, err := computer.NewFencingKey(make([]byte, computer.FencingKeySize))
	if err != nil {
		t.Fatal(err)
	}
	capability, err := deriveComputerCapabilityInput(key, physical.runtime)
	if err != nil {
		t.Fatal(err)
	}
	physical.runtime.WriterTokenHash, err = hex.DecodeString(strings.TrimPrefix(capability.Hash, "sha256:"))
	if err != nil {
		t.Fatal(err)
	}

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
		resetTarget: validComputerMountTargetAuthority(physical),
	}
	return runLeaseClaimResponseAuthority{
		run:      run,
		attempt:  attempt,
		runtime:  physical.runtime,
		runLease: physical.runLease,
		computer: physical.computer,
	}, projection, key
}

func deriveComputerCapabilityInput(
	key computer.FencingKey,
	instance db.ComputerInstance,
) (computer.FencingCapability, error) {
	return key.Derive(computer.FenceInput{
		InstanceID:       uuid.UUID(instance.ID.Bytes),
		ComputerID:       uuid.UUID(instance.ComputerID.Bytes),
		WriterGeneration: instance.WriterGeneration,
	})
}

func claimResponseRuntimeDescriptor() deployment.RuntimeDescriptor {
	return deployment.RuntimeDescriptor{
		Architecture:    definition.ArchitectureX8664,
		Digest:          "sha256:" + strings.Repeat("9", 64),
		FormatVersion:   deployment.RuntimeDescriptorFormatVersion,
		MediaType:       deployment.RuntimeArtifactMediaType,
		RuntimeContract: definition.RuntimeContract,
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

// These reads project the authority already admitted by ClaimExecution.
type runLeaseProjectionStore struct {
	db.Querier
	program           db.GetDeploymentProgramAuthorityRow
	definition        db.DeploymentDefinition
	resetTarget       db.GetComputerDiskVersionAuthorityRow
	resetTargetParams db.GetComputerDiskVersionAuthorityParams
}

func (s *runLeaseProjectionStore) GetDeploymentProgramAuthority(context.Context, db.GetDeploymentProgramAuthorityParams) (db.GetDeploymentProgramAuthorityRow, error) {
	return s.program, nil
}
func (s *runLeaseProjectionStore) GetDeploymentDefinition(context.Context, db.GetDeploymentDefinitionParams) (db.DeploymentDefinition, error) {
	return s.definition, nil
}
func (s *runLeaseProjectionStore) GetComputerDiskVersionAuthority(_ context.Context, p db.GetComputerDiskVersionAuthorityParams) (db.GetComputerDiskVersionAuthorityRow, error) {
	s.resetTargetParams = p
	return s.resetTarget, nil
}
