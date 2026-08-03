package programbuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/imagebuild"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type Executor struct {
	WorkDir           string
	CAS               cas.Store
	PlatformStore     cas.Reader
	Connector         vm.Connector
	RuntimeIdentityID string
	Encoder           string
	ImageWorkDir      string
	ImageAdmission    AdmissionClient
	ImageCredentials  RegistryCredentialFetcher
	ImageCache        CacheCredentialFetcher
	ImageCompletion   CompletionClient
}

func (executor Executor) Build(
	ctx context.Context,
	lease workerapi.DeploymentBuildLease,
	work workerapi.DeploymentBuild,
	revocations deployment.ImageOperationRevocations,
) (json.RawMessage, error) {
	if revocations == nil {
		return nil, errors.New("workspace image operation revocations are required")
	}
	result, err := executor.build(ctx, lease, work, revocations)
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
	raw, err := deployment.CanonicalBuildResult(result)
	if err != nil {
		return nil, fmt.Errorf("encode canonical build result: %w", err)
	}
	return json.RawMessage(raw), nil
}

func (executor Executor) build(
	ctx context.Context,
	lease workerapi.DeploymentBuildLease,
	work workerapi.DeploymentBuild,
	revocations deployment.ImageOperationRevocations,
) (_ deployment.BuildResult, returnErr error) {
	if err := executor.validate(); err != nil {
		return deployment.BuildResult{}, err
	}
	inputs, err := deployment.OpenProgramBuildInputs(ctx, deployment.ProgramBuildInputRequest{
		WorkDir:       executor.WorkDir,
		SourceStore:   executor.CAS,
		PlatformStore: executor.PlatformStore,
		Build:         work,
		Encoder:       executor.Encoder,
	})
	if err != nil {
		var inputFailure *deployment.ProgramBuildInputFailure
		if errors.As(err, &inputFailure) {
			return deployment.NewFailedBuildResult(inputFailure.Reason, inputFailure, nil), nil
		}
		return deployment.BuildResult{}, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, inputs.Close())
	}()

	manager := inputs.Manager()
	runtime := inputs.Runtime()
	toolchain := inputs.Toolchain()
	selection := inputs.Selection()
	guest := buildGuest{
		connector: executor.Connector,
		workDir:   executor.WorkDir,
		encoder:   executor.Encoder,
	}
	execution, err := guest.execute(
		ctx,
		vm.WorkloadBinding{
			WorkerEpoch:       lease.WorkerEpoch,
			OwnerID:           lease.ID,
			Generation:        lease.LeaseSequence,
			RuntimeIdentityID: executor.RuntimeIdentityID,
		},
		deployment.BuildGuestRequest{
			FormatVersion:   deployment.BuildGuestFormatVersion,
			Manager:         manager,
			Runtime:         runtime,
			Toolchain:       toolchain,
			LockfileName:    selection.LockfileName,
			SourceDigest:    work.DeploymentSource.Digest,
			SourceSizeBytes: work.DeploymentSource.SizeBytes,
		},
		inputs.Source(),
		inputs.ManagerSnapshot(),
		inputs.RuntimeSnapshot(),
		inputs.ToolchainSnapshot(),
	)
	if err != nil {
		var failure buildFailure
		if errors.As(err, &failure) {
			return deployment.NewFailedBuildResult(failure.reason, failure, failure.logs), nil
		}
		return deployment.BuildResult{}, err
	}
	tree := execution.tree
	fail := func(reason deployment.BuildFailureReason, cause error) deployment.BuildResult {
		return deployment.NewFailedBuildResult(reason, cause, &execution.logs)
	}
	defer func() {
		returnErr = errors.Join(returnErr, tree.Close())
	}()
	verification := execution.verification
	if verification.Outcome == deployment.VerificationOutcomeFailed {
		return fail(
			deployment.BuildFailureDeclarationAnalysis,
			errors.New(verification.Failed.Error.Message),
		), nil
	}
	plan, err := deployment.ParseBuildPlan([]byte(verification.Succeeded.Files[0].Content))
	if err != nil {
		return fail(deployment.BuildFailureInvalidPlan, err), nil
	}
	configResultDigest, err := deployment.BuildConfigDigest(execution.config)
	if err != nil {
		return deployment.BuildResult{}, err
	}
	provenance := deployment.BuildProvenance{
		Architecture:         deployment.ArchitectureX8664,
		BuildContractVersion: work.BuildContractVersion,
		Config: deployment.ProgramConfig{
			EvaluatorAPIVersion: deployment.ConfigEvaluatorAPIVersion,
			SourceDigest:        selection.ConfigDigest,
			ResultDigest:        configResultDigest,
		},
		Manager: deployment.ProgramManager{
			Digest:  manager.Artifact.Digest,
			Name:    selection.Manager.Name,
			Version: selection.Manager.Version,
		},
		RuntimeDigest:   runtime.Artifact.Digest,
		ToolchainDigest: toolchain.Artifact.Digest,
		Submitted: deployment.ProgramSubmittedSource{
			LockfileDigest: selection.LockfileDigest,
			LockfileName:   selection.LockfileName,
			SourceDigest:   work.DeploymentSource.Digest,
		},
	}

	images, err := executor.buildWorkspaceImages(
		ctx,
		lease,
		work,
		plan,
		tree,
		execution.treeDescriptor,
		deployment.ArchitectureX8664,
		revocations,
	)
	if err != nil {
		var fatal interface{ FatalWorker() bool }
		if errors.As(err, &fatal) && fatal.FatalWorker() {
			return deployment.BuildResult{}, err
		}
		return fail(workspaceImageFailureReason(err), err), nil
	}

	var programOutput *deployment.ProgramOutput
	if deployment.BuildPlanHasProgram(plan) {
		program, err := deployment.EncodeProgram(
			ctx,
			executor.WorkDir,
			executor.Encoder,
			tree,
			verification,
			provenance,
			images,
			inputs.ToolchainDescriptor().Compiler,
			runtime.NodeVersion,
		)
		if err != nil {
			return fail(deployment.BuildFailureOutputInvalid, err), nil
		}
		defer func() {
			returnErr = errors.Join(returnErr, program.Close())
		}()
		if err := deployment.ValidateVerifiedProgram(
			verification,
			program.Output.Index,
		); err != nil {
			return deployment.BuildResult{}, err
		}
		published, err := program.Publish(ctx, executor.CAS)
		if err != nil {
			return deployment.BuildResult{}, err
		}
		programOutput = &published
	}
	result := deployment.BuildResult{
		FormatVersion: deployment.BuildResultFormatVersion,
		Outcome:       deployment.BuildOutcomeSucceeded,
		Logs:          &execution.logs,
		Succeeded: &deployment.BuildSucceeded{
			Plan:            plan,
			Provenance:      provenance,
			Program:         programOutput,
			WorkspaceImages: images,
		},
	}
	if err := deployment.ValidateBuildResultTarget(
		result,
		runtime.Artifact.Digest,
		deployment.ArchitectureX8664,
	); err != nil {
		return deployment.BuildResult{}, err
	}
	return result, nil
}

func (executor Executor) validate() error {
	switch {
	case executor.WorkDir == "" || !filepath.IsAbs(executor.WorkDir) ||
		filepath.Clean(executor.WorkDir) != executor.WorkDir:
		return errors.New("deployment build work directory must be an absolute clean path")
	case executor.CAS == nil:
		return errors.New("deployment build CAS is required")
	case executor.PlatformStore == nil:
		return errors.New("platform artifact store is required")
	case executor.Connector == nil:
		return errors.New("build guest connector is required")
	case strings.TrimSpace(executor.RuntimeIdentityID) == "":
		return errors.New("build worker runtime identity is required")
	case executor.ImageWorkDir == "" || !filepath.IsAbs(executor.ImageWorkDir) ||
		filepath.Clean(executor.ImageWorkDir) != executor.ImageWorkDir:
		return errors.New("workspace image VM work directory must be absolute and clean")
	case executor.ImageAdmission == nil || executor.ImageCredentials == nil ||
		executor.ImageCompletion == nil:
		return errors.New("workspace image VM engine dependencies are incomplete")
	}
	return nil
}

func (executor Executor) imageEngine() imageEngine {
	return imageEngine{
		Connector:   executor.Connector,
		Admission:   executor.ImageAdmission,
		Credentials: executor.ImageCredentials,
		Cache:       executor.ImageCache,
		Completion:  executor.ImageCompletion,
		WorkDir:     executor.ImageWorkDir,
	}
}

func workspaceImageFailureReason(err error) deployment.BuildFailureReason {
	var imageFailure *guestFailure
	if errors.As(err, &imageFailure) && imageFailure.Reason == imagebuild.GuestFailureNetworkQuota {
		return deployment.BuildFailureNetworkLimit
	}
	return deployment.BuildFailureWorkspaceImageFailed
}

func (executor Executor) buildWorkspaceImages(
	ctx context.Context,
	lease workerapi.DeploymentBuildLease,
	work workerapi.DeploymentBuild,
	plan deployment.BuildPlan,
	tree *deployment.BuildTree,
	treeDescriptor deployment.BuildTreeDescriptor,
	architecture deployment.RuntimeArchitecture,
	revocations deployment.ImageOperationRevocations,
) ([]deployment.WorkspaceImage, error) {
	workspaces := make([]deployment.DefinitionInput, 0)
	for _, definition := range plan.Definitions {
		if definition.Kind == deployment.DefinitionKindWorkspace {
			workspaces = append(workspaces, definition)
		}
	}
	if len(workspaces) == 0 {
		return []deployment.WorkspaceImage{}, nil
	}
	engine := executor.imageEngine()
	images := make([]deployment.WorkspaceImage, 0, len(workspaces))
	for _, definition := range workspaces {
		source, err := tree.SelectImageSource(ctx, definition.Workspace.ImageBuild)
		if err != nil {
			return nil, fmt.Errorf("select workspace %q image source: %w", definition.DeclaredID, err)
		}
		artifact, err := engine.BuildWorkspaceImage(ctx, buildRequest{
			Lease: LeaseAuthority{
				ID: lease.ID, OrgID: lease.OrgID, ProjectID: lease.ProjectID,
				EnvironmentID: lease.EnvironmentID, DeploymentID: lease.DeploymentID,
				WorkerGroupID: lease.WorkerGroupID, WorkerInstanceID: lease.WorkerInstanceID,
				WorkerEpoch: lease.WorkerEpoch, Generation: lease.LeaseSequence,
				WorkerProtocolVersion:            lease.WorkerProtocolVersion,
				RequestedGuestEphemeralDiskBytes: lease.RequestedGuestEphemeralDiskBytes,
				RequestedCPUMillis:               lease.RequestedCPUMillis,
				RequestedMemoryBytes:             lease.RequestedMemoryBytes,
				RequestedBuildExecutors:          lease.RequestedBuildExecutors,
			},
			RuntimeIdentityID:     executor.RuntimeIdentityID,
			DeclarationSlot:       definition.DeclaredID,
			Architecture:          string(architecture),
			Plan:                  definition.Workspace.ImageBuild,
			SubmittedSourceDigest: work.DeploymentSource.Digest,
			BuildTreeDigest:       treeDescriptor.Digest,
			BuildTreeSizeBytes:    treeDescriptor.SizeBytes,
			RequestedCacheMode:    imagebuild.CacheMode(work.ImageCacheMode),
			Source:                source,
		}, revocations)
		if err != nil {
			return nil, fmt.Errorf("build workspace %q image: %w", definition.DeclaredID, err)
		}
		object, verifyErr := executor.storeWorkspaceImage(ctx, artifact)
		if verifyErr == nil {
			verifyErr = engine.CompleteWorkspaceImage(ctx, artifact, PublishedArtifact{
				Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType,
			})
		}
		cleanupErr := artifact.close()
		if verifyErr != nil || cleanupErr != nil {
			return nil, errors.Join(verifyErr, cleanupErr)
		}
		images = append(images, deployment.WorkspaceImage{
			DeclaredID: definition.DeclaredID,
			Operation: deployment.WorkspaceImageOperationEvidence{
				BuildLeaseID:         artifact.Evidence.Lease.ID,
				BuildLeaseGeneration: artifact.Evidence.Lease.Generation,
				DeclarationSlot:      artifact.Evidence.DeclarationSlot,
				OperationID:          artifact.Evidence.OperationID,
				RequestFingerprint:   artifact.Evidence.RequestFingerprint,
				AttemptID:            artifact.Evidence.AttemptID,
				PlanDigest:           artifact.Evidence.PlanDigest,
				ResolutionSetDigest:  artifact.Evidence.ResolutionSetDigest,
				RequestedCacheMode:   artifact.Evidence.RequestedCacheMode,
			},
			Artifact: deployment.WorkspaceImageArtifact{
				Digest:       object.Digest,
				SizeBytes:    object.SizeBytes,
				MediaType:    object.MediaType,
				Architecture: architecture,
			},
		})
	}
	return images, nil
}

func (executor Executor) storeWorkspaceImage(
	ctx context.Context,
	artifact *artifact,
) (cas.Object, error) {
	if artifact == nil || artifact.SizeBytes < 1 ||
		artifact.SizeBytes > deployment.MaxWorkspaceImageBytes {
		return cas.Object{}, errors.New("workspace image size is outside the build contract")
	}
	if artifact.Replayed {
		object, err := executor.CAS.Stat(ctx, artifact.Digest)
		if err != nil {
			return cas.Object{}, fmt.Errorf("stat replayed workspace image: %w", err)
		}
		if object.Digest != artifact.Digest || object.SizeBytes != artifact.SizeBytes ||
			object.MediaType != deployment.WorkspaceImageArtifactMediaType {
			return cas.Object{}, errors.New("replayed workspace image does not exact-match CAS")
		}
		return object, nil
	}
	image, err := artifact.open()
	if err != nil {
		return cas.Object{}, err
	}
	object, putErr := executor.CAS.Put(
		ctx,
		deployment.WorkspaceImageArtifactMediaType,
		io.LimitReader(image, deployment.MaxWorkspaceImageBytes+1),
	)
	closeErr := image.Close()
	if putErr != nil || closeErr != nil {
		return cas.Object{}, errors.Join(putErr, closeErr)
	}
	if object.SizeBytes < 1 || object.SizeBytes > deployment.MaxWorkspaceImageBytes {
		return cas.Object{}, errors.New("workspace image size is outside the build contract")
	}
	if object.Digest != artifact.Digest || object.SizeBytes != artifact.SizeBytes {
		return cas.Object{}, errors.New("workspace image CAS result does not match the guest result")
	}
	return object, nil
}

func FindEncoder() (string, error) {
	path, err := exec.LookPath("mksquashfs")
	if err != nil {
		return "", err
	}
	return filepath.Abs(path)
}

type buildDeliveryError struct {
	err error
}

func (err *buildDeliveryError) Error() string {
	return err.err.Error()
}

func (err *buildDeliveryError) Unwrap() error {
	return err.err
}

func (err *buildDeliveryError) DeploymentBuildDeliveryFailureReason() workerapi.DeploymentBuildDeliveryFailureReason {
	return workerapi.DeploymentBuildDeliveryBuildGuestFailed
}

func buildGuestDeliveryFailure(err error) error {
	return &buildDeliveryError{err: err}
}
