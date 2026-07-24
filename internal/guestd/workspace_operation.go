package guestd

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/frameio"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

const (
	workspaceImageMediaType = "application/vnd.helmr.workspace-image.v0.oci-tar"
	workspaceImageEncoding  = "oci-tar"
)

type workspaceOperationRegistry struct {
	mu              sync.RWMutex
	entries         map[string]*workspaceMountEntry
	preparedRuntime *preparedWorkspaceRuntime
	programActive   bool
	programEntry    *workspaceMountEntry
	programReleased chan struct{}
}

type workspaceAuthorityState uint8

const (
	workspaceAuthorityLive workspaceAuthorityState = iota
	workspaceAuthorityFinalizing
)

type workspaceMountEntry struct {
	channelToken      string
	workspaceID       string
	workspaceMountID  string
	baseVersionID     string
	fencingMu         sync.RWMutex
	fencingGeneration uint64
	runtimeInstanceID string
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
	basicExecMu       sync.Mutex
	basicExecs        map[string]*workspaceBasicExec
	basicExecRun      func(*workspacev0.WorkspaceOperationRequest) *workspacev0.WorkspaceOperationResult
	active            int
	retired           bool
	authorityMu       sync.Mutex
	authority         *workspacev0.WorkspaceRunAuthority
	previousExpiry    int64
	finalizationMu    sync.Mutex
	finalizationRoot  string
	authorityState    workspaceAuthorityState
	finalizationID    string
	finalizationKind  string
	recoveryRequired  bool
	processAdmissions int
	turnCommitBlocked bool
}

type preparedWorkspaceRuntime struct {
	runtimeInstanceID    string
	workspaceImageDigest string
	imageRoot            string
	imageConfig          ociRuntimeConfig
	runtimeUser          *resolvedRuntimeUser
	workspaceMount       string
	workspaceRoot        string
	cleanup              func()
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

func (r *workspaceOperationRegistry) takePreparedRuntime(runtimeInstanceID string, workspaceImageDigest string, workspaceMount string) (*preparedWorkspaceRuntime, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prepared := r.preparedRuntime
	if prepared == nil {
		return nil, false
	}
	if prepared.runtimeInstanceID != runtimeInstanceID ||
		strings.TrimSpace(prepared.workspaceImageDigest) != strings.TrimSpace(workspaceImageDigest) ||
		strings.TrimSpace(prepared.workspaceMount) != strings.TrimSpace(workspaceMount) {
		return nil, false
	}
	r.preparedRuntime = nil
	return prepared, true
}

func (r *workspaceOperationRegistry) register(workspaceMountID string, entry *workspaceMountEntry) {
	for {
		r.mu.Lock()
		previous := r.entries[workspaceMountID]
		if previous == entry {
			entry.workspaceMountID = workspaceMountID
			r.mu.Unlock()
			return
		}
		if previous == nil {
			entry.workspaceMountID = workspaceMountID
			r.entries[workspaceMountID] = entry
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()

		previous.finalizationMu.Lock()
		r.mu.Lock()
		if r.entries[workspaceMountID] != previous {
			r.mu.Unlock()
			previous.finalizationMu.Unlock()
			continue
		}
		entry.workspaceMountID = workspaceMountID
		r.entries[workspaceMountID] = entry
		previous.retired = true
		var cleanup func()
		if previous.active == 0 {
			cleanup = previous.cleanup
			previous.cleanup = nil
		}
		r.mu.Unlock()
		previous.finalizationMu.Unlock()
		if cleanup != nil {
			cleanup()
		}
		return
	}
}

func (r *workspaceOperationRegistry) acquire(workspaceMountID string, workspaceID string, token string, fencingGeneration uint64) (*workspaceMountEntry, func(), bool) {
	workspaceID = strings.TrimSpace(workspaceID)
	token = strings.TrimSpace(token)
	if workspaceID == "" || token == "" || fencingGeneration == 0 {
		return nil, func() {}, false
	}
	for {
		r.mu.Lock()
		entry := r.entries[workspaceMountID]
		if !workspaceEntryMatches(entry, workspaceMountID, workspaceID, token) {
			r.mu.Unlock()
			return nil, func() {}, false
		}
		entry.processesMu.Lock()
		recoveryRequired := entry.recoveryRequired
		entry.processesMu.Unlock()
		currentGeneration := entry.currentFencingGeneration()
		if recoveryRequired || fencingGeneration < currentGeneration {
			r.mu.Unlock()
			return nil, func() {}, false
		}
		if fencingGeneration == currentGeneration {
			entry.active++
			r.mu.Unlock()
			return entry, func() { r.release(entry) }, true
		}
		r.mu.Unlock()

		entry.finalizationMu.Lock()
		r.mu.Lock()
		if r.entries[workspaceMountID] != entry || !workspaceEntryMatches(entry, workspaceMountID, workspaceID, token) {
			r.mu.Unlock()
			entry.finalizationMu.Unlock()
			continue
		}
		if fencingGeneration < entry.currentFencingGeneration() {
			r.mu.Unlock()
			entry.finalizationMu.Unlock()
			return nil, func() {}, false
		}
		entry.processesMu.Lock()
		finalizing := entry.authorityState == workspaceAuthorityFinalizing || entry.recoveryRequired
		entry.processesMu.Unlock()
		if finalizing || r.programActive && r.programEntry == entry {
			r.mu.Unlock()
			entry.finalizationMu.Unlock()
			return nil, func() {}, false
		}
		entry.setFencingGeneration(fencingGeneration)
		entry.active++
		r.mu.Unlock()
		entry.finalizationMu.Unlock()
		return entry, func() { r.release(entry) }, true
	}
}

func (r *workspaceOperationRegistry) acquireAuthorityMount(workspaceMountID string, workspaceID string, token string) (*workspaceMountEntry, func(), bool) {
	r.mu.Lock()
	entry := r.entries[workspaceMountID]
	workspaceID = strings.TrimSpace(workspaceID)
	token = strings.TrimSpace(token)
	if workspaceID == "" || token == "" || !workspaceEntryMatches(entry, workspaceMountID, workspaceID, token) {
		r.mu.Unlock()
		return nil, func() {}, false
	}
	entry.processesMu.Lock()
	recoveryRequired := entry.recoveryRequired
	entry.processesMu.Unlock()
	if recoveryRequired {
		r.mu.Unlock()
		return nil, func() {}, false
	}
	entry.active++
	r.mu.Unlock()
	return entry, func() { r.release(entry) }, true
}

func (r *workspaceOperationRegistry) acquireExact(workspaceMountID string, workspaceID string, token string, fencingGeneration uint64) (*workspaceMountEntry, func(), bool) {
	r.mu.Lock()
	entry, ok := r.entries[workspaceMountID]
	workspaceID = strings.TrimSpace(workspaceID)
	token = strings.TrimSpace(token)
	if !(ok &&
		workspaceID != "" &&
		token != "" &&
		fencingGeneration != 0 &&
		entry.workspaceMountID == workspaceMountID &&
		entry.workspaceID == workspaceID &&
		entry.currentFencingGeneration() == fencingGeneration &&
		!entry.retired &&
		subtle.ConstantTimeCompare([]byte(entry.channelToken), []byte(token)) == 1) {
		r.mu.Unlock()
		return nil, func() {}, false
	}
	entry.active++
	r.mu.Unlock()
	return entry, func() { r.release(entry) }, true
}

func workspaceEntryMatches(entry *workspaceMountEntry, workspaceMountID string, workspaceID string, token string) bool {
	return entry != nil &&
		entry.workspaceMountID == workspaceMountID &&
		entry.workspaceID == workspaceID &&
		!entry.retired &&
		subtle.ConstantTimeCompare([]byte(entry.channelToken), []byte(token)) == 1
}

func (r *workspaceOperationRegistry) currentExactLocked(entry *workspaceMountEntry, workspaceMountID string, workspaceID string, token string, fencingGeneration uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.entries[workspaceMountID] == entry &&
		workspaceEntryMatches(entry, workspaceMountID, workspaceID, token) &&
		entry.currentFencingGeneration() == fencingGeneration
}

func (entry *workspaceMountEntry) currentFencingGeneration() uint64 {
	entry.fencingMu.RLock()
	defer entry.fencingMu.RUnlock()
	return entry.fencingGeneration
}

func (entry *workspaceMountEntry) setFencingGeneration(generation uint64) {
	entry.fencingMu.Lock()
	entry.fencingGeneration = generation
	entry.fencingMu.Unlock()
}

func (r *workspaceOperationRegistry) currentMountLocked(entry *workspaceMountEntry, workspaceMountID string, workspaceID string, token string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.entries[workspaceMountID] == entry && workspaceEntryMatches(entry, workspaceMountID, workspaceID, token)
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
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
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

func (r *workspaceOperationRegistry) admitProgram(entry *workspaceMountEntry, authority *workspacev0.WorkspaceRunAuthority, now time.Time) (func(), error) {
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	entry.processesMu.Lock()
	recoveryRequired := entry.recoveryRequired
	entry.processesMu.Unlock()
	if recoveryRequired {
		return func() {}, errors.New("Workspace Mount requires recovery")
	}
	if authority == nil || authority.GetFence() == nil || !r.currentMountLocked(
		entry,
		authority.GetFence().GetWorkspaceMountId(),
		authority.GetFence().GetWorkspaceId(),
		authority.GetChannelToken(),
	) {
		return func() {}, errors.New("Program authority is not current for the Workspace Mount")
	}
	release, err := r.claimProgramLocked(entry)
	if err != nil {
		return func() {}, err
	}
	if err := entry.installWorkspaceRunAuthorityLocked(authority, now); err != nil {
		release()
		return func() {}, err
	}
	return release, nil
}

func (r *workspaceOperationRegistry) admitMountedProgram(entry *workspaceMountEntry) (func(), error) {
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	entry.processesMu.Lock()
	unavailable := entry.authorityState == workspaceAuthorityFinalizing || entry.recoveryRequired
	entry.processesMu.Unlock()
	if unavailable || !r.currentExactLocked(
		entry,
		entry.workspaceMountID,
		entry.workspaceID,
		entry.channelToken,
		entry.currentFencingGeneration(),
	) {
		return func() {}, errors.New("Workspace is unavailable for Program admission")
	}
	return r.claimProgramLocked(entry)
}

func (r *workspaceOperationRegistry) claimProgramLocked(entry *workspaceMountEntry) (func(), error) {
	r.mu.Lock()
	if r.programActive {
		r.mu.Unlock()
		return func() {}, errors.New("Workspace already has an active managed Program")
	}
	r.programActive = true
	r.programEntry = entry
	r.programReleased = make(chan struct{})
	released := r.programReleased
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if r.programActive && r.programEntry == entry && r.programReleased == released {
			r.programActive = false
			r.programEntry = nil
			close(released)
		}
		r.mu.Unlock()
	}, nil
}

func (r *workspaceOperationRegistry) waitForProgramRelease(ctx context.Context, entry *workspaceMountEntry) error {
	r.mu.Lock()
	if !r.programActive || r.programEntry != entry {
		r.mu.Unlock()
		return nil
	}
	released := r.programReleased
	r.mu.Unlock()
	select {
	case <-released:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func handleWorkspaceMaterializeConnection(_ context.Context, conn io.ReadWriter, logger *slog.Logger, registry *workspaceOperationRegistry, waits *waitingRunRegistry) error {
	if logger == nil {
		logger = slog.Default()
	}
	totalStarted := time.Now()
	var request workspacev0.MaterializeWorkspaceRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
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
	if strings.TrimSpace(request.GetRestoredCheckpointId()) != "" {
		phases, err := registry.rebindRestoredWorkspaceMount(&request, waits)
		if err != nil {
			phases = appendWorkspaceMountFailurePhase(phases, "guest_restore_rebind", totalStarted, err)
			writeErr := frameio.WriteProtoFrame(conn, &workspacev0.MaterializeWorkspaceResponse{State: "failed", Phases: phases})
			if writeErr != nil {
				return errors.Join(err, writeErr)
			}
			return err
		}
		logger.Info("restored workspace mount rebound", "workspace_id", workspaceID,
			"workspace_mount_id", workspaceMountID, "checkpoint_id", request.GetRestoredCheckpointId(),
			"duration_ms", time.Since(totalStarted).Milliseconds())
		return frameio.WriteProtoFrame(conn, &workspacev0.MaterializeWorkspaceResponse{
			State: "running", GuestdChannelTokenHash: sha256sum.HexBytes([]byte(strings.TrimSpace(envelope.ChannelToken))),
			Phases: phases,
		})
	}
	entry, phases, err := restoreWorkspaceMount(conn, &request, logger, registry)
	if err != nil {
		phases = appendWorkspaceMountFailurePhase(phases, "guest_materialize", totalStarted, err)
		writeErr := frameio.WriteProtoFrame(conn, &workspacev0.MaterializeWorkspaceResponse{
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
	entry.setFencingGeneration(envelope.FencingGeneration)
	registerStarted := time.Now()
	registry.register(envelope.WorkspaceMountId, entry)
	phases = append(phases, workspaceMountPhase("guest_register", registerStarted, 0, 0, nil))
	logger.Info("workspace materialize registered", "workspace_id", workspaceID, "workspace_mount_id", workspaceMountID, "duration_ms", time.Since(totalStarted).Milliseconds())
	return frameio.WriteProtoFrame(conn, &workspacev0.MaterializeWorkspaceResponse{
		State:                  "running",
		GuestdChannelTokenHash: sha256sum.HexBytes([]byte(strings.TrimSpace(envelope.ChannelToken))),
		Phases:                 phases,
	})
}

func (r *workspaceOperationRegistry) rebindRestoredWorkspaceMount(
	request *workspacev0.MaterializeWorkspaceRequest,
	waits *waitingRunRegistry,
) ([]*workspacev0.WorkspaceMountPhase, error) {
	started := time.Now()
	if request == nil || request.GetEnvelope() == nil || !request.GetUsePreparedRuntime() {
		return nil, errors.New("restored Workspace rebind requires a prepared runtime")
	}
	checkpointID := strings.TrimSpace(request.GetRestoredCheckpointId())
	if waits == nil || !waits.hasFrozenProgramCheckpoint(checkpointID) {
		return nil, errors.New("restored Workspace rebind did not match a frozen Program Checkpoint")
	}
	envelope := request.GetEnvelope()
	newMountID := strings.TrimSpace(envelope.GetWorkspaceMountId())
	workspaceID := strings.TrimSpace(envelope.GetWorkspaceId())
	channelToken := strings.TrimSpace(envelope.GetChannelToken())
	runtimeInstanceID := strings.TrimSpace(request.GetRuntimeInstanceId())
	baseVersionID := strings.TrimSpace(request.GetBaseVersionId())
	mountPath := filepath.Clean(strings.TrimSpace(request.GetMountPath()))
	if newMountID == "" || workspaceID == "" || channelToken == "" || runtimeInstanceID == "" ||
		baseVersionID == "" || envelope.GetFencingGeneration() == 0 || mountPath == "." ||
		mountPath == string(filepath.Separator) || !filepath.IsAbs(mountPath) {
		return nil, errors.New("restored Workspace rebind authority is incomplete")
	}
	r.mu.Lock()
	entry := r.programEntry
	r.mu.Unlock()
	if entry == nil {
		return nil, errors.New("restored Workspace has no active frozen Program")
	}
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	entry.processesMu.Lock()
	unavailable := entry.authorityState == workspaceAuthorityFinalizing || entry.recoveryRequired
	entry.processesMu.Unlock()
	if !r.programActive || r.programEntry != entry || unavailable || entry.workspaceID != workspaceID ||
		filepath.Clean(entry.workspaceMount) != mountPath {
		return nil, errors.New("restored Workspace rebind did not match the frozen mounted runtime")
	}
	currentGeneration := entry.currentFencingGeneration()
	if envelope.GetFencingGeneration() == currentGeneration && entry.workspaceMountID == newMountID &&
		entry.channelToken == channelToken && entry.runtimeInstanceID == runtimeInstanceID &&
		entry.baseVersionID == baseVersionID && r.entries[newMountID] == entry {
		return []*workspacev0.WorkspaceMountPhase{
			workspaceMountPhase("guest_restore_rebind_replay", started, 0, 0, nil),
		}, nil
	}
	if envelope.GetFencingGeneration() <= currentGeneration {
		return nil, errors.New("restored Workspace rebind fencing generation did not advance")
	}
	if current := r.entries[newMountID]; current != nil && current != entry {
		return nil, errors.New("restored Workspace rebind target Mount is already registered")
	}
	for id, current := range r.entries {
		if current == entry {
			delete(r.entries, id)
		}
	}
	entry.authorityMu.Lock()
	entry.authority = nil
	entry.previousExpiry = 0
	entry.authorityMu.Unlock()
	entry.workspaceMountID = newMountID
	entry.channelToken = channelToken
	entry.runtimeInstanceID = runtimeInstanceID
	entry.baseVersionID = baseVersionID
	entry.setFencingGeneration(envelope.GetFencingGeneration())
	r.entries[newMountID] = entry
	return []*workspacev0.WorkspaceMountPhase{
		workspaceMountPhase("guest_restore_rebind", started, 0, 0, nil),
	}, nil
}

func handleWorkspaceRuntimePrepareConnection(_ context.Context, conn io.ReadWriter, logger *slog.Logger, registry *workspaceOperationRegistry) error {
	if logger == nil {
		logger = slog.Default()
	}
	totalStarted := time.Now()
	var request workspacev0.PrepareWorkspaceRuntimeRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
		return fmt.Errorf("read workspace runtime prepare request: %w", err)
	}
	runtime, phases, err := restorePreparedWorkspaceRuntime(conn, &request, logger)
	if err != nil {
		phases = appendWorkspaceMountFailurePhase(phases, "guest_runtime_prepare", totalStarted, err)
		writeErr := frameio.WriteProtoFrame(conn, &workspacev0.PrepareWorkspaceRuntimeResponse{
			State:             "failed",
			RuntimeInstanceId: request.GetRuntimeInstanceId(),
			Phases:            phases,
		})
		if writeErr != nil {
			return errors.Join(fmt.Errorf("restore prepared workspace runtime: %w", err), fmt.Errorf("write workspace runtime prepare failure response: %w", writeErr))
		}
		return fmt.Errorf("restore prepared workspace runtime: %w", err)
	}
	registry.setPreparedRuntime(runtime)
	logger.Info("workspace runtime prepared", "runtime_instance_id_hash", runtimeInstanceLogID(request.GetRuntimeInstanceId()))
	return frameio.WriteProtoFrame(conn, &workspacev0.PrepareWorkspaceRuntimeResponse{
		State:             "prepared",
		RuntimeInstanceId: request.GetRuntimeInstanceId(),
		Phases:            phases,
	})
}

func runtimeInstanceLogID(runtimeInstanceID string) string {
	hash := sha256sum.HexBytes([]byte(runtimeInstanceID))
	if len(hash) < 16 {
		return hash
	}
	return hash[:16]
}

func restorePreparedWorkspaceRuntime(conn io.Reader, request *workspacev0.PrepareWorkspaceRuntimeRequest, logger *slog.Logger) (*preparedWorkspaceRuntime, []*workspacev0.WorkspaceMountPhase, error) {
	var phases []*workspacev0.WorkspaceMountPhase
	runtimeInstanceID := request.GetRuntimeInstanceId()
	if strings.TrimSpace(runtimeInstanceID) == "" {
		return nil, phases, errors.New("workspace runtime prepare runtime_instance_id is required")
	}
	mountPath := filepath.Clean(strings.TrimSpace(request.GetMountPath()))
	if mountPath == "" || mountPath == "." || mountPath == string(filepath.Separator) || !filepath.IsAbs(mountPath) {
		return nil, phases, fmt.Errorf("workspace runtime prepare mount_path %q is invalid", request.GetMountPath())
	}
	workspaceImage := request.GetWorkspaceImage()
	if workspaceImage == nil {
		return nil, phases, errors.New("workspace runtime prepare workspace_image is required")
	}
	if strings.TrimSpace(workspaceImage.GetDigest()) == "" {
		return nil, phases, errors.New("workspace runtime prepare workspace_image digest is required")
	}
	if workspaceImage.GetMediaType() != workspaceImageMediaType {
		return nil, phases, fmt.Errorf("workspace runtime prepare workspace_image media_type %q is not supported", workspaceImage.GetMediaType())
	}
	if workspaceImage.GetEncoding() != workspaceImageEncoding {
		return nil, phases, fmt.Errorf("workspace runtime prepare workspace_image encoding %q is not supported", workspaceImage.GetEncoding())
	}
	if workspaceImage.GetSizeBytes() == 0 {
		return nil, phases, errors.New("workspace runtime prepare workspace_image size_bytes is required")
	}
	phaseStarted := time.Now()
	image, cleanupImage, err := restorePreparedWorkspaceImage(conn, request)
	phases = append(phases, workspaceMountPhase("guest_workspace_image_restore", phaseStarted, workspaceImage.GetSizeBytes(), 0, err))
	logger.Info("workspace runtime prepare workspace image restored", "runtime_instance_id_hash", runtimeInstanceLogID(runtimeInstanceID), "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", workspaceImage.GetSizeBytes(), "error", errorText(err))
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
		runtimeInstanceID:    runtimeInstanceID,
		workspaceImageDigest: strings.TrimSpace(workspaceImage.GetDigest()),
		imageRoot:            image.RootfsDir,
		imageConfig:          image.Config,
		runtimeUser:          runtimeUser,
		workspaceMount:       mountPath,
		workspaceRoot:        workspaceRoot,
		cleanup:              cleanup,
	}, phases, nil
}

func restoreWorkspaceMount(conn io.Reader, request *workspacev0.MaterializeWorkspaceRequest, logger *slog.Logger, registry *workspaceOperationRegistry) (*workspaceMountEntry, []*workspacev0.WorkspaceMountPhase, error) {
	entry := &workspaceMountEntry{}
	var phases []*workspacev0.WorkspaceMountPhase
	envelope := request.GetEnvelope()
	workspaceMountID := strings.TrimSpace(envelope.GetWorkspaceMountId())
	workspaceID := strings.TrimSpace(envelope.GetWorkspaceId())
	runtimeInstanceID := strings.TrimSpace(request.GetRuntimeInstanceId())
	if runtimeInstanceID == "" {
		return nil, phases, errors.New("workspace materialize runtime_instance_id is required")
	}
	entry.runtimeInstanceID = runtimeInstanceID
	entry.workspaceMountID = workspaceMountID
	mountPath := filepath.Clean(strings.TrimSpace(request.GetMountPath()))
	if mountPath == "" || mountPath == "." || mountPath == string(filepath.Separator) || !filepath.IsAbs(mountPath) {
		return nil, phases, fmt.Errorf("workspace materialize mount_path %q is invalid", request.GetMountPath())
	}
	if strings.TrimSpace(request.GetBaseVersionId()) == "" {
		return nil, phases, errors.New("workspace materialize base_version_id is required")
	}
	entry.baseVersionID = strings.TrimSpace(request.GetBaseVersionId())
	artifact := request.GetBaseArtifact()
	if artifact != nil {
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
	}
	workspaceImage := request.GetWorkspaceImage()
	if workspaceImage == nil {
		return nil, phases, errors.New("workspace materialize workspace_image is required")
	}
	if strings.TrimSpace(workspaceImage.GetDigest()) == "" {
		return nil, phases, errors.New("workspace materialize workspace_image digest is required")
	}
	if workspaceImage.GetMediaType() != workspaceImageMediaType {
		return nil, phases, fmt.Errorf("workspace materialize workspace_image media_type %q is not supported", workspaceImage.GetMediaType())
	}
	if workspaceImage.GetEncoding() != workspaceImageEncoding {
		return nil, phases, fmt.Errorf("workspace materialize workspace_image encoding %q is not supported", workspaceImage.GetEncoding())
	}
	if workspaceImage.GetSizeBytes() == 0 {
		return nil, phases, errors.New("workspace materialize workspace_image size_bytes is required")
	}
	if request.GetUsePreparedRuntime() {
		phaseStarted := time.Now()
		prepared, ok := registry.takePreparedRuntime(request.GetRuntimeInstanceId(), workspaceImage.GetDigest(), mountPath)
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
		image, cleanupImage, err := restoreWorkspaceMountWorkspaceImage(conn, request)
		phases = append(phases, workspaceMountPhase("guest_workspace_image_restore", phaseStarted, workspaceImage.GetSizeBytes(), 0, err))
		logger.Info("workspace materialize workspace image restored", "workspace_id", workspaceID, "workspace_mount_id", workspaceMountID, "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", workspaceImage.GetSizeBytes(), "error", errorText(err))
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
		logger.Info("workspace materialize runtime user resolved", "workspace_id", workspaceID, "workspace_mount_id", workspaceMountID, "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", errorText(err))
		if err != nil {
			entry.cleanup()
			return nil, phases, fmt.Errorf("resolve workspace runtime user: %w", err)
		}
		entry.runtimeUser = runtimeUser
		phaseStarted = time.Now()
		workspaceRoot, err := workspaceRootForImage(entry.imageRoot, mountPath)
		phases = append(phases, workspaceMountPhase("guest_workspace_root_resolve", phaseStarted, 0, 0, err))
		logger.Info("workspace materialize workspace root resolved", "workspace_id", workspaceID, "workspace_mount_id", workspaceMountID, "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", errorText(err))
		if err != nil {
			entry.cleanup()
			return nil, phases, fmt.Errorf("resolve workspace mount: %w", err)
		}
		entry.workspaceRoot = workspaceRoot
	}
	entry.processes = map[string]*workspaceProcess{}
	entry.events = make(chan *workspacev0.WorkspaceOperationEvent, 1024)
	entry.eventsDone = make(chan struct{})
	finalizationRoot, err := os.MkdirTemp(filepath.Dir(entry.imageRoot), ".helmr-workspace-state-*")
	if err != nil {
		entry.cleanup()
		return nil, phases, fmt.Errorf("create Workspace finalization state: %w", err)
	}
	entry.finalizationRoot = finalizationRoot
	cleanupMount := entry.cleanup
	entry.cleanup = func() {
		cleanupMount()
		_ = os.RemoveAll(finalizationRoot)
	}
	if artifact == nil {
		phaseStarted := time.Now()
		err := initializeEmptyWorkspaceRoot(entry.workspaceRoot)
		phases = append(phases, workspaceMountPhase("guest_workspace_empty_root_init", phaseStarted, 0, 0, err))
		logger.Info("workspace materialize empty workspace root initialized", "workspace_id", workspaceID, "workspace_mount_id", workspaceMountID, "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", errorText(err))
		if err != nil {
			entry.cleanup()
			return nil, phases, err
		}
		return entry, phases, nil
	}
	phaseStarted := time.Now()
	header, bodyLen, err := wire.ReadStreamFrameHeader(conn)
	if err != nil {
		entry.cleanup()
		phases = append(phases, workspaceMountPhase("guest_workspace_artifact_restore", phaseStarted, 0, 0, err))
		return nil, phases, fmt.Errorf("read workspace artifact stream header: %w", err)
	}
	if header.Type != wire.StreamTypeWorkspaceArtifact {
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

func initializeEmptyWorkspaceRoot(workspaceRoot string) error {
	workspaceParent := filepath.Dir(workspaceRoot)
	if err := os.MkdirAll(workspaceParent, 0o755); err != nil {
		return fmt.Errorf("create empty workspace mount parent: %w", err)
	}
	stagingRoot, err := os.MkdirTemp(workspaceParent, ".helmr-workspace-empty-*")
	if err != nil {
		return fmt.Errorf("create empty workspace staging dir: %w", err)
	}
	cleanupStaging := func() { _ = os.RemoveAll(stagingRoot) }
	if err := replaceWorkspaceRoot(workspaceRoot, stagingRoot); err != nil {
		cleanupStaging()
		return fmt.Errorf("initialize empty workspace mount: %w", err)
	}
	return nil
}

func workspaceMountPhase(name string, started time.Time, sizeBytes uint64, entryCount uint32, err error) *workspacev0.WorkspaceMountPhase {
	return &workspacev0.WorkspaceMountPhase{
		Name:       name,
		DurationMs: uint64(time.Since(started).Milliseconds()),
		SizeBytes:  sizeBytes,
		EntryCount: entryCount,
		Error:      errorText(err),
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

func restoreWorkspaceMountWorkspaceImage(conn io.Reader, request *workspacev0.MaterializeWorkspaceRequest) (ociImage, func(), error) {
	cleanup := func() {}
	header, bodyLen, err := wire.ReadStreamFrameHeader(conn)
	if err != nil {
		return ociImage{}, cleanup, fmt.Errorf("read workspace image stream header: %w", err)
	}
	if header.Type != wire.StreamTypeRunImage {
		drainStreamBody(conn, bodyLen)
		return ociImage{}, cleanup, fmt.Errorf("unsupported workspace materialize workspace image input type %q", header.Type)
	}
	if header.WorkspaceID != strings.TrimSpace(request.GetEnvelope().GetWorkspaceId()) {
		drainStreamBody(conn, bodyLen)
		return ociImage{}, cleanup, fmt.Errorf("workspace image workspace_id %q does not match materialize workspace_id %q", header.WorkspaceID, request.GetEnvelope().GetWorkspaceId())
	}
	workspaceImage := request.GetWorkspaceImage()
	if workspaceImage.GetSizeBytes() != bodyLen {
		drainStreamBody(conn, bodyLen)
		return ociImage{}, cleanup, fmt.Errorf("workspace image size_bytes %d does not match frame size %d", workspaceImage.GetSizeBytes(), bodyLen)
	}
	frameDigest := ""
	if header.BodyDigest != nil {
		frameDigest = strings.TrimSpace(*header.BodyDigest)
	}
	if frameDigest != "" && frameDigest != strings.TrimSpace(workspaceImage.GetDigest()) {
		drainStreamBody(conn, bodyLen)
		return ociImage{}, cleanup, fmt.Errorf("workspace image digest %q does not match frame digest %q", workspaceImage.GetDigest(), frameDigest)
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
			return ociImage{}, cleanup, fmt.Errorf("create workspace image root: %w", imageRootErr)
		}
		cleanup = func() { _ = os.RemoveAll(imageRoot) }
		image, err = unpackOCIImage(hashedBody, imageRoot)
	}
	if err != nil {
		if _, drainErr := io.Copy(io.Discard, hashedBody); drainErr != nil {
			cleanup()
			return ociImage{}, func() {}, errors.Join(fmt.Errorf("extract workspace image: %w", err), fmt.Errorf("drain workspace image: %w", drainErr))
		}
		cleanup()
		return ociImage{}, func() {}, fmt.Errorf("extract workspace image: %w", err)
	}
	if _, err := io.Copy(io.Discard, hashedBody); err != nil {
		cleanup()
		return ociImage{}, func() {}, fmt.Errorf("drain workspace image: %w", err)
	}
	if digest := hashedBody.Digest(); digest != strings.TrimSpace(workspaceImage.GetDigest()) {
		cleanup()
		return ociImage{}, func() {}, fmt.Errorf("workspace image body digest %q does not match declared digest %q", digest, workspaceImage.GetDigest())
	}
	return image, cleanup, nil
}

func restorePreparedWorkspaceImage(conn io.Reader, request *workspacev0.PrepareWorkspaceRuntimeRequest) (ociImage, func(), error) {
	cleanup := func() {}
	header, bodyLen, err := wire.ReadStreamFrameHeader(conn)
	if err != nil {
		return ociImage{}, cleanup, fmt.Errorf("read prepared workspace image stream header: %w", err)
	}
	if header.Type != wire.StreamTypeRunImage {
		drainStreamBody(conn, bodyLen)
		return ociImage{}, cleanup, fmt.Errorf("unsupported workspace runtime prepare input type %q", header.Type)
	}
	workspaceImage := request.GetWorkspaceImage()
	if workspaceImage.GetSizeBytes() != bodyLen {
		drainStreamBody(conn, bodyLen)
		return ociImage{}, cleanup, fmt.Errorf("prepared workspace image size_bytes %d does not match frame size %d", workspaceImage.GetSizeBytes(), bodyLen)
	}
	frameDigest := ""
	if header.BodyDigest != nil {
		frameDigest = strings.TrimSpace(*header.BodyDigest)
	}
	if frameDigest != "" && frameDigest != strings.TrimSpace(workspaceImage.GetDigest()) {
		drainStreamBody(conn, bodyLen)
		return ociImage{}, cleanup, fmt.Errorf("prepared workspace image digest %q does not match frame digest %q", workspaceImage.GetDigest(), frameDigest)
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
			return ociImage{}, cleanup, fmt.Errorf("create prepared workspace image root: %w", imageRootErr)
		}
		cleanup = func() { _ = os.RemoveAll(imageRoot) }
		image, err = unpackOCIImage(hashedBody, imageRoot)
	}
	if err != nil {
		if _, drainErr := io.Copy(io.Discard, hashedBody); drainErr != nil {
			cleanup()
			return ociImage{}, func() {}, errors.Join(fmt.Errorf("extract prepared workspace image: %w", err), fmt.Errorf("drain prepared workspace image: %w", drainErr))
		}
		cleanup()
		return ociImage{}, func() {}, fmt.Errorf("extract prepared workspace image: %w", err)
	}
	if _, err := io.Copy(io.Discard, hashedBody); err != nil {
		cleanup()
		return ociImage{}, func() {}, fmt.Errorf("drain prepared workspace image: %w", err)
	}
	if digest := hashedBody.Digest(); digest != strings.TrimSpace(workspaceImage.GetDigest()) {
		cleanup()
		return ociImage{}, func() {}, fmt.Errorf("prepared workspace image body digest %q does not match declared digest %q", digest, workspaceImage.GetDigest())
	}
	return image, cleanup, nil
}

func handleWorkspaceEventsConnection(ctx context.Context, conn io.ReadWriter, registry *workspaceOperationRegistry) error {
	var envelope workspacev0.WorkspaceOperationEnvelope
	if err := frameio.ReadProtoFrame(conn, &envelope); err != nil {
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
			if err := frameio.WriteProtoFrame(conn, event); err != nil {
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
		if writeErr := frameio.WriteProtoFrame(conn, response); writeErr != nil {
			return errors.Join(err, fmt.Errorf("write workspace stop failure: %w", writeErr))
		}
		return nil
	}
	return nil
}

func handleWorkspaceStop(conn io.ReadWriter, registry *workspaceOperationRegistry) error {
	var request workspacev0.StopWorkspaceRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
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
	if err := frameio.WriteProtoFrame(conn, response); err != nil {
		return fmt.Errorf("write workspace stop response: %w", err)
	}
	if request.GetCaptureBeforeStop() {
		entryCount := artifact.EntryCount
		if err := wire.WriteFileFrameWithMetadata(conn, wire.StreamHeader{
			Type:        wire.StreamTypeWorkspaceArtifact,
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

func handleWorkspaceOperationConnection(ctx context.Context, conn io.ReadWriter, registry *workspaceOperationRegistry) error {
	var request workspacev0.WorkspaceOperationRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
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
	if strings.TrimSpace(request.OperationKind) == wire.GuestVerbBasicExec {
		result := entry.runWorkspaceBasicExec(ctx, &request)
		return frameio.WriteProtoFrame(conn, result)
	}
	actual, err := wire.RequestFingerprint(request.OperationKind, []byte(request.RequestJson))
	if err != nil {
		return writeWorkspaceOperationResult(conn, err)
	}
	if actual != fingerprint {
		return writeWorkspaceOperationResult(conn, fmt.Errorf("workspace operation request_fingerprint %q does not match request %q", fingerprint, actual))
	}
	switch strings.TrimSpace(request.OperationKind) {
	case wire.GuestVerbStartExec:
		return writeWorkspaceOperationResult(conn, entry.startWorkspaceExec(request.GetEnvelope(), request.RequestJson))
	case wire.GuestVerbCreatePty:
		return writeWorkspaceOperationResult(conn, entry.createWorkspacePty(request.GetEnvelope(), request.RequestJson))
	case wire.GuestVerbResizePty:
		return writeWorkspaceOperationResult(conn, entry.resizeWorkspacePty(request.RequestJson))
	case wire.GuestVerbClosePty:
		return writeWorkspaceOperationResult(conn, entry.closeWorkspacePty(request.RequestJson))
	default:
		return frameio.WriteProtoFrame(conn, &workspacev0.WorkspaceOperationResult{
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
	return frameio.WriteProtoFrame(conn, result)
}

func workspaceRootForImage(imageRoot, mountPath string) (string, error) {
	root, err := confinedLayerPath(
		imageRoot,
		strings.TrimPrefix(mountPath, "/"),
	)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return root, nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf(
			"workspace mount path is not a directory: %s",
			mountPath,
		)
	}
	return root, nil
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type digestingReader struct {
	reader io.Reader
	hash   hash.Hash
}

func newDigestingReader(reader io.Reader) *digestingReader {
	return &digestingReader{reader: reader, hash: sha256.New()}
}

func (reader *digestingReader) Read(body []byte) (int, error) {
	count, err := reader.reader.Read(body)
	if count > 0 {
		_, _ = reader.hash.Write(body[:count])
	}
	return count, err
}

func (reader *digestingReader) Digest() string {
	return sha256sum.DigestHash(reader.hash)
}

func replaceWorkspaceRoot(workspaceRoot, stagingRoot string) error {
	workspaceParent := filepath.Dir(workspaceRoot)
	backupRoot, err := os.MkdirTemp(
		workspaceParent,
		".helmr-workspace-backup-*",
	)
	if err != nil {
		return fmt.Errorf("create workspace backup marker: %w", err)
	}
	if err := os.Remove(backupRoot); err != nil {
		return fmt.Errorf("remove workspace backup marker: %w", err)
	}
	backupCreated := false
	if err := os.Rename(workspaceRoot, backupRoot); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("move existing workspace aside: %w", err)
		}
	} else {
		backupCreated = true
	}
	if err := os.Rename(stagingRoot, workspaceRoot); err != nil {
		if backupCreated {
			if rollbackErr := os.Rename(
				backupRoot,
				workspaceRoot,
			); rollbackErr != nil {
				return errors.Join(
					fmt.Errorf("install restored workspace: %w", err),
					fmt.Errorf("rollback workspace restore: %w", rollbackErr),
				)
			}
		}
		return fmt.Errorf("install restored workspace: %w", err)
	}
	if backupCreated {
		if err := os.RemoveAll(backupRoot); err != nil {
			return fmt.Errorf("remove replaced workspace backup: %w", err)
		}
	}
	return nil
}
