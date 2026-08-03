package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/imagebuild"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type ProgramBuildInputRequest struct {
	WorkDir       string
	SourceStore   cas.Store
	PlatformStore cas.Reader
	Build         workerapi.DeploymentBuild
	Encoder       string
}

type ProgramBuildInputFailure struct {
	Reason BuildFailureReason
	err    error
}

func (failure *ProgramBuildInputFailure) Error() string {
	if failure == nil || failure.err == nil {
		return "program build input is invalid"
	}
	return failure.err.Error()
}

func (failure *ProgramBuildInputFailure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.err
}

type ProgramBuildInputs struct {
	source              *submittedSource
	selection           SourceSelection
	manager             BuildManager
	runtime             BuildRuntime
	toolchain           BuildToolchain
	managerSnapshot     *ArtifactSnapshot
	runtimeSnapshot     *ArtifactSnapshot
	toolchainSnapshot   *ArtifactSnapshot
	toolchainDescriptor ToolchainArtifactDescriptor
}

func OpenProgramBuildInputs(
	ctx context.Context,
	request ProgramBuildInputRequest,
) (_ *ProgramBuildInputs, returnErr error) {
	if ctx == nil {
		return nil, errors.New("program build input context is nil")
	}
	if request.WorkDir == "" || !filepath.IsAbs(request.WorkDir) ||
		filepath.Clean(request.WorkDir) != request.WorkDir {
		return nil, errors.New("program build input work directory must be an absolute clean path")
	}
	if request.SourceStore == nil {
		return nil, errors.New("program build source store is required")
	}
	if request.PlatformStore == nil {
		return nil, errors.New("program build platform artifact store is required")
	}
	if err := validateProgramEncoder(request.Encoder); err != nil {
		return nil, err
	}

	work := request.Build
	if work.BuildContractVersion != ProgramBuildContractVersion {
		return nil, &ProgramBuildInputFailure{
			Reason: BuildFailureUnsupportedToolchain,
			err:    errors.New("deployment build contract is unsupported"),
		}
	}
	if work.ImageCacheMode != string(imagebuild.CachePrefer) &&
		work.ImageCacheMode != string(imagebuild.CacheBypass) {
		return nil, &ProgramBuildInputFailure{
			Reason: BuildFailureInvalidPlan,
			err:    fmt.Errorf("deployment image cache mode %q is unsupported", work.ImageCacheMode),
		}
	}
	managerPin := PackageManager{
		Integrity: work.Manager.Integrity,
		Name:      PackageManagerName(work.Manager.Name),
		Version:   work.Manager.Version,
	}
	if err := ValidatePackageManager(managerPin); err != nil {
		return nil, &ProgramBuildInputFailure{
			Reason: BuildFailureUnsupportedToolchain,
			err:    err,
		}
	}
	runtime := BuildRuntime{
		Artifact:    platformArtifactDescriptor(work.Runtime),
		NodeVersion: work.NodeVersion,
	}
	managerKind, managerPath, _, err := managerDistribution(managerPin)
	if err != nil {
		return nil, &ProgramBuildInputFailure{
			Reason: BuildFailureUnsupportedToolchain,
			err:    err,
		}
	}
	manager := BuildManager{
		Artifact:       platformArtifactDescriptor(work.Manager.Artifact),
		Entrypoint:     ManagerEntrypoint{Kind: managerKind, Path: managerPath},
		PackageManager: managerPin,
	}
	toolchain := BuildToolchain{
		Artifact:      platformArtifactDescriptor(work.Toolchain),
		RuntimeDigest: work.Runtime.Digest,
	}
	if err := validateBuildRuntime(runtime); err != nil {
		return nil, &ProgramBuildInputFailure{
			Reason: BuildFailureUnsupportedToolchain,
			err:    err,
		}
	}
	if err := validateBuildManager(manager); err != nil {
		return nil, &ProgramBuildInputFailure{
			Reason: BuildFailureManagerNotFound,
			err:    err,
		}
	}
	if err := validateBuildToolchain(toolchain); err != nil {
		return nil, &ProgramBuildInputFailure{
			Reason: BuildFailureUnsupportedToolchain,
			err:    err,
		}
	}

	inputs := &ProgramBuildInputs{
		manager:   manager,
		runtime:   runtime,
		toolchain: toolchain,
	}
	defer func() {
		if returnErr != nil {
			returnErr = closeProgramBuildInputsAfterError(inputs, returnErr)
		}
	}()

	inputs.source, inputs.selection, err = snapshotSubmittedSource(
		ctx,
		request.WorkDir,
		request.SourceStore,
		work.DeploymentSource,
	)
	if err != nil {
		return nil, &ProgramBuildInputFailure{
			Reason: BuildFailureInvalidSource,
			err:    err,
		}
	}
	if managerPin != inputs.selection.Manager {
		return nil, &ProgramBuildInputFailure{
			Reason: BuildFailureInvalidSource,
			err:    errors.New("deployment manager pin does not match submitted source"),
		}
	}

	inputs.managerSnapshot, err = snapshotPlatformObject(
		ctx,
		request.WorkDir,
		request.PlatformStore,
		managerArtifact,
		manager.Artifact,
	)
	if err != nil {
		return nil, err
	}
	inputs.runtimeSnapshot, err = snapshotPlatformObject(
		ctx,
		request.WorkDir,
		request.PlatformStore,
		runtimeArtifact,
		runtime.Artifact,
	)
	if err != nil {
		return nil, err
	}
	inputs.toolchainSnapshot, err = snapshotPlatformObject(
		ctx,
		request.WorkDir,
		request.PlatformStore,
		toolchainArtifact,
		toolchain.Artifact,
	)
	if err != nil {
		return nil, err
	}
	inputs.toolchainDescriptor, err = inspectPinnedToolchain(
		ctx,
		inputs.toolchainSnapshot,
		toolchain.Artifact,
	)
	if err != nil {
		return nil, err
	}
	if inputs.toolchainDescriptor.NodeVersion != runtime.NodeVersion ||
		inputs.toolchainDescriptor.RuntimeDigest != runtime.Artifact.Digest {
		return nil, errors.New("toolchain descriptor does not match the deployment runtime pin")
	}
	if _, err := inputs.source.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind submitted source: %w", err)
	}
	return inputs, nil
}

func (inputs *ProgramBuildInputs) Source() io.Reader {
	if inputs == nil || inputs.source == nil {
		return nil
	}
	return inputs.source
}

func (inputs *ProgramBuildInputs) Selection() SourceSelection {
	if inputs == nil {
		return SourceSelection{}
	}
	return inputs.selection
}

func (inputs *ProgramBuildInputs) Manager() BuildManager {
	if inputs == nil {
		return BuildManager{}
	}
	return inputs.manager
}

func (inputs *ProgramBuildInputs) Runtime() BuildRuntime {
	if inputs == nil {
		return BuildRuntime{}
	}
	return inputs.runtime
}

func (inputs *ProgramBuildInputs) Toolchain() BuildToolchain {
	if inputs == nil {
		return BuildToolchain{}
	}
	return inputs.toolchain
}

func (inputs *ProgramBuildInputs) ManagerSnapshot() *ArtifactSnapshot {
	if inputs == nil {
		return nil
	}
	return inputs.managerSnapshot
}

func (inputs *ProgramBuildInputs) RuntimeSnapshot() *ArtifactSnapshot {
	if inputs == nil {
		return nil
	}
	return inputs.runtimeSnapshot
}

func (inputs *ProgramBuildInputs) ToolchainSnapshot() *ArtifactSnapshot {
	if inputs == nil {
		return nil
	}
	return inputs.toolchainSnapshot
}

func (inputs *ProgramBuildInputs) ToolchainDescriptor() ToolchainArtifactDescriptor {
	if inputs == nil {
		return ToolchainArtifactDescriptor{}
	}
	return inputs.toolchainDescriptor
}

func (inputs *ProgramBuildInputs) Close() error {
	if inputs == nil {
		return nil
	}
	err := errors.Join(
		closeProgramBuildInput(inputs.source),
		closeProgramBuildInput(inputs.managerSnapshot),
		closeProgramBuildInput(inputs.runtimeSnapshot),
		closeProgramBuildInput(inputs.toolchainSnapshot),
	)
	inputs.source = nil
	inputs.managerSnapshot = nil
	inputs.runtimeSnapshot = nil
	inputs.toolchainSnapshot = nil
	return err
}

type programBuildInputCloser interface {
	Close() error
}

func closeProgramBuildInput(input programBuildInputCloser) error {
	if input == nil {
		return nil
	}
	return input.Close()
}

func closeProgramBuildInputsAfterError(inputs *ProgramBuildInputs, cause error) error {
	closeErr := inputs.Close()
	if closeErr == nil {
		return cause
	}
	var inputFailure *ProgramBuildInputFailure
	if errors.As(cause, &inputFailure) {
		return fmt.Errorf("close program build inputs after %v: %w", cause, closeErr)
	}
	return errors.Join(cause, closeErr)
}

func platformArtifactDescriptor(object workerapi.CASObject) ArtifactDescriptor {
	return ArtifactDescriptor{
		Digest:    object.Digest,
		SizeBytes: object.SizeBytes,
		MediaType: object.MediaType,
	}
}

func inspectPinnedToolchain(
	ctx context.Context,
	snapshot *ArtifactSnapshot,
	descriptor ArtifactDescriptor,
) (ToolchainArtifactDescriptor, error) {
	file, err := snapshot.verifier()
	if err != nil {
		return ToolchainArtifactDescriptor{}, err
	}
	defer file.Close()
	reader, err := newSquashFSArtifactReader(
		ctx,
		file,
		descriptor.SizeBytes,
		toolchainArtifact,
	)
	if err != nil {
		return ToolchainArtifactDescriptor{}, err
	}
	artifact, err := inspectArtifact(
		ctx,
		reader,
		toolchainArtifact,
		maxToolArtifactBytes,
		descriptor.SizeBytes,
	)
	if err != nil {
		return ToolchainArtifactDescriptor{}, err
	}
	raw, err := artifact.read(
		ctx,
		PlatformDescriptorPath,
		maxPlatformArtifactDocumentBytes,
	)
	if err != nil {
		return ToolchainArtifactDescriptor{}, err
	}
	return ParseToolchainArtifactDescriptor(raw)
}

func snapshotPlatformObject(
	ctx context.Context,
	workDir string,
	store cas.Reader,
	role artifactRole,
	descriptor ArtifactDescriptor,
) (*ArtifactSnapshot, error) {
	spec, err := artifactSnapshotSpecForRole(role)
	if err != nil {
		return nil, err
	}
	if err := validateArtifactSnapshotDescriptor(
		spec,
		artifactSnapshotDescriptor{
			Digest: descriptor.Digest, MediaType: descriptor.MediaType, SizeBytes: descriptor.SizeBytes,
		},
	); err != nil {
		return nil, err
	}
	object, err := store.Stat(ctx, descriptor.Digest)
	if err != nil {
		return nil, fmt.Errorf("stat %s platform artifact: %w", spec.label, err)
	}
	if object.Digest != descriptor.Digest ||
		object.SizeBytes != descriptor.SizeBytes ||
		object.MediaType != descriptor.MediaType {
		return nil, fmt.Errorf("%s platform artifact metadata does not match its pin", spec.label)
	}
	body, err := store.Get(ctx, descriptor.Digest)
	if err != nil {
		return nil, fmt.Errorf("open %s platform artifact: %w", spec.label, err)
	}
	content, snapshotErr := snapshotArtifact(
		ctx,
		workDir,
		role,
		artifactSnapshotDescriptor{
			Digest: descriptor.Digest, MediaType: descriptor.MediaType, SizeBytes: descriptor.SizeBytes,
		},
		body,
	)
	closeErr := body.Close()
	if snapshotErr != nil || closeErr != nil {
		if content != nil {
			_ = content.Close()
		}
		return nil, errors.Join(snapshotErr, closeErr)
	}
	return &ArtifactSnapshot{content: content}, nil
}

type submittedSource struct {
	*os.File
	path string
}

func (source *submittedSource) Close() error {
	if source == nil || source.File == nil {
		return nil
	}
	closeErr := source.File.Close()
	removeErr := os.Remove(source.path)
	source.File = nil
	source.path = ""
	return errors.Join(closeErr, removeErr)
}

func snapshotSubmittedSource(
	ctx context.Context,
	workDir string,
	store cas.Store,
	artifact api.DeploymentSourceArtifact,
) (_ *submittedSource, selection SourceSelection, returnErr error) {
	if artifact.MediaType != api.DeploymentSourceArtifactMediaType ||
		!sha256DigestPattern.MatchString(artifact.Digest) ||
		artifact.SizeBytes < 1 ||
		artifact.SizeBytes > archive.MaxSourceArtifactBytes {
		return nil, SourceSelection{}, errors.New("submitted source descriptor is invalid")
	}
	object, err := store.Stat(ctx, artifact.Digest)
	if err != nil {
		return nil, SourceSelection{}, fmt.Errorf("stat submitted source: %w", err)
	}
	if object.Digest != artifact.Digest ||
		object.SizeBytes != artifact.SizeBytes ||
		object.MediaType != artifact.MediaType {
		return nil, SourceSelection{}, errors.New("submitted source object does not match descriptor")
	}
	body, err := store.Get(ctx, artifact.Digest)
	if err != nil {
		return nil, SourceSelection{}, fmt.Errorf("open submitted source: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, body.Close())
	}()
	file, err := os.CreateTemp(workDir, ".helmr-source-")
	if err != nil {
		return nil, SourceSelection{}, err
	}
	source := &submittedSource{File: file, path: file.Name()}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, source.Close())
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return nil, SourceSelection{}, err
	}
	digest := sha256.New()
	written, err := io.Copy(
		io.MultiWriter(file, digest),
		io.LimitReader(body, artifact.SizeBytes+1),
	)
	if err != nil {
		return nil, SourceSelection{}, err
	}
	if written != artifact.SizeBytes ||
		"sha256:"+hex.EncodeToString(digest.Sum(nil)) != artifact.Digest {
		return nil, SourceSelection{}, errors.New("submitted source bytes do not match descriptor")
	}
	if err := file.Sync(); err != nil {
		return nil, SourceSelection{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, SourceSelection{}, err
	}
	selection, err = InspectSource(file)
	if err != nil {
		return nil, SourceSelection{}, err
	}
	return source, selection, nil
}
