package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/cas"
	buildmodel "github.com/helmrdotdev/helmr/internal/imagebuild"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type Builder struct {
	WorkDir       string
	CAS           cas.Store
	PlatformStore cas.Reader
	Connector     vm.Connector
	Encoder       string
	Images        buildmodel.Engine
}

func (builder Builder) Build(
	ctx context.Context,
	lease api.WorkerDeploymentBuildLease,
	deployment api.WorkerDeploymentBuild,
) (json.RawMessage, error) {
	result, err := builder.build(ctx, lease, deployment)
	if err != nil {
		var guestError *vm.GuestError
		if errors.As(err, &guestError) {
			return nil, buildGuestDeliveryFailure(err)
		}
		var fatal interface{ FatalWorker() bool }
		if errors.As(err, &fatal) && fatal.FatalWorker() {
			return nil, err
		}
		return nil, err
	}
	raw, err := CanonicalBuildResult(result)
	if err != nil {
		return nil, fmt.Errorf("encode canonical build result: %w", err)
	}
	return json.RawMessage(raw), nil
}

func (builder Builder) build(
	ctx context.Context,
	lease api.WorkerDeploymentBuildLease,
	work api.WorkerDeploymentBuild,
) (_ BuildResult, returnErr error) {
	if err := builder.validate(); err != nil {
		return BuildResult{}, err
	}
	if work.BuildContractVersion != ProgramBuildContractVersion {
		return failedBuild(BuildFailureUnsupportedToolchain, errors.New("Deployment build contract is unsupported")), nil
	}
	managerPin := PackageManager{
		Integrity: work.Manager.Integrity,
		Name:      PackageManagerName(work.Manager.Name),
		Version:   work.Manager.Version,
	}
	if err := ValidatePackageManager(managerPin); err != nil {
		return failedBuild(BuildFailureUnsupportedToolchain, err), nil
	}
	runtime := BuildRuntime{
		Artifact:    platformArtifactDescriptor(work.Runtime),
		NodeVersion: work.NodeVersion,
	}
	managerKind, managerPath, _, err := managerDistribution(managerPin)
	if err != nil {
		return failedBuild(BuildFailureUnsupportedToolchain, err), nil
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
		return failedBuild(BuildFailureUnsupportedToolchain, err), nil
	}
	if err := validateBuildManager(manager); err != nil {
		return failedBuild(BuildFailureManagerNotFound, err), nil
	}
	if err := validateBuildToolchain(toolchain); err != nil {
		return failedBuild(BuildFailureUnsupportedToolchain, err), nil
	}

	source, selection, err := builder.snapshotSource(ctx, work.DeploymentSource)
	if err != nil {
		return failedBuild(BuildFailureInvalidSource, err), nil
	}
	defer func() {
		returnErr = errors.Join(returnErr, source.Close())
	}()

	if managerPin != selection.Manager {
		return failedBuild(
			BuildFailureInvalidSource,
			errors.New("Deployment Manager pin does not match submitted source"),
		), nil
	}
	managerSnapshot, err := builder.snapshotPlatformObject(
		ctx,
		managerArtifact,
		manager.Artifact,
	)
	if err != nil {
		return BuildResult{}, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, managerSnapshot.Close())
	}()
	runtimeSnapshot, err := builder.snapshotPlatformObject(
		ctx,
		runtimeArtifact,
		runtime.Artifact,
	)
	if err != nil {
		return BuildResult{}, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, runtimeSnapshot.Close())
	}()
	toolchainSnapshot, err := builder.snapshotPlatformObject(
		ctx,
		toolchainArtifact,
		toolchain.Artifact,
	)
	if err != nil {
		return BuildResult{}, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, toolchainSnapshot.Close())
	}()
	toolchainDescriptor, err := inspectPinnedToolchain(
		ctx,
		toolchainSnapshot,
		toolchain.Artifact,
	)
	if err != nil {
		return BuildResult{}, err
	}
	if toolchainDescriptor.NodeVersion != runtime.NodeVersion ||
		toolchainDescriptor.RuntimeDigest != runtime.Artifact.Digest {
		return BuildResult{}, errors.New(
			"Toolchain descriptor does not match the Deployment Runtime pin",
		)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return BuildResult{}, fmt.Errorf("rewind submitted source: %w", err)
	}
	guest := BuildGuest{
		Connector: builder.Connector,
		WorkDir:   builder.WorkDir,
		Encoder:   builder.Encoder,
	}
	execution, err := guest.Execute(
		ctx,
		lease.ID,
		BuildGuestRequest{
			FormatVersion:   BuildGuestFormatVersion,
			Manager:         manager,
			Runtime:         runtime,
			Toolchain:       toolchain,
			LockfileName:    selection.LockfileName,
			SourceDigest:    work.DeploymentSource.Digest,
			SourceSizeBytes: work.DeploymentSource.SizeBytes,
		},
		source,
		managerSnapshot,
		runtimeSnapshot,
		toolchainSnapshot,
	)
	if err != nil {
		var failure BuildFailure
		if errors.As(err, &failure) {
			return failedBuild(failure.Reason, failure), nil
		}
		return BuildResult{}, err
	}
	tree := execution.Tree
	fail := func(reason BuildFailureReason, cause error) BuildResult {
		result := failedBuild(reason, cause)
		result.Logs = &execution.Logs
		return result
	}
	defer func() {
		returnErr = errors.Join(returnErr, tree.Close())
	}()
	verification := execution.Verification
	if verification.Outcome == VerificationOutcomeFailed {
		return fail(
			BuildFailureDeclarationAnalysis,
			errors.New(verification.Failed.Error.Message),
		), nil
	}
	plan, err := ParseBuildPlan([]byte(verification.Succeeded.Files[0].Content))
	if err != nil {
		return fail(BuildFailureInvalidPlan, err), nil
	}
	configResultDigest, err := BuildConfigDigest(execution.Config)
	if err != nil {
		return BuildResult{}, err
	}
	provenance := BuildProvenance{
		Architecture:         ArchitectureX8664,
		BuildContractVersion: work.BuildContractVersion,
		Config: ProgramConfig{
			EvaluatorAPIVersion: ConfigEvaluatorAPIVersion,
			SourceDigest:        selection.ConfigDigest,
			ResultDigest:        configResultDigest,
		},
		Manager: ProgramManager{
			Digest:  manager.Artifact.Digest,
			Name:    selection.Manager.Name,
			Version: selection.Manager.Version,
		},
		RuntimeDigest:   runtime.Artifact.Digest,
		ToolchainDigest: toolchain.Artifact.Digest,
		Submitted: ProgramSubmittedSource{
			LockfileDigest: selection.LockfileDigest,
			LockfileName:   selection.LockfileName,
			SourceDigest:   work.DeploymentSource.Digest,
		},
	}

	images, err := builder.buildWorkspaceImages(
		ctx,
		lease,
		plan,
		tree,
		ArchitectureX8664,
	)
	if err != nil {
		var fatal interface{ FatalWorker() bool }
		if errors.As(err, &fatal) && fatal.FatalWorker() {
			return BuildResult{}, err
		}
		return fail(BuildFailureWorkspaceImageFailed, err), nil
	}

	var programOutput *ProgramOutput
	if len(buildPlanProgramDeclarations(plan)) != 0 {
		program, err := EncodeProgram(
			ctx,
			builder.WorkDir,
			builder.Encoder,
			tree,
			verification,
			provenance,
			images,
			toolchainDescriptor.Compiler,
			runtime.NodeVersion,
		)
		if err != nil {
			return fail(BuildFailureOutputInvalid, err), nil
		}
		defer func() {
			returnErr = errors.Join(returnErr, program.Close())
		}()
		if err := ValidateVerifiedProgram(
			verification,
			program.Output.Index,
		); err != nil {
			return BuildResult{}, err
		}
		published, err := program.Publish(ctx, builder.CAS)
		if err != nil {
			return BuildResult{}, err
		}
		programOutput = &published
	}
	result := BuildResult{
		FormatVersion: BuildResultFormatVersion,
		Outcome:       BuildOutcomeSucceeded,
		Logs:          &execution.Logs,
		Succeeded: &BuildSucceeded{
			Plan:            plan,
			Provenance:      provenance,
			Program:         programOutput,
			WorkspaceImages: images,
		},
	}
	if err := ValidateBuildResultTarget(
		result,
		runtime.Artifact.Digest,
		ArchitectureX8664,
	); err != nil {
		return BuildResult{}, err
	}
	return result, nil
}

func (builder Builder) validate() error {
	switch {
	case builder.WorkDir == "" ||
		!filepath.IsAbs(builder.WorkDir) ||
		filepath.Clean(builder.WorkDir) != builder.WorkDir:
		return errors.New("deployment build work directory must be an absolute clean path")
	case builder.CAS == nil:
		return errors.New("deployment build CAS is required")
	case builder.PlatformStore == nil:
		return errors.New("Platform Artifact store is required")
	case builder.Connector == nil:
		return errors.New("build guest connector is required")
	case builder.Images == nil:
		return errors.New("Workspace image builder is required")
	}
	if err := validateProgramEncoder(builder.Encoder); err != nil {
		return err
	}
	return nil
}

func platformArtifactDescriptor(object api.CASObject) ArtifactDescriptor {
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

func (builder Builder) snapshotPlatformObject(
	ctx context.Context,
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
	object, err := builder.PlatformStore.Stat(ctx, descriptor.Digest)
	if err != nil {
		return nil, fmt.Errorf("stat %s Platform Artifact: %w", spec.label, err)
	}
	if object.Digest != descriptor.Digest ||
		object.SizeBytes != descriptor.SizeBytes ||
		object.MediaType != descriptor.MediaType {
		return nil, fmt.Errorf("%s Platform Artifact metadata does not match its pin", spec.label)
	}
	body, err := builder.PlatformStore.Get(ctx, descriptor.Digest)
	if err != nil {
		return nil, fmt.Errorf("open %s Platform Artifact: %w", spec.label, err)
	}
	content, snapshotErr := snapshotArtifact(
		ctx,
		builder.WorkDir,
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

func failedBuild(reason BuildFailureReason, cause error) BuildResult {
	message := "build failed"
	if cause != nil && strings.TrimSpace(cause.Error()) != "" {
		message = cause.Error()
	}
	if len(message) > maxBuildFailureMessageBytes {
		message = truncateUTF8(message, maxBuildFailureMessageBytes)
	}
	result := BuildResult{
		FormatVersion: BuildResultFormatVersion,
		Outcome:       BuildOutcomeFailed,
		Failed: &BuildFailed{Error: BuildError{
			ReasonCode: reason,
			Message:    message,
		}},
	}
	var failure BuildFailure
	if errors.As(cause, &failure) {
		result.Logs = failure.Logs
	}
	return result
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

func (builder Builder) snapshotSource(
	ctx context.Context,
	artifact api.DeploymentSourceArtifact,
) (_ *submittedSource, selection SourceSelection, returnErr error) {
	if artifact.MediaType != api.DeploymentSourceArtifactMediaType ||
		!sha256DigestPattern.MatchString(artifact.Digest) ||
		artifact.SizeBytes < 1 ||
		artifact.SizeBytes > archive.MaxSourceArtifactBytes {
		return nil, SourceSelection{}, errors.New("submitted source descriptor is invalid")
	}
	object, err := builder.CAS.Stat(ctx, artifact.Digest)
	if err != nil {
		return nil, SourceSelection{}, fmt.Errorf("stat submitted source: %w", err)
	}
	if object.Digest != artifact.Digest ||
		object.SizeBytes != artifact.SizeBytes ||
		object.MediaType != artifact.MediaType {
		return nil, SourceSelection{}, errors.New("submitted source object does not match descriptor")
	}
	body, err := builder.CAS.Get(ctx, artifact.Digest)
	if err != nil {
		return nil, SourceSelection{}, fmt.Errorf("open submitted source: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, body.Close())
	}()
	file, err := os.CreateTemp(builder.WorkDir, ".helmr-source-")
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

func (builder Builder) buildWorkspaceImages(
	ctx context.Context,
	lease api.WorkerDeploymentBuildLease,
	plan BuildPlan,
	tree *BuildTree,
	architecture RuntimeArchitecture,
) (_ []WorkspaceImage, returnErr error) {
	workspaces := make([]DefinitionInput, 0)
	for _, definition := range plan.Definitions {
		if definition.Kind == DefinitionKindWorkspace {
			workspaces = append(workspaces, definition)
		}
	}
	if len(workspaces) == 0 {
		return []WorkspaceImage{}, nil
	}
	sourceRoot, cleanup, err := tree.MaterializeApplication(ctx, builder.WorkDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, cleanup())
	}()
	images := make([]WorkspaceImage, 0, len(workspaces))
	for _, definition := range workspaces {
		artifact, err := builder.Images.BuildImage(ctx, buildmodel.Request{
			RunID:       lease.ID,
			WorkspaceID: definition.DeclaredID,
			CacheScope:  lease.EnvironmentID,
			Build:       definition.Workspace.ImageBuild,
			Source:      buildmodel.Source{ProjectRoot: sourceRoot},
		})
		if err != nil {
			return nil, fmt.Errorf("build Workspace %q image: %w", definition.DeclaredID, err)
		}
		object, verifyErr := builder.storeWorkspaceImage(
			ctx,
			artifact,
			architecture,
		)
		cleanupErr := cleanupImageArtifact(artifact)
		if verifyErr != nil || cleanupErr != nil {
			return nil, errors.Join(verifyErr, cleanupErr)
		}
		images = append(images, WorkspaceImage{
			DeclaredID: definition.DeclaredID,
			Artifact: WorkspaceImageArtifact{
				Digest:       object.Digest,
				SizeBytes:    object.SizeBytes,
				MediaType:    object.MediaType,
				Architecture: architecture,
			},
		})
	}
	return images, nil
}

func (builder Builder) storeWorkspaceImage(
	ctx context.Context,
	artifact buildmodel.Artifact,
	architecture RuntimeArchitecture,
) (cas.Object, error) {
	info, err := os.Stat(artifact.ImageTarPath)
	if err != nil {
		return cas.Object{}, err
	}
	if !info.Mode().IsRegular() ||
		info.Size() < 1 ||
		info.Size() > maxWorkspaceImageBytes {
		return cas.Object{}, errors.New(
			"Workspace image size is outside the build contract",
		)
	}
	image, err := os.Open(artifact.ImageTarPath)
	if err != nil {
		return cas.Object{}, err
	}
	metadata, verifyErr := oci.Inspect(image)
	if verifyErr == nil {
		if metadata.ManifestCount != 1 {
			verifyErr = errors.New(
				"Workspace image must contain exactly one OCI manifest",
			)
		}
	}
	if verifyErr == nil {
		verifyErr = validateWorkspaceImagePlatform(
			metadata.Platform,
			architecture,
		)
	}
	closeErr := image.Close()
	if verifyErr != nil || closeErr != nil {
		return cas.Object{}, errors.Join(verifyErr, closeErr)
	}
	image, err = os.Open(artifact.ImageTarPath)
	if err != nil {
		return cas.Object{}, err
	}
	object, putErr := builder.CAS.Put(
		ctx,
		WorkspaceImageArtifactMediaType,
		io.LimitReader(image, maxWorkspaceImageBytes+1),
	)
	closeErr = image.Close()
	if putErr != nil || closeErr != nil {
		return cas.Object{}, errors.Join(putErr, closeErr)
	}
	if object.SizeBytes < 1 || object.SizeBytes > maxWorkspaceImageBytes {
		return cas.Object{}, errors.New("Workspace image size is outside the build contract")
	}
	return object, nil
}

func cleanupImageArtifact(artifact buildmodel.Artifact) error {
	root := filepath.Clean(strings.TrimSpace(artifact.RootPath))
	if root == "" ||
		!filepath.IsAbs(root) ||
		root == string(filepath.Separator) {
		return errors.New("Workspace image artifact root is unsafe")
	}
	for _, name := range []string{
		artifact.ImageTarPath,
		artifact.ConfigPath,
		artifact.ManifestPath,
	} {
		candidate := filepath.Clean(strings.TrimSpace(name))
		relative, err := filepath.Rel(root, candidate)
		if err != nil ||
			relative == "." ||
			relative == ".." ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("Workspace image artifact paths are not confined")
		}
	}
	return os.RemoveAll(root)
}

func validateWorkspaceImagePlatform(
	platform *oci.Platform,
	architecture RuntimeArchitecture,
) error {
	if architecture != ArchitectureX8664 {
		return fmt.Errorf(
			"Workspace image architecture %q is unsupported",
			architecture,
		)
	}
	if platform == nil ||
		platform.OS != "linux" ||
		platform.Architecture != "amd64" {
		return fmt.Errorf(
			"Workspace image platform does not match linux/amd64",
		)
	}
	return nil
}

func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func FindEncoder() (string, error) {
	path, err := exec.LookPath("mksquashfs")
	if err != nil {
		return "", err
	}
	return filepath.Abs(path)
}
