package computerhost

import (
	"context"
	"errors"
	"strings"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (p *PreparedMachines) sessionRuntimeDescriptor(object workerapi.CASObject) artifact.RuntimeDescriptor {
	return artifact.RuntimeDescriptor{Architecture: p.RuntimeArchitecture, Digest: object.Digest, FormatVersion: artifact.RuntimeDescriptorFormatVersion, MediaType: object.MediaType, RuntimeContract: definition.RuntimeContract, SizeBytes: object.SizeBytes}
}

func sessionProgramDescriptor(object workerapi.CASObject) artifact.ProgramDescriptor {
	return artifact.ProgramDescriptor{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}
}

// Metadata receipts authorize only exact immutable content. The guest owns its
// live sealed artifacts and decides whether it needs bytes. A requested transfer
// makes the attachment job retry with descriptor-bound snapshots prepared
// outside the VM lifecycle barrier. No remote read runs under that barrier.
func (p *PreparedMachines) prepareSessionProgramArtifacts(ctx context.Context, directory, instanceID string, program *workerapi.RuntimeProgram) (*programSnapshots, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(program.DeploymentID) == "" {
		return nil, errors.New("program deployment id is required")
	}
	if p.PlatformStore == nil {
		return nil, errors.New("managed runtime delivery is not configured")
	}
	runtimeDescriptor := p.sessionRuntimeDescriptor(program.Runtime)
	programDescriptor := sessionProgramDescriptor(program.Artifact)
	if err := artifact.ValidateRuntimeDescriptor(runtimeDescriptor); err != nil {
		return nil, err
	}
	if err := cas.ValidateDescriptor(cas.Descriptor(programDescriptor)); err != nil {
		return nil, err
	}
	if programDescriptor.MediaType != artifact.ProgramArtifactMediaType || programDescriptor.SizeBytes > artifact.MaxProgramPhysicalBytes || runtimeDescriptor.SizeBytes > artifact.MaxRuntimePhysicalBytes {
		return nil, errors.New("invalid Session Program descriptor")
	}
	p.mu.Lock()
	runtimeIndex, runtimeHit := p.verifiedRuntimes[runtimeDescriptor]
	var metadata *artifact.ProgramMetadata
	if p.programDescriptor == programDescriptor && p.programMetadata != nil {
		owned := p.programMetadata.Clone()
		metadata = &owned
	}
	p.mu.Unlock()
	if !runtimeHit || metadata == nil {
		return p.prepareProgramArtifacts(ctx, directory, instanceID, program)
	}
	if runtimeIndex != (artifact.RuntimeIndex{Architecture: runtimeDescriptor.Architecture, RuntimeContract: runtimeDescriptor.RuntimeContract}) {
		return nil, errors.New("managed runtime index does not match its descriptor")
	}
	if metadata.RuntimeDigest != runtimeDescriptor.Digest || metadata.RuntimeContract != runtimeDescriptor.RuntimeContract || metadata.Architecture != runtimeDescriptor.Architecture {
		return nil, errors.New("program metadata does not match runtime reservation authority")
	}
	if err := verifyProgramMetadataDigest(*metadata, program.IndexDigest); err != nil {
		return nil, err
	}
	return &programSnapshots{}, nil
}
