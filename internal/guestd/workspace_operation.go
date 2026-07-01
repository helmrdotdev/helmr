package guestd

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/archive"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/transport"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/helmrdotdev/helmr/internal/workspace/protocol"
)

type workspaceOperationRegistry struct {
	mu              sync.RWMutex
	entries         map[string]*workspaceMountEntry
	preparedRuntime *preparedWorkspaceRuntime
}

type workspaceMountEntry struct {
	channelToken      string
	workspaceID       string
	fencingGeneration uint64
	imageRoot         string
	imageConfig       ociRuntimeConfig
	runtimeUser       *resolvedRuntimeUser
	workspaceMount    string
	workspaceRoot     string
	cleanup           func()
	events            chan *workspacev0.WorkspaceOperationEvent
	eventsDone        chan struct{}
	eventsDoneOnce    sync.Once
	processesMu       sync.Mutex
	processes         map[string]*workspaceProcess
	active            int
	retired           bool
}

type preparedWorkspaceRuntime struct {
	key            string
	sandboxDigest  string
	imageRoot      string
	imageConfig    ociRuntimeConfig
	runtimeUser    *resolvedRuntimeUser
	workspaceMount string
	workspaceRoot  string
	cleanup        func()
}

func newWorkspaceOperationRegistry() *workspaceOperationRegistry {
	return &workspaceOperationRegistry{entries: map[string]*workspaceMountEntry{}}
}

func (r *workspaceOperationRegistry) setPreparedRuntime(runtime *preparedWorkspaceRuntime) {
	if runtime == nil {
		return
	}
	r.mu.Lock()
	previous := r.preparedRuntime
	r.preparedRuntime = runtime
	r.mu.Unlock()
	if previous != nil && previous.cleanup != nil {
		previous.cleanup()
	}
}

func (r *workspaceOperationRegistry) takePreparedRuntime(key string, sandboxDigest string, workspaceMount string) (*preparedWorkspaceRuntime, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prepared := r.preparedRuntime
	if prepared == nil {
		return nil, false
	}
	if strings.TrimSpace(prepared.key) != strings.TrimSpace(key) ||
		strings.TrimSpace(prepared.sandboxDigest) != strings.TrimSpace(sandboxDigest) ||
		strings.TrimSpace(prepared.workspaceMount) != strings.TrimSpace(workspaceMount) {
		return nil, false
	}
	r.preparedRuntime = nil
	return prepared, true
}

func (r *workspaceOperationRegistry) register(workspaceMountID string, entry *workspaceMountEntry) {
	r.mu.Lock()
	previous := r.entries[workspaceMountID]
	r.entries[workspaceMountID] = entry
	var cleanup func()
	if previous != nil {
		previous.retired = true
		if previous.active == 0 {
			cleanup = previous.cleanup
		}
	}
	r.mu.Unlock()
	if cleanup != nil {
		cleanup()
	}
}

func (r *workspaceOperationRegistry) acquire(workspaceMountID string, workspaceID string, token string, fencingGeneration uint64) (*workspaceMountEntry, func(), bool) {
	r.mu.Lock()
	entry, ok := r.entries[workspaceMountID]
	workspaceID = strings.TrimSpace(workspaceID)
	token = strings.TrimSpace(token)
	if !(ok &&
		workspaceID != "" &&
		token != "" &&
		fencingGeneration != 0 &&
		entry.workspaceID == workspaceID &&
		entry.fencingGeneration <= fencingGeneration &&
		!entry.retired &&
		subtle.ConstantTimeCompare([]byte(entry.channelToken), []byte(token)) == 1) {
		r.mu.Unlock()
		return nil, func() {}, false
	}
	if fencingGeneration > entry.fencingGeneration {
		entry.fencingGeneration = fencingGeneration
	}
	entry.active++
	r.mu.Unlock()
	return entry, func() { r.release(entry) }, true
}

func (r *workspaceOperationRegistry) release(entry *workspaceMountEntry) {
	r.mu.Lock()
	if entry.active > 0 {
		entry.active--
	}
	var cleanup func()
	if entry.retired && entry.active == 0 {
		cleanup = entry.cleanup
		entry.cleanup = nil
	}
	r.mu.Unlock()
	if cleanup != nil {
		cleanup()
	}
}

func (r *workspaceOperationRegistry) retire(workspaceMountID string, entry *workspaceMountEntry) {
	r.mu.Lock()
	current := r.entries[workspaceMountID]
	if current != entry {
		r.mu.Unlock()
		return
	}
	delete(r.entries, workspaceMountID)
	entry.retired = true
	var cleanup func()
	if entry.active == 0 {
		cleanup = entry.cleanup
		entry.cleanup = nil
	}
	r.mu.Unlock()
	if cleanup != nil {
		cleanup()
	}
}

func handleWorkspaceMaterializeConnection(_ context.Context, conn io.ReadWriter, logger *slog.Logger, registry *workspaceOperationRegistry) error {
	if logger == nil {
		logger = slog.Default()
	}
	totalStarted := time.Now()
	var request workspacev0.MaterializeWorkspaceRequest
	if err := transport.ReadProtoFrame(conn, &request); err != nil {
		return fmt.Errorf("read workspace materialize request: %w", err)
	}
	envelope := request.GetEnvelope()
	if envelope == nil {
		return errors.New("workspace materialize envelope is required")
	}
	if strings.TrimSpace(envelope.WorkspaceMountId) == "" {
		return errors.New("workspace materialize workspace_mount_id is required")
	}
	if strings.TrimSpace(envelope.WorkspaceId) == "" {
		return errors.New("workspace materialize workspace_id is required")
	}
	if strings.TrimSpace(envelope.ChannelToken) == "" {
		return errors.New("workspace materialize channel_token is required")
	}
	if envelope.FencingGeneration == 0 {
		return errors.New("workspace materialize fencing_generation is required")
	}
	workspaceMountID := strings.TrimSpace(envelope.WorkspaceMountId)
	workspaceID := strings.TrimSpace(envelope.WorkspaceId)
	entry, phases, err := restoreWorkspaceMount(conn, &request, logger, registry)
	if err != nil {
		phases = appendWorkspaceMountFailurePhase(phases, "guest_materialize", totalStarted, err)
		writeErr := transport.WriteProtoFrame(conn, &workspacev0.MaterializeWorkspaceResponse{
			State:  "failed",
			Phases: phases,
		})
		if writeErr != nil {
			return errors.Join(fmt.Errorf("restore materialized workspace: %w", err), fmt.Errorf("write workspace materialize failure response: %w", writeErr))
		}
		return fmt.Errorf("restore materialized workspace: %w", err)
	}
	entry.channelToken = envelope.ChannelToken
	entry.workspaceID = workspaceID
	entry.fencingGeneration = envelope.FencingGeneration
	registerStarted := time.Now()
	registry.register(envelope.WorkspaceMountId, entry)
	phases = append(phases, workspaceMountPhase("guest_register", registerStarted, 0, 0, nil))
	logger.Info("workspace materialize registered", "workspace_id", workspaceID, "workspace_mount_id", workspaceMountID, "duration_ms", time.Since(totalStarted).Milliseconds())
	return transport.WriteProtoFrame(conn, &workspacev0.MaterializeWorkspaceResponse{
		State:                  "running",
		GuestdChannelTokenHash: sha256sum.HexBytes([]byte(strings.TrimSpace(envelope.ChannelToken))),
		Phases:                 phases,
	})
}

func handleWorkspaceRuntimePrepareConnection(_ context.Context, conn io.ReadWriter, logger *slog.Logger, registry *workspaceOperationRegistry) error {
	if logger == nil {
		logger = slog.Default()
	}
	totalStarted := time.Now()
	var request workspacev0.PrepareWorkspaceRuntimeRequest
	if err := transport.ReadProtoFrame(conn, &request); err != nil {
		return fmt.Errorf("read workspace runtime prepare request: %w", err)
	}
	runtime, phases, err := restorePreparedWorkspaceRuntime(conn, &request, logger)
	if err != nil {
		phases = appendWorkspaceMountFailurePhase(phases, "guest_runtime_prepare", totalStarted, err)
		writeErr := transport.WriteProtoFrame(conn, &workspacev0.PrepareWorkspaceRuntimeResponse{
			State:      "failed",
			RuntimeKey: strings.TrimSpace(request.RuntimeKey),
			Phases:     phases,
		})
		if writeErr != nil {
			return errors.Join(fmt.Errorf("restore prepared workspace runtime: %w", err), fmt.Errorf("write workspace runtime prepare failure response: %w", writeErr))
		}
		return fmt.Errorf("restore prepared workspace runtime: %w", err)
	}
	registry.setPreparedRuntime(runtime)
	logger.Info("workspace runtime prepared", "runtime_key_id", runtimeKeyLogID(request.RuntimeKey))
	return transport.WriteProtoFrame(conn, &workspacev0.PrepareWorkspaceRuntimeResponse{
		State:      "prepared",
		RuntimeKey: strings.TrimSpace(request.RuntimeKey),
		Phases:     phases,
	})
}

func runtimeKeyLogID(key string) string {
	hash := sha256sum.HexBytes([]byte(strings.TrimSpace(key)))
	if len(hash) < 16 {
		return hash
	}
	return hash[:16]
}

func restorePreparedWorkspaceRuntime(conn io.Reader, request *workspacev0.PrepareWorkspaceRuntimeRequest, logger *slog.Logger) (*preparedWorkspaceRuntime, []*workspacev0.WorkspaceMountPhase, error) {
	var phases []*workspacev0.WorkspaceMountPhase
	runtimeKey := strings.TrimSpace(request.GetRuntimeKey())
	if runtimeKey == "" {
		return nil, phases, errors.New("workspace runtime prepare runtime_key is required")
	}
	mountPath := filepath.Clean(strings.TrimSpace(request.GetMountPath()))
	if mountPath == "" || mountPath == "." || mountPath == string(filepath.Separator) || !filepath.IsAbs(mountPath) {
		return nil, phases, fmt.Errorf("workspace runtime prepare mount_path %q is invalid", request.GetMountPath())
	}
	sandbox := request.GetSandboxArtifact()
	if sandbox == nil {
		return nil, phases, errors.New("workspace runtime prepare sandbox_artifact is required")
	}
	if strings.TrimSpace(sandbox.GetDigest()) == "" {
		return nil, phases, errors.New("workspace runtime prepare sandbox_artifact digest is required")
	}
	if strings.TrimSpace(sandbox.GetMediaType()) != "application/vnd.helmr.sandbox-image.v0.oci-tar" {
		return nil, phases, fmt.Errorf("workspace runtime prepare sandbox_artifact media_type %q is not supported", sandbox.GetMediaType())
	}
	if strings.TrimSpace(sandbox.GetEncoding()) != "oci-tar" {
		return nil, phases, fmt.Errorf("workspace runtime prepare sandbox_artifact encoding %q is not supported", sandbox.GetEncoding())
	}
	if sandbox.GetSizeBytes() == 0 {
		return nil, phases, errors.New("workspace runtime prepare sandbox_artifact size_bytes is required")
	}
	phaseStarted := time.Now()
	image, cleanupImage, err := restorePreparedSandboxImage(conn, request)
	phases = append(phases, workspaceMountPhase("guest_sandbox_image_restore", phaseStarted, sandbox.GetSizeBytes(), 0, err))
	logger.Info("workspace runtime prepare sandbox image restored", "runtime_key_id", runtimeKeyLogID(runtimeKey), "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", sandbox.GetSizeBytes(), "error", errorString(err))
	if err != nil {
		return nil, phases, err
	}
	cleanup := cleanupImage
	phaseStarted = time.Now()
	runtimeUser, err := resolveRuntimeUser(image.RootfsDir, image.Config.User)
	phases = append(phases, workspaceMountPhase("guest_runtime_user_resolve", phaseStarted, 0, 0, err))
	if err != nil {
		cleanup()
		return nil, phases, fmt.Errorf("resolve prepared runtime user: %w", err)
	}
	phaseStarted = time.Now()
	workspaceRoot, err := workspaceRootForImage(image.RootfsDir, mountPath)
	phases = append(phases, workspaceMountPhase("guest_workspace_root_resolve", phaseStarted, 0, 0, err))
	if err != nil {
		cleanup()
		return nil, phases, fmt.Errorf("resolve prepared runtime workspace mount: %w", err)
	}
	return &preparedWorkspaceRuntime{
		key:            runtimeKey,
		sandboxDigest:  strings.TrimSpace(sandbox.GetDigest()),
		imageRoot:      image.RootfsDir,
		imageConfig:    image.Config,
		runtimeUser:    runtimeUser,
		workspaceMount: mountPath,
		workspaceRoot:  workspaceRoot,
		cleanup:        cleanup,
	}, phases, nil
}

func restoreWorkspaceMount(conn io.Reader, request *workspacev0.MaterializeWorkspaceRequest, logger *slog.Logger, registry *workspaceOperationRegistry) (*workspaceMountEntry, []*workspacev0.WorkspaceMountPhase, error) {
	entry := &workspaceMountEntry{}
	var phases []*workspacev0.WorkspaceMountPhase
	envelope := request.GetEnvelope()
	workspaceMountID := strings.TrimSpace(envelope.GetWorkspaceMountId())
	workspaceID := strings.TrimSpace(envelope.GetWorkspaceId())
	mountPath := filepath.Clean(strings.TrimSpace(request.GetMountPath()))
	if mountPath == "" || mountPath == "." || mountPath == string(filepath.Separator) || !filepath.IsAbs(mountPath) {
		return nil, phases, fmt.Errorf("workspace materialize mount_path %q is invalid", request.GetMountPath())
	}
	if strings.TrimSpace(request.GetBaseVersionId()) == "" {
		return nil, phases, errors.New("workspace materialize base_version_id is required")
	}
	artifact := request.GetBaseArtifact()
	if artifact == nil {
		return nil, phases, errors.New("workspace materialize base_artifact is required")
	}
	if strings.TrimSpace(artifact.GetDigest()) == "" {
		return nil, phases, errors.New("workspace materialize base_artifact digest is required")
	}
	if strings.TrimSpace(artifact.GetMediaType()) != workspace.ArtifactMediaType {
		return nil, phases, fmt.Errorf("workspace materialize base_artifact media_type %q is not supported", artifact.GetMediaType())
	}
	if strings.TrimSpace(artifact.GetEncoding()) != workspace.ArtifactEncoding {
		return nil, phases, fmt.Errorf("workspace materialize base_artifact encoding %q is not supported", artifact.GetEncoding())
	}
	if artifact.GetSizeBytes() == 0 {
		return nil, phases, errors.New("workspace materialize base_artifact size_bytes is required")
	}
	if artifact.GetSizeBytes() > uint64(workspace.MaxArtifactArchiveBytes) {
		return nil, phases, fmt.Errorf("workspace materialize base_artifact size_bytes %d exceeds max %d", artifact.GetSizeBytes(), workspace.MaxArtifactArchiveBytes)
	}
	if artifact.GetEntryCount() > uint32(workspace.MaxArtifactEntries) {
		return nil, phases, fmt.Errorf("workspace materialize base_artifact entry_count %d exceeds max %d", artifact.GetEntryCount(), workspace.MaxArtifactEntries)
	}
	sandbox := request.GetSandboxArtifact()
	if sandbox == nil {
		return nil, phases, errors.New("workspace materialize sandbox_artifact is required")
	}
	if strings.TrimSpace(sandbox.GetDigest()) == "" {
		return nil, phases, errors.New("workspace materialize sandbox_artifact digest is required")
	}
	if strings.TrimSpace(sandbox.GetMediaType()) != "application/vnd.helmr.sandbox-image.v0.oci-tar" {
		return nil, phases, fmt.Errorf("workspace materialize sandbox_artifact media_type %q is not supported", sandbox.GetMediaType())
	}
	if strings.TrimSpace(sandbox.GetEncoding()) != "oci-tar" {
		return nil, phases, fmt.Errorf("workspace materialize sandbox_artifact encoding %q is not supported", sandbox.GetEncoding())
	}
	if sandbox.GetSizeBytes() == 0 {
		return nil, phases, errors.New("workspace materialize sandbox_artifact size_bytes is required")
	}
	if request.GetUsePreparedRuntime() {
		phaseStarted := time.Now()
		prepared, ok := registry.takePreparedRuntime(request.GetRuntimeKey(), sandbox.GetDigest(), mountPath)
		var err error
		if !ok {
			err = errors.New("prepared workspace runtime is not available")
		}
		phases = append(phases, workspaceMountPhase("guest_prepared_runtime_checkout", phaseStarted, 0, 0, err))
		if err != nil {
			return nil, phases, err
		}
		entry.imageRoot = prepared.imageRoot
		entry.imageConfig = prepared.imageConfig
		entry.runtimeUser = prepared.runtimeUser
		entry.workspaceMount = prepared.workspaceMount
		entry.workspaceRoot = prepared.workspaceRoot
		entry.cleanup = func() {
			entry.stopWorkspaceProcesses()
			entry.closeEvents()
			prepared.cleanup()
		}
	} else {
		phaseStarted := time.Now()
		image, cleanupImage, err := restoreWorkspaceMountSandboxImage(conn, request)
		phases = append(phases, workspaceMountPhase("guest_sandbox_image_restore", phaseStarted, sandbox.GetSizeBytes(), 0, err))
		logger.Info("workspace materialize sandbox image restored", "workspace_id", workspaceID, "workspace_mount_id", workspaceMountID, "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", sandbox.GetSizeBytes(), "error", errorString(err))
		if err != nil {
			return nil, phases, err
		}
		entry.imageRoot = image.RootfsDir
		entry.imageConfig = image.Config
		entry.workspaceMount = mountPath
		entry.cleanup = func() {
			entry.stopWorkspaceProcesses()
			entry.closeEvents()
			cleanupImage()
		}
		phaseStarted = time.Now()
		runtimeUser, err := resolveRuntimeUser(entry.imageRoot, entry.imageConfig.User)
		phases = append(phases, workspaceMountPhase("guest_runtime_user_resolve", phaseStarted, 0, 0, err))
		logger.Info("workspace materialize runtime user resolved", "workspace_id", workspaceID, "workspace_mount_id", workspaceMountID, "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", errorString(err))
		if err != nil {
			entry.cleanup()
			return nil, phases, fmt.Errorf("resolve workspace runtime user: %w", err)
		}
		entry.runtimeUser = runtimeUser
		phaseStarted = time.Now()
		workspaceRoot, err := workspaceRootForImage(entry.imageRoot, mountPath)
		phases = append(phases, workspaceMountPhase("guest_workspace_root_resolve", phaseStarted, 0, 0, err))
		logger.Info("workspace materialize workspace root resolved", "workspace_id", workspaceID, "workspace_mount_id", workspaceMountID, "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", errorString(err))
		if err != nil {
			entry.cleanup()
			return nil, phases, fmt.Errorf("resolve workspace mount: %w", err)
		}
		entry.workspaceRoot = workspaceRoot
	}
	entry.processes = map[string]*workspaceProcess{}
	entry.events = make(chan *workspacev0.WorkspaceOperationEvent, 1024)
	entry.eventsDone = make(chan struct{})
	phaseStarted := time.Now()
	header, bodyLen, err := transport.ReadStreamFrameHeader(conn)
	if err != nil {
		entry.cleanup()
		phases = append(phases, workspaceMountPhase("guest_workspace_artifact_restore", phaseStarted, 0, 0, err))
		return nil, phases, fmt.Errorf("read workspace artifact stream header: %w", err)
	}
	if header.Type != transport.StreamTypeWorkspaceArtifact {
		drainStreamBody(conn, bodyLen)
		entry.cleanup()
		err := fmt.Errorf("unsupported workspace materialize input type %q", header.Type)
		phases = append(phases, workspaceMountPhase("guest_workspace_artifact_restore", phaseStarted, bodyLen, 0, err))
		return nil, phases, err
	}
	if header.WorkspaceID != strings.TrimSpace(request.GetEnvelope().GetWorkspaceId()) {
		drainStreamBody(conn, bodyLen)
		entry.cleanup()
		err := fmt.Errorf("workspace artifact workspace_id %q does not match materialize workspace_id %q", header.WorkspaceID, request.GetEnvelope().GetWorkspaceId())
		phases = append(phases, workspaceMountPhase("guest_workspace_artifact_restore", phaseStarted, bodyLen, 0, err))
		return nil, phases, err
	}
	frameDigest := ""
	if header.BodyDigest != nil {
		frameDigest = strings.TrimSpace(*header.BodyDigest)
	}
	if frameDigest != "" && frameDigest != strings.TrimSpace(artifact.GetDigest()) {
		drainStreamBody(conn, bodyLen)
		entry.cleanup()
		err := fmt.Errorf("workspace artifact digest %q does not match frame digest %q", artifact.GetDigest(), frameDigest)
		phases = append(phases, workspaceMountPhase("guest_workspace_artifact_restore", phaseStarted, bodyLen, 0, err))
		return nil, phases, err
	}
	if artifact.GetSizeBytes() != bodyLen {
		drainStreamBody(conn, bodyLen)
		entry.cleanup()
		err := fmt.Errorf("workspace artifact size_bytes %d does not match frame size %d", artifact.GetSizeBytes(), bodyLen)
		phases = append(phases, workspaceMountPhase("guest_workspace_artifact_restore", phaseStarted, bodyLen, 0, err))
		return nil, phases, err
	}
	workspaceParent := filepath.Dir(entry.workspaceRoot)
	if err := os.MkdirAll(workspaceParent, 0o755); err != nil {
		drainStreamBody(conn, bodyLen)
		entry.cleanup()
		phases = append(phases, workspaceMountPhase("guest_workspace_artifact_restore", phaseStarted, bodyLen, 0, err))
		return nil, phases, fmt.Errorf("create workspace mount parent: %w", err)
	}
	stagingRoot, err := os.MkdirTemp(workspaceParent, ".helmr-workspace-restore-*")
	if err != nil {
		drainStreamBody(conn, bodyLen)
		entry.cleanup()
		phases = append(phases, workspaceMountPhase("guest_workspace_artifact_restore", phaseStarted, bodyLen, 0, err))
		return nil, phases, fmt.Errorf("create workspace restore staging dir: %w", err)
	}
	cleanupStaging := func() { _ = os.RemoveAll(stagingRoot) }
	body := &io.LimitedReader{R: conn, N: int64(bodyLen)}
	hashedBody := newDigestingReader(body)
	stats, err := archive.ExtractTarWithStats(hashedBody, stagingRoot, archive.ExtractOptions{
		MaxBytes:   workspace.MaxArtifactExtractedBytes,
		MaxEntries: workspace.MaxArtifactEntries,
	})
	if err != nil {
		if _, drainErr := io.Copy(io.Discard, hashedBody); drainErr != nil {
			cleanupStaging()
			entry.cleanup()
			joined := errors.Join(fmt.Errorf("extract workspace artifact: %w", err), fmt.Errorf("drain workspace artifact: %w", drainErr))
			phases = append(phases, workspaceMountPhase("guest_workspace_artifact_restore", phaseStarted, bodyLen, 0, joined))
			return nil, phases, joined
		}
		cleanupStaging()
		entry.cleanup()
		wrapped := fmt.Errorf("extract workspace artifact: %w", err)
		phases = append(phases, workspaceMountPhase("guest_workspace_artifact_restore", phaseStarted, bodyLen, 0, wrapped))
		return nil, phases, wrapped
	}
	if _, err := io.Copy(io.Discard, hashedBody); err != nil {
		cleanupStaging()
		entry.cleanup()
		phases = append(phases, workspaceMountPhase("guest_workspace_artifact_restore", phaseStarted, bodyLen, uint32(stats.EntryCount), err))
		return nil, phases, fmt.Errorf("drain workspace artifact: %w", err)
	}
	if digest := hashedBody.Digest(); digest != strings.TrimSpace(artifact.GetDigest()) {
		cleanupStaging()
		entry.cleanup()
		err := fmt.Errorf("workspace artifact body digest %q does not match declared digest %q", digest, artifact.GetDigest())
		phases = append(phases, workspaceMountPhase("guest_workspace_artifact_restore", phaseStarted, bodyLen, uint32(stats.EntryCount), err))
		return nil, phases, err
	}
	if stats.EntryCount != int(artifact.GetEntryCount()) {
		cleanupStaging()
		entry.cleanup()
		err := fmt.Errorf("workspace artifact entry_count %d does not match declared entry_count %d", stats.EntryCount, artifact.GetEntryCount())
		phases = append(phases, workspaceMountPhase("guest_workspace_artifact_restore", phaseStarted, bodyLen, uint32(stats.EntryCount), err))
		return nil, phases, err
	}
	if err := os.RemoveAll(entry.workspaceRoot); err != nil {
		cleanupStaging()
		entry.cleanup()
		phases = append(phases, workspaceMountPhase("guest_workspace_artifact_restore", phaseStarted, bodyLen, uint32(stats.EntryCount), err))
		return nil, phases, fmt.Errorf("replace workspace mount: remove existing mount: %w", err)
	}
	if err := os.Rename(stagingRoot, entry.workspaceRoot); err != nil {
		cleanupStaging()
		entry.cleanup()
		phases = append(phases, workspaceMountPhase("guest_workspace_artifact_restore", phaseStarted, bodyLen, uint32(stats.EntryCount), err))
		return nil, phases, fmt.Errorf("replace workspace mount: %w", err)
	}
	logger.Info("workspace materialize workspace artifact restored", "workspace_id", workspaceID, "workspace_mount_id", workspaceMountID, "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", bodyLen, "entry_count", stats.EntryCount)
	phases = append(phases, workspaceMountPhase("guest_workspace_artifact_restore", phaseStarted, bodyLen, uint32(stats.EntryCount), nil))
	return entry, phases, nil
}

func workspaceMountPhase(name string, started time.Time, sizeBytes uint64, entryCount uint32, err error) *workspacev0.WorkspaceMountPhase {
	return &workspacev0.WorkspaceMountPhase{
		Name:       name,
		DurationMs: uint64(time.Since(started).Milliseconds()),
		SizeBytes:  sizeBytes,
		EntryCount: entryCount,
		Error:      errorString(err),
	}
}

func appendWorkspaceMountFailurePhase(phases []*workspacev0.WorkspaceMountPhase, name string, started time.Time, err error) []*workspacev0.WorkspaceMountPhase {
	if err == nil {
		return phases
	}
	for i := len(phases) - 1; i >= 0; i-- {
		if phases[i] != nil && strings.TrimSpace(phases[i].GetError()) != "" {
			return phases
		}
	}
	return append(phases, workspaceMountPhase(name, started, 0, 0, err))
}

func restoreWorkspaceMountSandboxImage(conn io.Reader, request *workspacev0.MaterializeWorkspaceRequest) (ociImage, func(), error) {
	cleanup := func() {}
	header, bodyLen, err := transport.ReadStreamFrameHeader(conn)
	if err != nil {
		return ociImage{}, cleanup, fmt.Errorf("read sandbox image stream header: %w", err)
	}
	if header.Type != transport.StreamTypeRunImage {
		drainStreamBody(conn, bodyLen)
		return ociImage{}, cleanup, fmt.Errorf("unsupported workspace materialize sandbox input type %q", header.Type)
	}
	if header.WorkspaceID != strings.TrimSpace(request.GetEnvelope().GetWorkspaceId()) {
		drainStreamBody(conn, bodyLen)
		return ociImage{}, cleanup, fmt.Errorf("sandbox image workspace_id %q does not match materialize workspace_id %q", header.WorkspaceID, request.GetEnvelope().GetWorkspaceId())
	}
	sandbox := request.GetSandboxArtifact()
	if sandbox.GetSizeBytes() != bodyLen {
		drainStreamBody(conn, bodyLen)
		return ociImage{}, cleanup, fmt.Errorf("sandbox image artifact size_bytes %d does not match frame size %d", sandbox.GetSizeBytes(), bodyLen)
	}
	frameDigest := ""
	if header.BodyDigest != nil {
		frameDigest = strings.TrimSpace(*header.BodyDigest)
	}
	if frameDigest != "" && frameDigest != strings.TrimSpace(sandbox.GetDigest()) {
		drainStreamBody(conn, bodyLen)
		return ociImage{}, cleanup, fmt.Errorf("sandbox image artifact digest %q does not match frame digest %q", sandbox.GetDigest(), frameDigest)
	}
	body := &io.LimitedReader{R: conn, N: int64(bodyLen)}
	hashedBody := newDigestingReader(body)
	var image ociImage
	if substrateRoot := guestdSubstrateRoot(); substrateRoot != "" {
		image, cleanup, err = imageFromMountedSubstrate(hashedBody, substrateRoot)
	} else {
		imageRoot, imageRootErr := mkdirGuestdTemp("helmr-workspace-image-*")
		if imageRootErr != nil {
			drainStreamBody(conn, bodyLen)
			return ociImage{}, cleanup, fmt.Errorf("create sandbox image root: %w", imageRootErr)
		}
		cleanup = func() { _ = os.RemoveAll(imageRoot) }
		image, err = unpackOCIImage(hashedBody, imageRoot)
	}
	if err != nil {
		if _, drainErr := io.Copy(io.Discard, hashedBody); drainErr != nil {
			cleanup()
			return ociImage{}, func() {}, errors.Join(fmt.Errorf("extract sandbox image artifact: %w", err), fmt.Errorf("drain sandbox image artifact: %w", drainErr))
		}
		cleanup()
		return ociImage{}, func() {}, fmt.Errorf("extract sandbox image artifact: %w", err)
	}
	if _, err := io.Copy(io.Discard, hashedBody); err != nil {
		cleanup()
		return ociImage{}, func() {}, fmt.Errorf("drain sandbox image artifact: %w", err)
	}
	if digest := hashedBody.Digest(); digest != strings.TrimSpace(sandbox.GetDigest()) {
		cleanup()
		return ociImage{}, func() {}, fmt.Errorf("sandbox image artifact body digest %q does not match declared digest %q", digest, sandbox.GetDigest())
	}
	return image, cleanup, nil
}

func restorePreparedSandboxImage(conn io.Reader, request *workspacev0.PrepareWorkspaceRuntimeRequest) (ociImage, func(), error) {
	cleanup := func() {}
	header, bodyLen, err := transport.ReadStreamFrameHeader(conn)
	if err != nil {
		return ociImage{}, cleanup, fmt.Errorf("read prepared sandbox image stream header: %w", err)
	}
	if header.Type != transport.StreamTypeRunImage {
		drainStreamBody(conn, bodyLen)
		return ociImage{}, cleanup, fmt.Errorf("unsupported workspace runtime prepare input type %q", header.Type)
	}
	sandbox := request.GetSandboxArtifact()
	if sandbox.GetSizeBytes() != bodyLen {
		drainStreamBody(conn, bodyLen)
		return ociImage{}, cleanup, fmt.Errorf("prepared sandbox image artifact size_bytes %d does not match frame size %d", sandbox.GetSizeBytes(), bodyLen)
	}
	frameDigest := ""
	if header.BodyDigest != nil {
		frameDigest = strings.TrimSpace(*header.BodyDigest)
	}
	if frameDigest != "" && frameDigest != strings.TrimSpace(sandbox.GetDigest()) {
		drainStreamBody(conn, bodyLen)
		return ociImage{}, cleanup, fmt.Errorf("prepared sandbox image artifact digest %q does not match frame digest %q", sandbox.GetDigest(), frameDigest)
	}
	body := &io.LimitedReader{R: conn, N: int64(bodyLen)}
	hashedBody := newDigestingReader(body)
	var image ociImage
	if substrateRoot := guestdSubstrateRoot(); substrateRoot != "" {
		image, cleanup, err = imageFromMountedSubstrate(hashedBody, substrateRoot)
	} else {
		imageRoot, imageRootErr := mkdirGuestdTemp("helmr-prepared-workspace-image-*")
		if imageRootErr != nil {
			drainStreamBody(conn, bodyLen)
			return ociImage{}, cleanup, fmt.Errorf("create prepared sandbox image root: %w", imageRootErr)
		}
		cleanup = func() { _ = os.RemoveAll(imageRoot) }
		image, err = unpackOCIImage(hashedBody, imageRoot)
	}
	if err != nil {
		if _, drainErr := io.Copy(io.Discard, hashedBody); drainErr != nil {
			cleanup()
			return ociImage{}, func() {}, errors.Join(fmt.Errorf("extract prepared sandbox image artifact: %w", err), fmt.Errorf("drain prepared sandbox image artifact: %w", drainErr))
		}
		cleanup()
		return ociImage{}, func() {}, fmt.Errorf("extract prepared sandbox image artifact: %w", err)
	}
	if _, err := io.Copy(io.Discard, hashedBody); err != nil {
		cleanup()
		return ociImage{}, func() {}, fmt.Errorf("drain prepared sandbox image artifact: %w", err)
	}
	if digest := hashedBody.Digest(); digest != strings.TrimSpace(sandbox.GetDigest()) {
		cleanup()
		return ociImage{}, func() {}, fmt.Errorf("prepared sandbox image artifact body digest %q does not match declared digest %q", digest, sandbox.GetDigest())
	}
	return image, cleanup, nil
}

func handleWorkspaceEventsConnection(ctx context.Context, conn io.ReadWriter, registry *workspaceOperationRegistry) error {
	var envelope workspacev0.WorkspaceOperationEnvelope
	if err := transport.ReadProtoFrame(conn, &envelope); err != nil {
		return fmt.Errorf("read workspace events envelope: %w", err)
	}
	if strings.TrimSpace(envelope.WorkspaceMountId) == "" {
		return errors.New("workspace events workspace_mount_id is required")
	}
	if strings.TrimSpace(envelope.WorkspaceId) == "" {
		return errors.New("workspace events workspace_id is required")
	}
	entry, release, ok := registry.acquire(envelope.WorkspaceMountId, envelope.WorkspaceId, envelope.ChannelToken, envelope.FencingGeneration)
	if !ok {
		return errors.New("workspace events channel token or fencing generation is invalid")
	}
	defer release()
	if entry.events == nil {
		return errors.New("workspace events channel is not available")
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-entry.eventsDone:
			return nil
		case event, ok := <-entry.events:
			if !ok {
				return nil
			}
			if event.Envelope == nil {
				event.Envelope = &workspacev0.WorkspaceOperationEnvelope{
					WorkspaceMountId:  envelope.WorkspaceMountId,
					WorkspaceId:       envelope.WorkspaceId,
					FencingGeneration: envelope.FencingGeneration,
				}
			}
			if err := transport.WriteProtoFrame(conn, event); err != nil {
				return fmt.Errorf("write workspace operation event: %w", err)
			}
		}
	}
}

func handleWorkspaceStopConnection(_ context.Context, conn io.ReadWriter, registry *workspaceOperationRegistry) error {
	if err := handleWorkspaceStop(conn, registry); err != nil {
		response := &workspacev0.StopWorkspaceResponse{
			State:     "failed",
			ErrorJson: workspaceOperationErrorJSON(err),
		}
		if writeErr := transport.WriteProtoFrame(conn, response); writeErr != nil {
			return errors.Join(err, fmt.Errorf("write workspace stop failure: %w", writeErr))
		}
		return nil
	}
	return nil
}

func handleWorkspaceStop(conn io.ReadWriter, registry *workspaceOperationRegistry) error {
	var request workspacev0.StopWorkspaceRequest
	if err := transport.ReadProtoFrame(conn, &request); err != nil {
		return fmt.Errorf("read workspace stop request: %w", err)
	}
	envelope := request.GetEnvelope()
	if envelope == nil {
		return errors.New("workspace stop envelope is required")
	}
	if strings.TrimSpace(envelope.WorkspaceMountId) == "" {
		return errors.New("workspace stop workspace_mount_id is required")
	}
	if strings.TrimSpace(envelope.WorkspaceId) == "" {
		return errors.New("workspace stop workspace_id is required")
	}
	entry, release, ok := registry.acquire(envelope.WorkspaceMountId, envelope.WorkspaceId, envelope.ChannelToken, envelope.FencingGeneration)
	if !ok {
		return errors.New("workspace stop channel token or fencing generation is invalid")
	}
	defer release()
	entry.stopWorkspaceProcesses()
	finalize := request.GetFinalizeStop() || !request.GetCaptureBeforeStop()
	response := &workspacev0.StopWorkspaceResponse{State: "stopped"}
	var artifact workspace.WorkspaceArtifact
	var cleanupArtifact func()
	if request.GetCaptureBeforeStop() {
		if finalize {
			return errors.New("workspace stop capture and finalize must be separate requests")
		}
		tempDir, err := mkdirGuestdTemp("helmr-workspace-stop-*")
		if err != nil {
			return fmt.Errorf("create workspace stop temp dir: %w", err)
		}
		defer os.RemoveAll(tempDir)
		artifact, cleanupArtifact, err = workspace.CreateWorkspaceArtifactFromRoot(entry.workspaceRoot, tempDir, filepath.Dir(entry.workspaceRoot))
		if err != nil {
			return fmt.Errorf("capture workspace stop artifact: %w", err)
		}
		defer cleanupArtifact()
		response.CapturedArtifact = &workspacev0.WorkspaceArtifact{
			Digest:     artifact.Digest,
			MediaType:  artifact.MediaType,
			Encoding:   artifact.Encoding,
			SizeBytes:  uint64(artifact.SizeBytes),
			EntryCount: uint32(artifact.EntryCount),
		}
		response.State = "captured"
	}
	if err := transport.WriteProtoFrame(conn, response); err != nil {
		return fmt.Errorf("write workspace stop response: %w", err)
	}
	if request.GetCaptureBeforeStop() {
		entryCount := artifact.EntryCount
		if err := transport.WriteFileFrameWithMetadata(conn, transport.StreamHeader{
			Type:        transport.StreamTypeWorkspaceArtifact,
			WorkspaceID: envelope.WorkspaceId,
			EntryCount:  &entryCount,
		}, artifact.Path, artifact.Digest, artifact.SizeBytes); err != nil {
			return fmt.Errorf("write workspace stop artifact: %w", err)
		}
	}
	if finalize {
		registry.retire(envelope.WorkspaceMountId, entry)
	}
	return nil
}

func handleWorkspaceOperationConnection(_ context.Context, conn io.ReadWriter, registry *workspaceOperationRegistry) error {
	var request workspacev0.WorkspaceOperationRequest
	if err := transport.ReadProtoFrame(conn, &request); err != nil {
		return fmt.Errorf("read workspace operation request: %w", err)
	}
	envelope := request.GetEnvelope()
	if envelope == nil {
		return writeWorkspaceOperationResult(conn, errors.New("workspace operation envelope is required"))
	}
	if strings.TrimSpace(envelope.OperationId) == "" {
		return writeWorkspaceOperationResult(conn, errors.New("workspace operation operation_id is required"))
	}
	if strings.TrimSpace(envelope.WorkspaceMountId) == "" {
		return writeWorkspaceOperationResult(conn, errors.New("workspace operation workspace_mount_id is required"))
	}
	if strings.TrimSpace(envelope.WorkspaceId) == "" {
		return writeWorkspaceOperationResult(conn, errors.New("workspace operation workspace_id is required"))
	}
	entry, release, ok := registry.acquire(envelope.WorkspaceMountId, envelope.WorkspaceId, envelope.ChannelToken, envelope.FencingGeneration)
	if !ok {
		return writeWorkspaceOperationResult(conn, errors.New("workspace operation channel token or fencing generation is invalid"))
	}
	defer release()
	_ = entry
	if envelope.OperationExpiresAtUnixNano <= 0 {
		return writeWorkspaceOperationResult(conn, errors.New("workspace operation operation_expires_at is required"))
	}
	if time.Now().UnixNano() >= envelope.OperationExpiresAtUnixNano {
		return writeWorkspaceOperationResult(conn, errors.New("workspace operation expired"))
	}
	fingerprint := strings.TrimSpace(envelope.RequestFingerprint)
	if fingerprint == "" {
		return writeWorkspaceOperationResult(conn, errors.New("workspace operation request_fingerprint is required"))
	}
	actual, err := protocol.RequestFingerprint(request.OperationKind, []byte(request.RequestJson))
	if err != nil {
		return writeWorkspaceOperationResult(conn, err)
	}
	if actual != fingerprint {
		return writeWorkspaceOperationResult(conn, fmt.Errorf("workspace operation request_fingerprint %q does not match request %q", fingerprint, actual))
	}
	switch strings.TrimSpace(request.OperationKind) {
	case protocol.GuestVerbStartExec:
		return writeWorkspaceOperationResult(conn, entry.startWorkspaceExec(request.GetEnvelope(), request.RequestJson))
	case protocol.GuestVerbCreatePty:
		return writeWorkspaceOperationResult(conn, entry.createWorkspacePty(request.GetEnvelope(), request.RequestJson))
	case protocol.GuestVerbResizePty:
		return writeWorkspaceOperationResult(conn, entry.resizeWorkspacePty(request.RequestJson))
	case protocol.GuestVerbClosePty:
		return writeWorkspaceOperationResult(conn, entry.closeWorkspacePty(request.RequestJson))
	default:
		return transport.WriteProtoFrame(conn, &workspacev0.WorkspaceOperationResult{
			ErrorJson: fmt.Sprintf(`{"message":"unsupported workspace operation %q"}`, strings.TrimSpace(request.OperationKind)),
		})
	}
}

func writeWorkspaceOperationResult(conn io.Writer, err error) error {
	result := &workspacev0.WorkspaceOperationResult{ResultJson: `{"ok":true}`}
	if err != nil {
		result.ResultJson = ""
		result.ErrorJson = workspaceOperationErrorJSON(err)
	}
	return transport.WriteProtoFrame(conn, result)
}
