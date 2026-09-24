package guestd

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/oci"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

const (
	workspaceImageMediaType = "application/vnd.helmr.workspace-image.v0.oci-tar"
	workspaceImageEncoding  = "oci-tar"
)

type workspaceOperationRegistry struct {
	mu              sync.RWMutex
	entries         map[string]*workspaceMountEntry
	preparedRuntime *preparedWorkspaceRuntime
	programClaims   []*managedProgramClaim
}

type managedProgramClaim struct {
	entry     *workspaceMountEntry
	authority *workspacev0.WorkspaceRunAuthority
	released  chan struct{}
}

type workspaceAuthorityState uint8

const (
	workspaceAuthorityLive workspaceAuthorityState = iota
	workspaceAuthorityFinalizing
)

type workspaceMountEntry struct {
	channelToken           string
	workspaceID            string
	workspaceMountID       string
	baseWorkspaceVersionID string
	fencingMu              sync.RWMutex
	fencingGeneration      uint64
	runtimeInstanceID      string
	imageRoot              string
	imageConfig            ociRuntimeConfig
	runtimeUser            *resolvedRuntimeUser
	workspaceMount         string
	workspaceRoot          string
	cleanup                func()
	processesMu            sync.Mutex
	basicExec              *workspaceBasicExec
	basicExecRun           func(*workspacev0.WorkspaceBasicExecRequest) *workspacev0.WorkspaceBasicExecResult
	active                 int
	retired                bool
	authorityMu            sync.Mutex
	authority              *workspacev0.WorkspaceRunAuthority
	previousExpiry         int64
	// stopping is terminal for new admissions, protected by lifecycle/finalization locks.
	stopping          bool
	finalizationMu    sync.Mutex
	lifecycleMu       sync.Mutex
	finalizationRoot  string
	authorityState    workspaceAuthorityState
	finalizationID    string
	finalizationKind  string
	recoveryRequired  bool
	processAdmissions int
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

		previous.lifecycleMu.Lock()
		previous.finalizationMu.Lock()
		r.mu.Lock()
		if r.entries[workspaceMountID] != previous {
			r.mu.Unlock()
			previous.finalizationMu.Unlock()
			previous.lifecycleMu.Unlock()
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
		previous.lifecycleMu.Unlock()
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

		entry.lifecycleMu.Lock()
		entry.finalizationMu.Lock()
		r.mu.Lock()
		if r.entries[workspaceMountID] != entry || !workspaceEntryMatches(entry, workspaceMountID, workspaceID, token) {
			r.mu.Unlock()
			entry.finalizationMu.Unlock()
			entry.lifecycleMu.Unlock()
			continue
		}
		if fencingGeneration < entry.currentFencingGeneration() {
			r.mu.Unlock()
			entry.finalizationMu.Unlock()
			entry.lifecycleMu.Unlock()
			return nil, func() {}, false
		}
		entry.processesMu.Lock()
		finalizing := entry.authorityState == workspaceAuthorityFinalizing || entry.recoveryRequired
		entry.processesMu.Unlock()
		if finalizing || entry.basicExec != nil || r.hasProgramClaimLocked(entry) {
			r.mu.Unlock()
			entry.finalizationMu.Unlock()
			entry.lifecycleMu.Unlock()
			return nil, func() {}, false
		}
		entry.setFencingGeneration(fencingGeneration)
		entry.active++
		r.mu.Unlock()
		entry.finalizationMu.Unlock()
		entry.lifecycleMu.Unlock()
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
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	r.retireLocked(workspaceMountID, entry)
}

// Caller holds the entry lifecycle/finalization locks.
func (r *workspaceOperationRegistry) retireLocked(workspaceMountID string, entry *workspaceMountEntry) {
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
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	entry.processesMu.Lock()
	recoveryRequired := entry.recoveryRequired
	entry.processesMu.Unlock()
	if recoveryRequired {
		return func() {}, errors.New("workspace mount requires recovery")
	}
	if authority == nil || authority.GetFence() == nil || !r.currentMountLocked(
		entry,
		authority.GetFence().GetWorkspaceMountId(),
		authority.GetFence().GetWorkspaceId(),
		authority.GetChannelToken(),
	) {
		return func() {}, errors.New("program authority is not current for the workspace mount")
	}
	release, err := r.claimProgramLocked(entry, authority)
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
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
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
		return func() {}, errors.New("workspace is unavailable for program admission")
	}
	entry.authorityMu.Lock()
	if entry.authority == nil {
		entry.authorityMu.Unlock()
		return func() {}, errors.New("workspace run authority is not installed")
	}
	authority := proto.Clone(entry.authority).(*workspacev0.WorkspaceRunAuthority)
	entry.authorityMu.Unlock()
	release, err := r.claimProgramLocked(entry, authority)
	return release, err
}

func (r *workspaceOperationRegistry) claimProgramLocked(
	entry *workspaceMountEntry,
	authority *workspacev0.WorkspaceRunAuthority,
) (func(), error) {
	if entry.stopping {
		return func() {}, errors.New("workspace mount is stopping")
	}
	if entry.basicExec != nil {
		return func() {}, errors.New("workspace mount is owned by an exec operation")
	}
	if authority == nil || authority.GetFence() == nil {
		return func() {}, errors.New("managed program authority is required")
	}
	r.mu.Lock()
	if len(r.programClaims) != 0 {
		r.mu.Unlock()
		return func() {}, errors.New("workspace already has an active managed program")
	}
	claim := &managedProgramClaim{
		entry:     entry,
		authority: proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority),
		released:  make(chan struct{}),
	}
	r.programClaims = append(r.programClaims, claim)
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		for index, current := range r.programClaims {
			if current != claim {
				continue
			}
			r.programClaims = append(r.programClaims[:index], r.programClaims[index+1:]...)
			close(claim.released)
			break
		}
		r.mu.Unlock()
	}, nil
}

func (r *workspaceOperationRegistry) hasProgramClaimLocked(entry *workspaceMountEntry) bool {
	for _, claim := range r.programClaims {
		if claim.entry == entry {
			return true
		}
	}
	return false
}

func (r *workspaceOperationRegistry) waitForProgramRelease(
	ctx context.Context,
	entry *workspaceMountEntry,
	authority *workspacev0.WorkspaceRunAuthority,
) error {
	if authority == nil || authority.GetFence() == nil {
		return errors.New("workspace finalization program authority is required")
	}
	runLeaseID := authority.GetFence().GetRunLeaseId()
	r.mu.Lock()
	var released <-chan struct{}
	for _, claim := range r.programClaims {
		if claim.entry == entry && claim.authority.GetFence().GetRunLeaseId() == runLeaseID {
			released = claim.released
			break
		}
	}
	r.mu.Unlock()
	if released == nil {
		return nil
	}
	select {
	case <-released:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *workspaceOperationRegistry) currentProgramEntry(
	runID string,
	attemptNumber uint32,
) *workspaceMountEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := len(r.programClaims) - 1; index >= 0; index-- {
		claim := r.programClaims[index]
		fence := claim.authority.GetFence()
		if fence.GetRunId() == runID && fence.GetAttemptNumber() == attemptNumber {
			return claim.entry
		}
	}
	return nil
}

func (r *workspaceOperationRegistry) programClaimLocked(
	entry *workspaceMountEntry,
	authority *workspacev0.WorkspaceRunAuthority,
) *managedProgramClaim {
	if authority == nil || authority.GetFence() == nil {
		return nil
	}
	fence := authority.GetFence()
	for index := len(r.programClaims) - 1; index >= 0; index-- {
		claim := r.programClaims[index]
		current := claim.authority.GetFence()
		if claim.entry == entry &&
			current.GetRunId() == fence.GetRunId() &&
			current.GetAttemptNumber() == fence.GetAttemptNumber() {
			return claim
		}
	}
	return nil
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
		phases, err := registry.materializeRestoredWorkspaceMount(&request, waits)
		if err != nil {
			phases = appendWorkspaceMountFailurePhase(phases, "guest_restore_rebind", totalStarted, err)
			writeErr := frameio.WriteProtoFrame(conn, &workspacev0.MaterializeWorkspaceResponse{Status: "failed", Phases: phases})
			if writeErr != nil {
				return errors.Join(err, writeErr)
			}
			return err
		}
		logger.Info("restored workspace mount rebound", "workspace_id", workspaceID,
			"workspace_mount_id", workspaceMountID, "checkpoint_id", request.GetRestoredCheckpointId(),
			"duration_ms", time.Since(totalStarted).Milliseconds())
		return frameio.WriteProtoFrame(conn, &workspacev0.MaterializeWorkspaceResponse{
			Status: "running", GuestdChannelTokenHash: sha256sum.HexBytes([]byte(strings.TrimSpace(envelope.ChannelToken))),
			Phases: phases, Target: proto.Clone(request.GetTarget()).(*workspacev0.ComputerMountTarget),
		})
	}
	entry, phases, err := restoreWorkspaceMount(&request, registry)
	if err != nil {
		phases = appendWorkspaceMountFailurePhase(phases, "guest_materialize", totalStarted, err)
		writeErr := frameio.WriteProtoFrame(conn, &workspacev0.MaterializeWorkspaceResponse{
			Status: "failed",
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
		Status:                 "running",
		GuestdChannelTokenHash: sha256sum.HexBytes([]byte(strings.TrimSpace(envelope.ChannelToken))),
		Phases:                 phases,
		Target:                 proto.Clone(request.GetTarget()).(*workspacev0.ComputerMountTarget),
	})
}

func (r *workspaceOperationRegistry) materializeRestoredWorkspaceMount(
	request *workspacev0.MaterializeWorkspaceRequest,
	waits *waitingRunRegistry,
) ([]*workspacev0.WorkspaceMountPhase, error) {
	started := time.Now()
	if request == nil || request.GetEnvelope() == nil || !request.GetUsePreparedRuntime() {
		return nil, errors.New("restored workspace materialization requires a prepared runtime")
	}
	checkpointID := strings.TrimSpace(request.GetRestoredCheckpointId())
	sourceVersionID := strings.TrimSpace(request.GetRestoreSourceVersionId())
	target, err := computerMountTargetFromProto(request.GetTarget())
	if err != nil {
		return nil, fmt.Errorf("restored workspace target: %w", err)
	}
	if waits == nil || !waits.hasFrozenProgramCheckpoint(checkpointID) {
		return nil, errors.New("restored workspace materialization did not match a frozen program checkpoint")
	}
	envelope := request.GetEnvelope()
	newMountID := strings.TrimSpace(envelope.GetWorkspaceMountId())
	workspaceID := strings.TrimSpace(envelope.GetWorkspaceId())
	channelToken := strings.TrimSpace(envelope.GetChannelToken())
	runtimeInstanceID := strings.TrimSpace(request.GetRuntimeInstanceId())
	mountPath := filepath.Clean(strings.TrimSpace(request.GetMountPath()))
	if newMountID == "" || workspaceID == "" || channelToken == "" || runtimeInstanceID == "" ||
		checkpointID == "" || sourceVersionID == "" || envelope.GetFencingGeneration() == 0 || mountPath == "." ||
		mountPath == string(filepath.Separator) || !filepath.IsAbs(mountPath) {
		return nil, errors.New("restored workspace materialization authority is incomplete")
	}
	parentRunID, parentAttemptNumber, ok := waits.frozenProgramForCheckpoint(checkpointID)
	if !ok {
		return nil, errors.New("restored workspace has no frozen program identity")
	}
	entry := r.currentProgramEntry(parentRunID, parentAttemptNumber)
	if entry == nil {
		return nil, errors.New("restored workspace has no active frozen program")
	}
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	entry.processesMu.Lock()
	unavailable := entry.authorityState == workspaceAuthorityFinalizing || entry.recoveryRequired
	entry.processesMu.Unlock()
	if !r.hasProgramClaimLocked(entry) || unavailable || entry.workspaceID != workspaceID ||
		filepath.Clean(entry.workspaceMount) != mountPath {
		return nil, errors.New("restored workspace materialization did not match the frozen mounted runtime")
	}
	currentGeneration := entry.currentFencingGeneration()
	if envelope.GetFencingGeneration() == currentGeneration && entry.workspaceMountID == newMountID &&
		entry.channelToken == channelToken && entry.runtimeInstanceID == runtimeInstanceID &&
		entry.baseWorkspaceVersionID == target.GetBaseWorkspaceVersionId() && r.entries[newMountID] == entry {
		return []*workspacev0.WorkspaceMountPhase{
			workspaceMountPhase("guest_restore_materialize_replay", started, 0, 0, nil),
		}, nil
	}
	if envelope.GetFencingGeneration() <= currentGeneration {
		return nil, errors.New("restored workspace materialization fencing generation did not advance")
	}
	if current := r.entries[newMountID]; current != nil && current != entry {
		return nil, errors.New("restored workspace materialization target mount is already registered")
	}
	if entry.baseWorkspaceVersionID != sourceVersionID {
		return nil, errors.New("restored workspace source version does not match the frozen mounted runtime")
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
	entry.baseWorkspaceVersionID = target.GetBaseWorkspaceVersionId()
	entry.setFencingGeneration(envelope.GetFencingGeneration())
	r.entries[newMountID] = entry
	return []*workspacev0.WorkspaceMountPhase{
		workspaceMountPhase("guest_restore_materialize", started, 0, 0, nil),
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
			Status:            "failed",
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
		Status:            "prepared",
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
	if strings.TrimSpace(os.Getenv("HELMR_GUESTD_COMPUTER_ROOT")) == "" {
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

func restoreWorkspaceMount(request *workspacev0.MaterializeWorkspaceRequest, registry *workspaceOperationRegistry) (*workspaceMountEntry, []*workspacev0.WorkspaceMountPhase, error) {
	entry := &workspaceMountEntry{}
	var phases []*workspacev0.WorkspaceMountPhase
	envelope := request.GetEnvelope()
	workspaceMountID := strings.TrimSpace(envelope.GetWorkspaceMountId())
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
	target, err := computerMountTargetFromProto(request.GetTarget())
	if err != nil {
		return nil, phases, fmt.Errorf("workspace materialize target: %w", err)
	}
	entry.baseWorkspaceVersionID = target.GetBaseWorkspaceVersionId()
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
	if !request.GetUsePreparedRuntime() {
		return nil, phases, errors.New("computer mount requires a prepared runtime")
	}
	prepared, ok := registry.takePreparedRuntime(runtimeInstanceID, workspaceImage.GetDigest(), mountPath)
	if !ok {
		return nil, phases, errors.New("prepared computer runtime is not available")
	}
	entry.imageRoot, entry.imageConfig = prepared.imageRoot, prepared.imageConfig
	entry.runtimeUser, entry.workspaceMount = prepared.runtimeUser, prepared.workspaceMount
	entry.workspaceRoot, entry.cleanup = prepared.workspaceRoot, prepared.cleanup
	finalizationRoot, err := os.MkdirTemp(filepath.Dir(entry.imageRoot), ".helmr-workspace-state-*")
	if err != nil {
		entry.cleanup()
		return nil, phases, fmt.Errorf("create workspace finalization state: %w", err)
	}
	entry.finalizationRoot = finalizationRoot
	cleanupMount := entry.cleanup
	entry.cleanup = func() {
		cleanupMount()
		_ = os.RemoveAll(finalizationRoot)
	}
	return entry, phases, nil
}

func computerMountTargetFromProto(target *workspacev0.ComputerMountTarget) (*workspacev0.ComputerMountTarget, error) {
	if target == nil || strings.TrimSpace(target.GetBaseWorkspaceVersionId()) == "" {
		return nil, errors.New("computer mount version is required")
	}
	return target, nil
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

func restorePreparedWorkspaceImage(conn io.Reader, request *workspacev0.PrepareWorkspaceRuntimeRequest) (ociImage, func(), error) {
	cleanup := func() {}
	if root := strings.TrimSpace(os.Getenv("HELMR_GUESTD_COMPUTER_ROOT")); root != "" {
		config := request.GetMountedImageConfig()
		if config == nil {
			return ociImage{}, cleanup, errors.New("computer preparation requires admitted image config")
		}
		return ociImage{RootfsDir: root, Config: oci.RuntimeConfig{Env: config.GetEnv(), WorkingDir: config.GetWorkingDir(), User: config.GetUser(), Entrypoint: config.GetEntrypoint(), Cmd: config.GetCmd()}}, cleanup, nil
	}
	if config := request.GetMountedImageConfig(); config != nil {
		substrateRoot := guestdSubstrateRoot()
		if substrateRoot == "" {
			return ociImage{}, cleanup, errors.New("prepared image config requires a mounted substrate")
		}
		return imageFromMountedSubstrateConfig(oci.RuntimeConfig{
			Env: config.GetEnv(), WorkingDir: config.GetWorkingDir(), User: config.GetUser(),
			Entrypoint: config.GetEntrypoint(), Cmd: config.GetCmd(),
		}, substrateRoot)
	}
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

func handleWorkspaceStopConnection(ctx context.Context, conn io.ReadWriter, registry *workspaceOperationRegistry) error {
	if err := handleWorkspaceStop(ctx, conn, registry); err != nil {
		response := &workspacev0.StopWorkspaceResponse{
			Status:    "failed",
			ErrorJson: workspaceStopErrorJSON(err),
		}
		if writeErr := frameio.WriteProtoFrame(conn, response); writeErr != nil {
			return errors.Join(err, fmt.Errorf("write workspace stop failure: %w", writeErr))
		}
		return nil
	}
	return nil
}

func handleWorkspaceStop(ctx context.Context, conn io.ReadWriter, registry *workspaceOperationRegistry) error {
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
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	if !registry.currentExactLocked(entry, envelope.WorkspaceMountId, envelope.WorkspaceId, envelope.ChannelToken, envelope.FencingGeneration) {
		return errors.New("workspace stop mount is no longer current")
	}
	registry.mu.Lock()
	hasProgram := registry.hasProgramClaimLocked(entry)
	registry.mu.Unlock()
	entry.processesMu.Lock()
	activeExecs := entry.processAdmissions
	entry.processesMu.Unlock()
	if activeExecs != 0 || hasProgram {
		return errors.New("workspace stop requires no active exec")
	}
	entry.stopping = true
	entry.processesMu.Lock()
	entry.authorityState = workspaceAuthorityFinalizing
	entry.processesMu.Unlock()
	// Cooperative guest writeback precedes the host's disk-only pause. Physical
	// exclusion and publication remain host-owned, not a guest receipt.
	syscall.Sync()
	return frameio.WriteProtoFrame(conn, &workspacev0.StopWorkspaceResponse{Status: "stopped"})
}

func workspaceStopErrorJSON(err error) string {
	message := "Workspace stop failed"
	if err != nil {
		message = err.Error()
	}
	body, marshalErr := json.Marshal(map[string]string{"message": message})
	if marshalErr != nil {
		return `{"message":"Workspace stop failed"}`
	}
	return string(body)
}

func handleWorkspaceBasicExecConnection(
	ctx context.Context,
	conn io.ReadWriter,
	registry *workspaceOperationRegistry,
) error {
	var request workspacev0.WorkspaceBasicExecRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
		return fmt.Errorf("read workspace BasicExec request: %w", err)
	}
	envelope := request.GetEnvelope()
	fingerprint := ""
	if envelope != nil {
		fingerprint = strings.TrimSpace(envelope.GetRequestFingerprint())
	}
	fail := func(code string, err error) error {
		return frameio.WriteProtoFrame(
			conn,
			workspaceBasicExecFailure(fingerprint, code, err),
		)
	}
	if envelope == nil {
		return fail(
			"workspace_exec_invalid",
			errors.New("workspace BasicExec envelope is required"),
		)
	}
	if strings.TrimSpace(envelope.OperationId) == "" ||
		strings.TrimSpace(envelope.WorkspaceMountId) == "" ||
		strings.TrimSpace(envelope.WorkspaceId) == "" {
		return fail(
			"workspace_exec_invalid",
			errors.New("workspace BasicExec identity is incomplete"),
		)
	}
	entry, release, ok := registry.acquireAuthorityMount(
		envelope.WorkspaceMountId,
		envelope.WorkspaceId,
		envelope.ChannelToken,
	)
	if !ok {
		return fail(
			"workspace_exec_fenced",
			errors.New("workspace BasicExec authority is invalid"),
		)
	}
	defer release()
	if envelope.OperationExpiresAtUnixNano <= 0 {
		return fail(
			"workspace_exec_invalid",
			errors.New("workspace BasicExec expiry is required"),
		)
	}
	if time.Now().UnixNano() >= envelope.OperationExpiresAtUnixNano {
		return fail(
			"workspace_exec_expired",
			errors.New("workspace BasicExec claim expired"),
		)
	}
	if fingerprint == "" {
		return fail(
			"workspace_exec_invalid",
			errors.New("workspace BasicExec fingerprint is required"),
		)
	}
	return frameio.WriteProtoFrame(conn, registry.runWorkspaceBasicExec(ctx, entry, &request))
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
