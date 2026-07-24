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
	WorkDir      string
	CAS          cas.Store
	RuntimeStore cas.Reader
	Managers     *ManagerStore
	Acquirer     ManagerAcquirer
	Policy       *BuildPolicy
	Toolchains   *ToolchainCorpus
	Connector    vm.Connector
	Encoder      string
	Images       buildmodel.Engine
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
	runtimeDescriptor, err := RuntimeDescriptorFromWire(work.Runtime)
	if err != nil {
		return failedBuild(BuildFailureUnsupportedToolchain, err), nil
	}
	target, err := builder.Policy.Resolve(
		runtimeDescriptor.Digest,
		work.StandardToolchainDigest,
		work.BuildContractVersion,
	)
	if err != nil || target.Runtime != runtimeDescriptor {
		return failedBuild(
			BuildFailureUnsupportedToolchain,
			errors.New("Deployment build target is not admitted by Worker policy"),
		), nil
	}
	toolchain, err := builder.Policy.ResolveToolchain(
		target.StandardToolchainDigest,
	)
	if err != nil {
		return failedBuild(BuildFailureUnsupportedToolchain, err), nil
	}

	source, selection, err := builder.snapshotSource(ctx, work.DeploymentSource)
	if err != nil {
		return failedBuild(BuildFailureInvalidSource, err), nil
	}
	defer func() {
		returnErr = errors.Join(returnErr, source.Close())
	}()

	selector := NewManagerSelector(selection.Manager, target.Runtime.Architecture)
	capsule, err := builder.Acquirer.Acquire(ctx, selector)
	if err != nil {
		var guestError *vm.GuestError
		if errors.As(err, &guestError) {
			return BuildResult{}, err
		}
		reason := BuildFailureManagerNotFound
		if errors.Is(err, ErrManagerProtocolUnsupported) {
			reason = BuildFailureManagerUnsupported
		}
		return failedBuild(reason, err), nil
	}
	capsuleDigest, err := ManagerCapsuleDigest(capsule)
	if err != nil {
		return BuildResult{}, err
	}

	managerSnapshot, err := builder.Managers.Snapshot(
		ctx,
		builder.WorkDir,
		capsule,
	)
	if err != nil {
		return BuildResult{}, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, managerSnapshot.Close())
	}()
	runtimeSnapshot, err := SnapshotRuntimeObject(
		ctx,
		builder.RuntimeStore,
		builder.WorkDir,
		target.Runtime,
	)
	if err != nil {
		return BuildResult{}, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, runtimeSnapshot.Close())
	}()
	toolchainFile, err := builder.Toolchains.OpenToolchain(ctx, toolchain)
	if err != nil {
		return BuildResult{}, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, toolchainFile.Close())
	}()
	toolchainSnapshot, err := snapshotToolchain(
		ctx,
		builder.WorkDir,
		toolchain.ToolchainClosure,
		toolchainFile.File(),
	)
	if err != nil {
		return BuildResult{}, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, toolchainSnapshot.Close())
	}()

	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return BuildResult{}, fmt.Errorf("rewind submitted source: %w", err)
	}
	guest := BuildGuest{
		Connector: builder.Connector,
		WorkDir:   builder.WorkDir,
		Encoder:   builder.Encoder,
	}
	install, err := guest.Install(
		ctx,
		lease.ID,
		BuildInstallRequest{
			FormatVersion:        BuildGuestFormatVersion,
			Manager:              capsule,
			ManagerCapsuleDigest: capsuleDigest,
			Runtime:              target.Runtime,
			StandardToolchain:    toolchain,
			SourceDigest:         work.DeploymentSource.Digest,
			SourceSizeBytes:      work.DeploymentSource.SizeBytes,
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
	tree := install.Tree
	fail := func(reason BuildFailureReason, cause error) BuildResult {
		result := failedBuild(reason, cause)
		result.Logs = &install.Logs
		return result
	}
	defer func() {
		returnErr = errors.Join(returnErr, tree.Close())
	}()
	treeDescriptor, err := tree.Descriptor()
	if err != nil {
		return BuildResult{}, err
	}
	analysis, err := guest.Analyze(
		ctx,
		lease.ID,
		BuildAnalysisRequest{
			FormatVersion:     BuildGuestFormatVersion,
			Runtime:           target.Runtime,
			StandardToolchain: toolchain,
			Tree:              treeDescriptor,
		},
		runtimeSnapshot,
		toolchainSnapshot,
		tree,
	)
	if err != nil {
		return BuildResult{}, err
	}
	if analysis.Outcome == AnalysisOutcomeFailed {
		return fail(
			BuildFailureAnalysisFailed,
			errors.New(analysis.Failed.Error.Message),
		), nil
	}
	plan, err := ParseBuildPlan([]byte(analysis.Succeeded.Files[0].Content))
	if err != nil {
		return fail(BuildFailureInvalidPlan, err), nil
	}
	provenance := BuildProvenance{
		Architecture:         target.Runtime.Architecture,
		BuildContractVersion: target.BuildContractVersion,
		Manager: ProgramManager{
			CapsuleDigest: capsuleDigest,
			Name:          selection.Manager.Name,
			Version:       selection.Manager.Version,
		},
		RuntimeDigest:           target.Runtime.Digest,
		StandardToolchainDigest: target.StandardToolchainDigest,
		Submitted: ProgramSubmittedSource{
			LockfileDigest: selection.LockfileDigest,
			LockfileName:   selection.LockfileName,
			SourceDigest:   work.DeploymentSource.Digest,
		},
	}

	var programOutput *ProgramOutput
	if len(buildPlanProgramDeclarations(plan)) != 0 {
		program, err := EncodeProgram(
			ctx,
			builder.WorkDir,
			builder.Encoder,
			tree,
			analysis,
			provenance,
		)
		if err != nil {
			return fail(BuildFailureOutputInvalid, err), nil
		}
		defer func() {
			returnErr = errors.Join(returnErr, program.Close())
		}()
		proof, err := guest.Prove(
			ctx,
			lease.ID,
			ProgramProofRequest{
				FormatVersion: BuildGuestFormatVersion,
				Runtime:       target.Runtime,
				Program:       program.Output.Artifact,
			},
			runtimeSnapshot,
			program,
		)
		if err != nil {
			return BuildResult{}, err
		}
		if proof.Outcome == ProgramProofFailed {
			return fail(
				BuildFailureProgramInvalid,
				errors.New(proof.Error.Message),
			), nil
		}
		if err := ValidateProgramProof(proof, program.Output.Index); err != nil {
			return BuildResult{}, err
		}
		published, err := program.Publish(ctx, builder.CAS)
		if err != nil {
			return BuildResult{}, err
		}
		programOutput = &published
	}

	images, err := builder.buildWorkspaceImages(
		ctx,
		lease,
		plan,
		tree,
	)
	if err != nil {
		var fatal interface{ FatalWorker() bool }
		if errors.As(err, &fatal) && fatal.FatalWorker() {
			return BuildResult{}, err
		}
		return fail(BuildFailureWorkspaceImageFailed, err), nil
	}
	result := BuildResult{
		FormatVersion: BuildResultFormatVersion,
		Outcome:       BuildOutcomeSucceeded,
		Logs:          &install.Logs,
		Succeeded: &BuildSucceeded{
			Plan:            plan,
			Provenance:      provenance,
			Program:         programOutput,
			WorkspaceImages: images,
		},
	}
	if err := ValidateBuildResultTarget(
		result,
		target.Runtime.Digest,
		target.Runtime.Architecture,
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
	case builder.RuntimeStore == nil:
		return errors.New("managed Runtime store is required")
	case builder.Managers == nil:
		return errors.New("Manager store is required")
	case builder.Policy == nil:
		return errors.New("build policy is required")
	case builder.Toolchains == nil:
		return errors.New("standard toolchain corpus is required")
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
			definition.Workspace.Architecture,
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
				Architecture: definition.Workspace.Architecture,
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
	expected := ""
	switch architecture {
	case ArchitectureX8664:
		expected = "amd64"
	case ArchitectureAArch64:
		expected = "arm64"
	default:
		return fmt.Errorf(
			"Workspace image architecture %q is unsupported",
			architecture,
		)
	}
	if platform == nil ||
		platform.OS != "linux" ||
		platform.Architecture != expected {
		return fmt.Errorf(
			"Workspace image platform does not match linux/%s",
			expected,
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
