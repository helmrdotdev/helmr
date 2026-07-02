package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
)

func (r GuestRunner) materializeCheckpointObject(ctx context.Context, digest string, suffix string) (string, error) {
	return r.materializeEncryptedObject(ctx, digest, suffix, checkpointPurpose(suffix))
}

func (r GuestRunner) materializeEncryptedObject(ctx context.Context, digest string, suffix string, purpose string) (string, error) {
	if r.CheckpointEncryptor == nil {
		return "", errors.New("checkpoint encryption is required")
	}
	body, err := r.CAS.Get(ctx, digest)
	if err != nil {
		return "", fmt.Errorf("get checkpoint object %s: %w", digest, err)
	}
	if err := os.MkdirAll(r.tempDir(), 0o755); err != nil {
		_ = body.Close()
		return "", fmt.Errorf("create checkpoint temp dir: %w", err)
	}
	file, err := os.CreateTemp(r.tempDir(), "checkpoint-*."+suffix)
	if err != nil {
		_ = body.Close()
		return "", fmt.Errorf("create checkpoint temp file: %w", err)
	}
	path := file.Name()
	hash := sha256.New()
	copyErr := r.CheckpointEncryptor.Decrypt(ctx, io.TeeReader(body, hash), file, purpose)
	bodyCloseErr := body.Close()
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("decrypt checkpoint object %s: %w", digest, copyErr)
	}
	if bodyCloseErr != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close checkpoint object %s: %w", digest, bodyCloseErr)
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close checkpoint object %s: %w", digest, closeErr)
	}
	actual := sha256sum.DigestHash(hash)
	if actual != digest {
		_ = os.Remove(path)
		return "", fmt.Errorf("checkpoint object digest mismatch: expected %s, got %s", digest, actual)
	}
	return path, nil
}

func checkpointFileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return sha256sum.DigestHash(hash), nil
}

func validateRestoreIdentity(checkpoint api.WorkerCheckpointManifest) error {
	runtimeInfo := checkpoint.RecoveryPoint.Runtime
	if runtimeInfo.Backend != "firecracker" {
		return fmt.Errorf("restore checkpoint recovery_point.runtime.backend %q is not supported", runtimeInfo.Backend)
	}
	if runtimeInfo.Arch != runtime.GOARCH {
		return fmt.Errorf("restore checkpoint recovery_point.runtime.arch %q does not match worker arch %q", runtimeInfo.Arch, runtime.GOARCH)
	}
	if strings.TrimSpace(runtimeInfo.ABI) == "" {
		return errors.New("restore checkpoint recovery_point.runtime.abi is required")
	}
	if err := requireCheckpointDigest("recovery_point.runtime.id", runtimeInfo.ID); err != nil {
		return err
	}
	if err := requireCheckpointDigest("recovery_point.runtime.kernel_digest", runtimeInfo.KernelDigest); err != nil {
		return err
	}
	if err := requireCheckpointDigest("recovery_point.runtime.initramfs_digest", runtimeInfo.InitramfsDigest); err != nil {
		return err
	}
	if err := requireCheckpointDigest("recovery_point.runtime.rootfs_digest", runtimeInfo.RootfsDigest); err != nil {
		return err
	}
	if err := requireCheckpointDigest("recovery_point.runtime.config_digest", runtimeInfo.ConfigDigest); err != nil {
		return err
	}
	if runtimeInfo.Substrate != nil {
		if err := requireCheckpointDigest("recovery_point.runtime.substrate.digest", runtimeInfo.Substrate.Digest); err != nil {
			return err
		}
		if strings.TrimSpace(runtimeInfo.Substrate.Format) == "" {
			return errors.New("restore checkpoint recovery_point.runtime.substrate.format is required")
		}
		if strings.TrimSpace(runtimeInfo.Substrate.BuilderABI) == "" {
			return errors.New("restore checkpoint recovery_point.runtime.substrate.builder_abi is required")
		}
		if strings.TrimSpace(runtimeInfo.Substrate.LayoutABI) == "" {
			return errors.New("restore checkpoint recovery_point.runtime.substrate.layout_abi is required")
		}
		if checkpoint.RuntimeState.RuntimeSubstrateArtifact == nil {
			return errors.New("restore checkpoint runtime_state.runtime_substrate_artifact is required")
		}
		substrateArtifact := checkpoint.RuntimeState.RuntimeSubstrateArtifact
		if strings.TrimSpace(substrateArtifact.ID) == "" {
			return errors.New("restore checkpoint runtime_state.runtime_substrate_artifact.id is required")
		}
		if strings.TrimSpace(substrateArtifact.Artifact.Digest) == "" {
			return errors.New("restore checkpoint runtime_state.runtime_substrate_artifact.artifact.digest is required")
		}
		if strings.TrimSpace(substrateArtifact.Artifact.MediaType) == "" {
			return errors.New("restore checkpoint runtime_state.runtime_substrate_artifact.artifact.media_type is required")
		}
	}
	return requireCheckpointArtifact(checkpoint.RuntimeState.ConfigArtifact, "runtime_state.config_artifact")
}

func requireCheckpointDigest(field string, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("restore checkpoint %s is required", field)
	}
	return nil
}

func requireCheckpointArtifact(artifact api.WorkerCheckpointArtifact, field string) error {
	if strings.TrimSpace(artifact.Digest) == "" {
		return fmt.Errorf("restore checkpoint %s.digest is required", field)
	}
	if strings.TrimSpace(artifact.MediaType) == "" {
		return fmt.Errorf("restore checkpoint %s.media_type is required", field)
	}
	return nil
}

type runtimeCheckpointer struct {
	session           vm.CheckpointableSession
	cas               cas.Store
	encryptor         *checkpoint.Encryptor
	tempDir           string
	stream            io.ReadWriteCloser
	workspace         api.WorkerCheckpointWorkspaceBase
	substrateSource   *api.WorkerRuntimeSubstrateSource
	runtimeSubstrates RuntimeSubstrateArtifactRegistrar
	runEvent          func(context.Context, *runv0.RunEvent) error
}

func (c runtimeCheckpointer) CreateCheckpoint(ctx context.Context, request CheckpointRequest) (CheckpointResult, error) {
	if c.cas == nil {
		return CheckpointResult{}, errors.New("checkpoint CAS is required")
	}
	if c.encryptor == nil {
		return CheckpointResult{}, errors.New("checkpoint encryption is required")
	}
	if c.stream == nil {
		return CheckpointResult{}, errors.New("checkpoint control stream is required")
	}
	phases := []api.WorkerCheckpointPhase{}
	recordPhase := func(name string, started time.Time) {
		phases = append(phases, api.WorkerCheckpointPhase{Name: name, DurationMs: durationMilliseconds(time.Since(started))})
	}
	started := time.Now()
	workspaceCapture, err := c.suspendGuestForCheckpoint(ctx, request)
	if err != nil {
		return CheckpointResult{}, err
	}
	recordPhase("suspend_guest", started)
	started = time.Now()
	if err := c.stream.Close(); err != nil {
		_ = c.session.Resume(ctx)
		return CheckpointResult{}, fmt.Errorf("close checkpoint control stream: %w", err)
	}
	recordPhase("close_control_stream", started)
	started = time.Now()
	artifact, err := c.session.CreateSnapshot(ctx, vm.SnapshotRequest{ID: request.CheckpointID})
	if err != nil {
		_ = c.session.Resume(ctx)
		return CheckpointResult{}, err
	}
	recordPhase("create_runtime_snapshot", started)
	phases = append(phases, workerCheckpointPhases(artifact.Phases)...)
	defer func() {
		cleanupSnapshotArtifact(artifact)
	}()
	started = time.Now()
	manifest, err := c.storeSnapshotArtifact(ctx, request, artifact)
	if err != nil {
		_ = c.session.Resume(ctx)
		return CheckpointResult{}, err
	}
	recordPhase("store_checkpoint_artifacts", started)
	started = time.Now()
	if err := c.releaseCheckpointSource(ctx); err != nil {
		return CheckpointResult{}, fmt.Errorf("release checkpoint source: %w", err)
	}
	recordPhase("release_checkpoint_source", started)
	manifest.Phases = phases
	return CheckpointResult{Manifest: manifest, WorkspaceCapture: workspaceCapture}, nil
}

func (c runtimeCheckpointer) releaseCheckpointSource(ctx context.Context) error {
	if releaser, ok := c.session.(CheckpointSourceReleaser); ok {
		return releaser.ReleaseCheckpointSource(ctx)
	}
	return c.session.Close(ctx)
}

func (c runtimeCheckpointer) suspendGuestForCheckpoint(ctx context.Context, request CheckpointRequest) (*workspace.WorkspaceArtifact, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := wire.WriteCheckpointPauseRequest(c.stream, &runv0.CheckpointPauseRequest{
		RunWaitId:        request.RunWaitID,
		CheckpointId:     request.CheckpointID,
		CaptureWorkspace: request.CaptureWorkspace,
	}); err != nil {
		return nil, fmt.Errorf("write checkpoint suspend: %w", err)
	}
	reader := bufio.NewReader(c.stream)
	pauseCtx, cancelPause := context.WithTimeout(ctx, checkpointSuspendTimeout)
	ready, workspaceCapture, err := c.readPauseReadyContext(pauseCtx, reader, request)
	cancelPause()
	if err != nil {
		return nil, fmt.Errorf("read checkpoint pause ready: %w", err)
	}
	if ready.RunWaitId != request.RunWaitID || ready.CheckpointId != request.CheckpointID {
		return nil, fmt.Errorf("checkpoint pause ready mismatch: run_wait_id=%q checkpoint_id=%q", ready.RunWaitId, ready.CheckpointId)
	}
	if request.CaptureWorkspace && workspaceCapture == nil {
		return nil, errors.New("checkpoint pause did not return required workspace capture")
	}
	return workspaceCapture, nil
}

func (c runtimeCheckpointer) readPauseReadyContext(ctx context.Context, reader *bufio.Reader, request CheckpointRequest) (*runv0.CheckpointPauseReady, *workspace.WorkspaceArtifact, error) {
	type pauseReadyResult struct {
		ready            *runv0.CheckpointPauseReady
		workspaceCapture *workspace.WorkspaceArtifact
		err              error
	}
	result := make(chan pauseReadyResult, 1)
	go func() {
		parsed := &runv0.CheckpointPauseReady{}
		workspaceCapture, err := c.readPauseReady(ctx, reader, request, parsed)
		result <- pauseReadyResult{
			ready:            parsed,
			workspaceCapture: workspaceCapture,
			err:              err,
		}
	}()
	select {
	case result := <-result:
		if result.err != nil {
			return nil, nil, result.err
		}
		return result.ready, result.workspaceCapture, nil
	case <-ctx.Done():
		_ = c.stream.Close()
		return nil, nil, ctx.Err()
	}
}

func (c runtimeCheckpointer) readPauseReady(ctx context.Context, reader *bufio.Reader, request CheckpointRequest, ready *runv0.CheckpointPauseReady) (*workspace.WorkspaceArtifact, error) {
	var workspaceCapture *workspace.WorkspaceArtifact
	for {
		prefix, err := reader.Peek(4)
		if err != nil {
			return nil, err
		}
		if frameio.IsStreamFramePrefix(prefix) {
			header, bodyLen, err := wire.ReadStreamFrameHeader(reader)
			if err != nil {
				return nil, err
			}
			switch header.Type {
			case wire.StreamTypeCheckpointPauseReady:
				if bodyLen != 0 {
					return nil, fmt.Errorf("checkpoint pause ready body length %d must be zero", bodyLen)
				}
				if header.RunWaitID != request.RunWaitID || header.CheckpointID != request.CheckpointID {
					return nil, fmt.Errorf("checkpoint pause ready mismatch: run_wait_id=%q checkpoint_id=%q", header.RunWaitID, header.CheckpointID)
				}
				ready.RunWaitId = header.RunWaitID
				ready.CheckpointId = header.CheckpointID
				return workspaceCapture, nil
			case wire.StreamTypeWorkspaceArtifact:
				if !request.CaptureWorkspace {
					return nil, errors.New("checkpoint pause returned unexpected workspace capture")
				}
				if workspaceCapture != nil {
					return nil, errors.New("checkpoint pause returned multiple workspace captures")
				}
				artifact, err := storeWorkspaceArtifactFrame(ctx, c.cas, reader, header, bodyLen, request.RunID)
				if err != nil {
					return nil, err
				}
				workspaceCapture = &artifact
			default:
				return nil, fmt.Errorf("unsupported checkpoint stream type %q", header.Type)
			}
			continue
		}
		body, err := frameio.ReadMessageFrame(reader)
		if err != nil {
			return nil, err
		}
		var event runv0.RunEvent
		if err := proto.Unmarshal(body, &event); err != nil {
			return nil, fmt.Errorf("unmarshal checkpoint interleaved run event: %w", err)
		}
		if c.runEvent == nil {
			return nil, errors.New("received run event while checkpoint pause ready is pending")
		}
		if err := c.runEvent(ctx, &event); err != nil {
			return nil, err
		}
	}
}

func (c runtimeCheckpointer) storeSnapshotArtifact(ctx context.Context, request CheckpointRequest, artifact vm.SnapshotArtifact) (api.WorkerCheckpointManifest, error) {
	var manifest storedCheckpointArtifact
	var state storedCheckpointArtifact
	var scratchDisk storedCheckpointArtifact
	var substrate *api.WorkerRuntimeSubstrateArtifact
	memory := make([]api.WorkerCheckpointArtifact, len(artifact.Memory))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(4)
	group.Go(func() error {
		stored, err := c.storeSnapshotReader(groupCtx, bytes.NewReader(artifact.Manifest), cas.CheckpointRuntimeConfigMediaType, "manifest")
		if err != nil {
			return fmt.Errorf("store checkpoint manifest: %w", err)
		}
		manifest = stored
		return nil
	})
	group.Go(func() error {
		stored, err := c.storeSnapshotFile(groupCtx, artifact.VMState, "vmstate")
		if err != nil {
			return fmt.Errorf("store checkpoint vm state: %w", err)
		}
		state = stored
		return nil
	})
	group.Go(func() error {
		stored, err := c.storeSnapshotFile(groupCtx, artifact.ScratchDisk, "scratch-disk")
		if err != nil {
			return fmt.Errorf("store checkpoint scratch disk: %w", err)
		}
		scratchDisk = stored
		return nil
	})
	if artifact.Substrate != nil {
		group.Go(func() error {
			stored, err := c.ensureRuntimeSubstrateArtifact(groupCtx, artifact.Substrate)
			if err != nil {
				return fmt.Errorf("ensure runtime substrate artifact: %w", err)
			}
			substrate = stored
			return nil
		})
	}
	for i, file := range artifact.Memory {
		group.Go(func() error {
			stored, err := c.storeSnapshotFile(groupCtx, file, "memory")
			if err != nil {
				return fmt.Errorf("store checkpoint memory: %w", err)
			}
			memory[i] = stored.artifact
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return api.WorkerCheckpointManifest{}, err
	}
	for _, artifact := range memory {
		if strings.TrimSpace(artifact.Digest) == "" {
			return api.WorkerCheckpointManifest{}, errors.New("stored checkpoint memory artifact is missing digest")
		}
	}
	return api.WorkerCheckpointManifest{
		RecoveryPoint: api.WorkerCheckpointRecoveryPoint{
			ID:        request.CheckpointID,
			RunID:     request.RunID,
			RunWaitID: request.RunWaitID,
			Runtime: api.WorkerCheckpointRuntime{
				Backend:         artifact.RuntimeBackend,
				ID:              artifact.RuntimeID,
				Arch:            artifact.RuntimeArch,
				ABI:             artifact.RuntimeABI,
				KernelDigest:    artifact.KernelDigest,
				InitramfsDigest: artifact.InitramfsDigest,
				RootfsDigest:    artifact.RootfsDigest,
				ConfigDigest:    artifact.RuntimeConfigDigest,
				Substrate:       checkpointRuntimeSubstrate(artifact.Substrate),
			},
		},
		RuntimeState: api.WorkerCheckpointRuntimeState{
			ConfigArtifact:           manifest.artifact,
			VMStateArtifact:          state.artifact,
			ScratchDiskArtifact:      scratchDisk.artifact,
			RuntimeSubstrateArtifact: substrate,
			MemoryArtifacts:          memory,
			Config:                   artifact.Manifest,
		},
		WorkspaceState: api.WorkerCheckpointWorkspaceState{
			Base: c.workspace,
		},
	}, nil
}

func checkpointRuntimeSubstrate(substrate *vm.RuntimeSubstrate) *api.WorkerCheckpointRuntimeSubstrate {
	if substrate == nil {
		return nil
	}
	return &api.WorkerCheckpointRuntimeSubstrate{
		Digest:     strings.TrimSpace(substrate.Digest),
		Format:     strings.TrimSpace(substrate.Format),
		BuilderABI: strings.TrimSpace(substrate.BuilderABI),
		LayoutABI:  strings.TrimSpace(substrate.LayoutABI),
	}
}

func (c runtimeCheckpointer) ensureRuntimeSubstrateArtifact(ctx context.Context, substrate *vm.RuntimeSubstrate) (*api.WorkerRuntimeSubstrateArtifact, error) {
	if substrate == nil {
		return nil, nil
	}
	if c.substrateSource != nil && runtimeSubstrateArtifactMatches(c.substrateSource.RuntimeSubstrateArtifact, substrate) {
		return c.substrateSource.RuntimeSubstrateArtifact, nil
	}
	if c.runtimeSubstrates == nil {
		return nil, errors.New("runtime substrate artifact registrar is required")
	}
	if c.substrateSource == nil || strings.TrimSpace(c.substrateSource.DeploymentSandboxID) == "" {
		return nil, errors.New("runtime substrate source deployment_sandbox_id is required")
	}
	if lookup, ok := c.runtimeSubstrates.(RuntimeSubstrateArtifactLookup); ok {
		response, err := lookup.LookupRuntimeSubstrateArtifact(ctx, api.WorkerRuntimeSubstrateArtifactLookupRequest{
			DeploymentSandboxID: strings.TrimSpace(c.substrateSource.DeploymentSandboxID),
			SubstrateDigest:     strings.TrimSpace(substrate.Digest),
			Format:              strings.TrimSpace(substrate.Format),
			BuilderABI:          strings.TrimSpace(substrate.BuilderABI),
			LayoutABI:           strings.TrimSpace(substrate.LayoutABI),
		})
		if err == nil {
			artifact := response.RuntimeSubstrateArtifact
			return &artifact, nil
		}
		if !isHTTPStatus(err, 404) {
			return nil, fmt.Errorf("lookup runtime substrate artifact: %w", err)
		}
	}
	if strings.TrimSpace(substrate.Path) == "" {
		return nil, errors.New("runtime substrate path is required")
	}
	body, err := os.Open(substrate.Path)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	info, err := body.Stat()
	if err != nil {
		return nil, err
	}
	encryptStarted := time.Now()
	stage, err := c.cas.Stage(ctx, cas.RuntimeSubstrateMediaType)
	if err != nil {
		return nil, err
	}
	if err := c.encryptor.Encrypt(ctx, body, stage, runtimeSubstratePurpose(substrate.Digest)); err != nil {
		_ = stage.Abort(context.Background())
		return nil, err
	}
	encryptDuration := time.Since(encryptStarted)
	storeStarted := time.Now()
	object, err := stage.Commit(ctx)
	if err != nil {
		_ = stage.Abort(context.Background())
		return nil, err
	}
	source, err := runtimeSubstrateArtifactSource(c.substrateSource, map[string]any{
		"producer":            "checkpoint",
		"encrypt_duration_ms": durationMilliseconds(encryptDuration),
		"store_duration_ms":   durationMilliseconds(time.Since(storeStarted)),
	})
	if err != nil {
		return nil, err
	}
	response, err := c.runtimeSubstrates.RegisterRuntimeSubstrateArtifact(ctx, api.WorkerRuntimeSubstrateArtifactRegisterRequest{
		DeploymentSandboxID: strings.TrimSpace(c.substrateSource.DeploymentSandboxID),
		Artifact: api.CASObject{
			Digest:    object.Digest,
			SizeBytes: object.SizeBytes,
			MediaType: object.MediaType,
		},
		SubstrateDigest: strings.TrimSpace(substrate.Digest),
		Format:          strings.TrimSpace(substrate.Format),
		BuilderABI:      strings.TrimSpace(substrate.BuilderABI),
		LayoutABI:       strings.TrimSpace(substrate.LayoutABI),
		SizeBytes:       info.Size(),
		Source:          source,
	})
	if err != nil {
		return nil, err
	}
	artifact := response.RuntimeSubstrateArtifact
	return &artifact, nil
}

func runtimeSubstrateArtifactSource(source *api.WorkerRuntimeSubstrateSource, metadata map[string]any) ([]byte, error) {
	body := map[string]any{}
	maps.Copy(body, metadata)
	if source != nil {
		body["substrate_source"] = map[string]string{
			"sandbox_artifact_digest": strings.TrimSpace(source.SandboxImageArtifact.Digest),
			"sandbox_artifact_format": strings.TrimSpace(source.SandboxImageArtifactFormat),
			"image_digest":            strings.TrimSpace(source.ImageDigest),
			"rootfs_digest":           strings.TrimSpace(source.RootfsDigest),
			"runtime_abi":             strings.TrimSpace(source.RuntimeABI),
			"guestd_abi":              strings.TrimSpace(source.GuestdABI),
			"adapter_abi":             strings.TrimSpace(source.AdapterABI),
			"workspace_mount_path":    strings.TrimSpace(source.WorkspaceMountPath),
		}
	}
	return json.Marshal(body)
}

type httpStatusError interface {
	HTTPStatusCode() int
}

func isHTTPStatus(err error, statusCode int) bool {
	var statusErr httpStatusError
	return errors.As(err, &statusErr) && statusErr.HTTPStatusCode() == statusCode
}

func runtimeSubstrateArtifactMatches(artifact *api.WorkerRuntimeSubstrateArtifact, substrate *vm.RuntimeSubstrate) bool {
	if artifact == nil || substrate == nil {
		return false
	}
	return strings.TrimSpace(artifact.SubstrateDigest) == strings.TrimSpace(substrate.Digest) &&
		strings.TrimSpace(artifact.Format) == strings.TrimSpace(substrate.Format) &&
		strings.TrimSpace(artifact.BuilderABI) == strings.TrimSpace(substrate.BuilderABI) &&
		strings.TrimSpace(artifact.LayoutABI) == strings.TrimSpace(substrate.LayoutABI) &&
		strings.TrimSpace(artifact.ID) != "" &&
		strings.TrimSpace(artifact.Artifact.Digest) != ""
}

type storedCheckpointArtifact struct {
	artifact api.WorkerCheckpointArtifact
}

func (c runtimeCheckpointer) storeSnapshotFile(ctx context.Context, file vm.SnapshotFile, suffix string) (storedCheckpointArtifact, error) {
	if strings.TrimSpace(file.Path) == "" {
		return storedCheckpointArtifact{}, fmt.Errorf("checkpoint %s path is required", suffix)
	}
	body, err := os.Open(file.Path)
	if err != nil {
		return storedCheckpointArtifact{}, err
	}
	defer body.Close()
	return c.storeSnapshotReader(ctx, body, file.MediaType, suffix)
}

func (c runtimeCheckpointer) storeSnapshotReader(ctx context.Context, body io.Reader, mediaType string, suffix string) (storedCheckpointArtifact, error) {
	encryptStarted := time.Now()
	stage, err := c.cas.Stage(ctx, mediaType)
	if err != nil {
		return storedCheckpointArtifact{}, err
	}
	if err := c.encryptor.Encrypt(ctx, body, stage, checkpointPurpose(suffix)); err != nil {
		_ = stage.Abort(context.Background())
		return storedCheckpointArtifact{}, err
	}
	encryptDuration := time.Since(encryptStarted)
	storeStarted := time.Now()
	object, err := stage.Commit(ctx)
	if err != nil {
		_ = stage.Abort(context.Background())
		return storedCheckpointArtifact{}, err
	}
	return storedCheckpointArtifact{artifact: api.WorkerCheckpointArtifact{
		Digest:            object.Digest,
		SizeBytes:         object.SizeBytes,
		MediaType:         object.MediaType,
		EncryptDurationMs: durationMilliseconds(encryptDuration),
		StoreDurationMs:   durationMilliseconds(time.Since(storeStarted)),
	}}, nil
}

func checkpointPurpose(suffix string) string {
	return "helmr.checkpoint." + suffix
}

func runtimeSubstratePurpose(rawDigest string) string {
	return "helmr.runtime-substrate." + strings.TrimSpace(rawDigest)
}

func workerCheckpointPhases(phases []vm.RuntimePhase) []api.WorkerCheckpointPhase {
	if len(phases) == 0 {
		return nil
	}
	result := make([]api.WorkerCheckpointPhase, 0, len(phases))
	for _, phase := range phases {
		result = append(result, workerCheckpointPhase(phase))
	}
	return result
}

func workerCheckpointPhase(phase vm.RuntimePhase) api.WorkerCheckpointPhase {
	return api.WorkerCheckpointPhase{
		Name:       phase.Name,
		DurationMs: phase.DurationMs,
		Role:       phase.Role,
		MediaType:  phase.MediaType,
		ErrorClass: phase.ErrorClass,
		Filepack:   workerCheckpointFilepackStats(phase.Filepack),
	}
}

func workerCheckpointFilepackStats(stats *vm.FilepackStats) *api.WorkerCheckpointFilepackStats {
	if stats == nil {
		return nil
	}
	return &api.WorkerCheckpointFilepackStats{
		LogicalBytes:       stats.LogicalBytes,
		AllocatedBytes:     stats.AllocatedBytes,
		SparseSupported:    stats.SparseSupported,
		SparseDataRanges:   stats.SparseDataRanges,
		SparseDataBytes:    stats.SparseDataBytes,
		ZeroChunksSkipped:  stats.ZeroChunksSkipped,
		EncodedChunks:      stats.EncodedChunks,
		CompressedBytes:    stats.CompressedBytes,
		UnpackWrittenBytes: stats.UnpackWrittenBytes,
	}
}

func cleanupSnapshotArtifact(artifact vm.SnapshotArtifact) {
	_ = os.Remove(artifact.VMState.Path)
	_ = os.Remove(artifact.ScratchDisk.Path)
	if artifact.Substrate != nil {
		_ = os.Remove(artifact.Substrate.Path)
	}
	for _, file := range artifact.Memory {
		_ = os.Remove(file.Path)
	}
}
