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
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/oci"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

const (
	computerImageMediaType = "application/vnd.helmr.computer-image.v0.oci-tar"
	computerImageEncoding  = "oci-tar"
)

type computerOperationRegistry struct {
	mu                      sync.RWMutex
	captureAbortMu          sync.Mutex
	entries                 map[string]*computerMountEntry
	preparedRuntime         *preparedComputerRuntime
	programClaims           []*managedProgramClaim
	captureRequest          *computerv0.FreezeComputerRequest
	captureAbort            *captureAbortInstallation
	restoredMaterialization *computerv0.MaterializeComputerRequest
	restoreInstallation     *computerv0.ComputerRestoreInstallation
	restoreActivated        bool
	materializations        int
}

type managedProgramClaim struct {
	entry          *computerMountEntry
	authority      *computerv0.ComputerRunAuthority
	previousExpiry int64
	stop           context.CancelFunc
	stopRequested  bool
	done           chan struct{}
	cleanupErr     error
}

type computerMountEntry struct {
	writerGeneration          int64
	channelToken              string
	computerID                string
	computerInstanceID        string
	baseComputerDiskVersionID string
	fencingMu                 sync.RWMutex
	imageRoot                 string
	imageConfig               ociRuntimeConfig
	runtimeUser               *resolvedRuntimeUser
	computerMount             string
	computerRoot              string
	cleanup                   func()
	processesMu               sync.Mutex
	commands                  map[string]*computerBasicExec
	programCleanup            map[string]*managedProgramClaim
	basicExecRun              func(context.Context, *computerv0.ComputerBasicExecRequest) *computerv0.ComputerBasicExecResult
	active                    int
	retired                   bool
	// stopping is terminal for new admissions, protected by lifecycle/finalization locks.
	stopping          bool
	finalizationMu    sync.Mutex
	lifecycleMu       sync.Mutex
	recoveryRequired  bool
	processAdmissions int
}

type preparedComputerRuntime struct {
	computerID          string
	writerGeneration    int64
	computerInstanceID  string
	computerImageDigest string
	imageRoot           string
	imageConfig         ociRuntimeConfig
	runtimeUser         *resolvedRuntimeUser
	computerMount       string
	computerRoot        string
	cleanup             func()
}

func newComputerOperationRegistry() *computerOperationRegistry {
	return &computerOperationRegistry{entries: map[string]*computerMountEntry{}}
}

func (r *computerOperationRegistry) setPreparedRuntime(runtime *preparedComputerRuntime) error {
	if runtime == nil {
		return nil
	}
	r.mu.Lock()
	if r.captureRequest != nil {
		r.mu.Unlock()
		return errors.New("computer capture has sealed preparation")
	}
	previous := r.preparedRuntime
	r.preparedRuntime = runtime
	r.mu.Unlock()
	if previous != nil && previous.cleanup != nil {
		previous.cleanup()
	}
	return nil
}

func (r *computerOperationRegistry) takePreparedRuntime(computerInstanceID string, computerID string, computerImageDigest string, computerMount string, writerGeneration uint64) (*preparedComputerRuntime, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prepared := r.preparedRuntime
	if prepared == nil || r.captureRequest != nil {
		return nil, false
	}
	if writerGeneration == 0 || uint64(prepared.writerGeneration) != writerGeneration || prepared.computerID != computerID || prepared.computerInstanceID != computerInstanceID ||
		strings.TrimSpace(prepared.computerImageDigest) != strings.TrimSpace(computerImageDigest) ||
		strings.TrimSpace(prepared.computerMount) != strings.TrimSpace(computerMount) {
		return nil, false
	}
	r.preparedRuntime = nil
	return prepared, true
}

func (r *computerOperationRegistry) register(computerInstanceID string, entry *computerMountEntry) error {
	for {
		r.mu.Lock()
		if r.captureRequest != nil {
			r.mu.Unlock()
			return errors.New("computer capture has sealed materialization")
		}
		previous := r.entries[computerInstanceID]
		if previous == entry {
			entry.computerInstanceID = computerInstanceID
			r.mu.Unlock()
			return nil
		}
		if previous == nil {
			entry.computerInstanceID = computerInstanceID
			r.entries[computerInstanceID] = entry
			r.mu.Unlock()
			return nil
		}
		r.mu.Unlock()

		previous.lifecycleMu.Lock()
		previous.finalizationMu.Lock()
		r.mu.Lock()
		if r.captureRequest != nil || r.entries[computerInstanceID] != previous {
			r.mu.Unlock()
			previous.finalizationMu.Unlock()
			previous.lifecycleMu.Unlock()
			continue
		}
		entry.computerInstanceID = computerInstanceID
		r.entries[computerInstanceID] = entry
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
		return nil
	}
}

func (r *computerOperationRegistry) acquireAuthorityMount(computerInstanceID string, computerID string, token string) (*computerMountEntry, func(), bool) {
	r.mu.Lock()
	entry := r.entries[computerInstanceID]
	computerID = strings.TrimSpace(computerID)
	token = strings.TrimSpace(token)
	if computerID == "" || token == "" || !computerEntryMatches(entry, computerInstanceID, computerID, token) {
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

func (r *computerOperationRegistry) acquireExact(computerInstanceID string, computerID string, token string, writerGeneration uint64) (*computerMountEntry, func(), bool) {
	r.mu.Lock()
	entry, ok := r.entries[computerInstanceID]
	computerID = strings.TrimSpace(computerID)
	token = strings.TrimSpace(token)
	if !(ok &&
		computerID != "" &&
		token != "" &&
		writerGeneration != 0 &&
		entry.computerInstanceID == computerInstanceID &&
		entry.computerID == computerID &&
		entry.currentWriterGeneration() == writerGeneration &&
		!entry.retired &&
		subtle.ConstantTimeCompare([]byte(entry.channelToken), []byte(token)) == 1) {
		r.mu.Unlock()
		return nil, func() {}, false
	}
	entry.active++
	r.mu.Unlock()
	return entry, func() { r.release(entry) }, true
}

func computerEntryMatches(entry *computerMountEntry, computerInstanceID string, computerID string, token string) bool {
	return entry != nil &&
		entry.computerInstanceID == computerInstanceID &&
		entry.computerID == computerID &&
		!entry.retired &&
		subtle.ConstantTimeCompare([]byte(entry.channelToken), []byte(token)) == 1
}

func (entry *computerMountEntry) currentWriterGeneration() uint64 {
	entry.fencingMu.RLock()
	defer entry.fencingMu.RUnlock()
	return uint64(entry.writerGeneration)
}

func (entry *computerMountEntry) setWriterGeneration(generation uint64) {
	entry.fencingMu.Lock()
	entry.writerGeneration = int64(generation)
	entry.fencingMu.Unlock()
}

func (r *computerOperationRegistry) currentMountLocked(entry *computerMountEntry, computerInstanceID string, computerID string, token string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.entries[computerInstanceID] == entry && computerEntryMatches(entry, computerInstanceID, computerID, token)
}

func (r *computerOperationRegistry) release(entry *computerMountEntry) {
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

func (r *computerOperationRegistry) retire(computerInstanceID string, entry *computerMountEntry) {
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	r.retireLocked(computerInstanceID, entry)
}

// Caller holds the entry lifecycle/finalization locks.
func (r *computerOperationRegistry) retireLocked(computerInstanceID string, entry *computerMountEntry) {
	r.mu.Lock()
	current := r.entries[computerInstanceID]
	if current != entry {
		r.mu.Unlock()
		return
	}
	delete(r.entries, computerInstanceID)
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

func (r *computerOperationRegistry) admitProgram(entry *computerMountEntry, authority *computerv0.ComputerRunAuthority, clock func() time.Time) (func(), error) {
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	entry.processesMu.Lock()
	recoveryRequired := entry.recoveryRequired
	entry.processesMu.Unlock()
	if recoveryRequired {
		return func() {}, errors.New("computer mount requires recovery")
	}
	if entry.stopping {
		return func() {}, errors.New("computer is stopping")
	}
	r.mu.Lock()
	if r.captureRequest != nil {
		r.mu.Unlock()
		return func() {}, errors.New("computer capture has sealed program admission")
	}
	if authority == nil || authority.GetFence() == nil || r.entries[authority.GetFence().GetComputerInstanceId()] != entry {
		r.mu.Unlock()
		return func() {}, errors.New("program authority is not current for the computer instance")
	}
	if clock == nil {
		clock = time.Now
	}
	if err := validateComputerRunAuthority(entry, authority, clock()); err != nil {
		r.mu.Unlock()
		return func() {}, err
	}
	if entry.programCleanup[authority.GetFence().GetRunLeaseId()] != nil {
		r.mu.Unlock()
		return func() {}, errors.New("program lease has been retired")
	}
	for _, current := range r.programClaims {
		if current.authority.GetFence().GetRunId() == authority.GetFence().GetRunId() {
			r.mu.Unlock()
			return func() {}, errors.New("program Run already has an active claim")
		}
	}
	claim := &managedProgramClaim{
		entry:     entry,
		authority: proto.Clone(authority).(*computerv0.ComputerRunAuthority),
		done:      make(chan struct{}),
	}
	r.programClaims = append(r.programClaims, claim)
	r.mu.Unlock()
	return sync.OnceFunc(func() {
		r.mu.Lock()
		if entry.programCleanup == nil {
			entry.programCleanup = make(map[string]*managedProgramClaim)
		}
		fence := claim.authority.GetFence()
		// Keep only the scoped identity and cleanup result for lost-reply replay.
		claim.authority = &computerv0.ComputerRunAuthority{Fence: &computerv0.ComputerAuthorityFence{RunId: fence.RunId, RunLeaseId: fence.RunLeaseId, AttemptNumber: fence.AttemptNumber}}
		entry.programCleanup[claim.authority.GetFence().GetRunLeaseId()] = claim
		claim.stop = nil
		close(claim.done)
		for index, current := range r.programClaims {
			if current != claim {
				continue
			}
			r.programClaims = append(r.programClaims[:index], r.programClaims[index+1:]...)
			break
		}
		r.mu.Unlock()
	}), nil
}

func (r *computerOperationRegistry) programClaimLocked(
	entry *computerMountEntry,
	authority *computerv0.ComputerRunAuthority,
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

func handleComputerMaterializeConnection(_ context.Context, conn io.ReadWriter, logger *slog.Logger, registry *computerOperationRegistry, waits *waitingRunRegistry) error {
	if logger == nil {
		logger = slog.Default()
	}
	totalStarted := time.Now()
	var request computerv0.MaterializeComputerRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
		return fmt.Errorf("read computer materialize request: %w", err)
	}
	if request.GetRestoredCheckpointId() == "" {
		releaseMutation, err := registry.reserveMaterialization()
		if err != nil {
			return err
		}
		defer releaseMutation()
	}
	envelope := request.GetEnvelope()
	if envelope == nil {
		return errors.New("computer materialize envelope is required")
	}
	if strings.TrimSpace(envelope.ComputerInstanceId) == "" {
		return errors.New("computer materialize computer_instance_id is required")
	}
	if strings.TrimSpace(envelope.ComputerId) == "" {
		return errors.New("computer materialize computer_id is required")
	}
	if strings.TrimSpace(envelope.ChannelToken) == "" {
		return errors.New("computer materialize channel_token is required")
	}
	if envelope.WriterGeneration == 0 {
		return errors.New("computer materialize writer_generation is required")
	}
	computerInstanceID := strings.TrimSpace(envelope.ComputerInstanceId)
	computerID := strings.TrimSpace(envelope.ComputerId)
	if strings.TrimSpace(request.GetRestoredCheckpointId()) != "" {
		phases, err := registry.materializeRestoredComputerMount(&request, waits)
		if err != nil {
			phases = appendComputerMountFailurePhase(phases, "guest_restore_rebind", totalStarted, err)
			writeErr := frameio.WriteProtoFrame(conn, &computerv0.MaterializeComputerResponse{Status: "failed", Phases: phases})
			if writeErr != nil {
				return errors.Join(err, writeErr)
			}
			return err
		}
		logger.Info("restored computer mount rebound", "computer_id", computerID,
			"computer_instance_id", computerInstanceID, "checkpoint_id", request.GetRestoredCheckpointId(),
			"duration_ms", time.Since(totalStarted).Milliseconds())
		return frameio.WriteProtoFrame(conn, &computerv0.MaterializeComputerResponse{
			Status: "running", GuestdChannelTokenHash: sha256sum.HexBytes([]byte(strings.TrimSpace(envelope.ChannelToken))),
			Phases: phases, Target: proto.Clone(request.GetTarget()).(*computerv0.ComputerMountTarget),
		})
	}
	entry, err := restoreComputerMount(&request, registry)
	var phases []*computerv0.ComputerMountPhase
	if err != nil {
		phases = appendComputerMountFailurePhase(phases, "guest_materialize", totalStarted, err)
		writeErr := frameio.WriteProtoFrame(conn, &computerv0.MaterializeComputerResponse{
			Status: "failed",
			Phases: phases,
		})
		if writeErr != nil {
			return errors.Join(fmt.Errorf("restore materialized computer: %w", err), fmt.Errorf("write computer materialize failure response: %w", writeErr))
		}
		return fmt.Errorf("restore materialized computer: %w", err)
	}
	entry.channelToken = envelope.ChannelToken
	entry.computerID = computerID

	registerStarted := time.Now()
	if err := registry.register(envelope.ComputerInstanceId, entry); err != nil {
		if entry.cleanup != nil {
			entry.cleanup()
		}
		return err
	}
	phases = append(phases, computerMountPhase("guest_register", registerStarted, 0, nil))
	logger.Info("computer materialize registered", "computer_id", computerID, "computer_instance_id", computerInstanceID, "duration_ms", time.Since(totalStarted).Milliseconds())
	return frameio.WriteProtoFrame(conn, &computerv0.MaterializeComputerResponse{
		Status:                 "running",
		GuestdChannelTokenHash: sha256sum.HexBytes([]byte(strings.TrimSpace(envelope.ChannelToken))),
		Phases:                 phases,
		Target:                 proto.Clone(request.GetTarget()).(*computerv0.ComputerMountTarget),
	})
}

func (r *computerOperationRegistry) materializeRestoredComputerMount(
	request *computerv0.MaterializeComputerRequest,
	waits *waitingRunRegistry,
) ([]*computerv0.ComputerMountPhase, error) {
	started := time.Now()
	if request == nil || request.GetEnvelope() == nil || !request.GetUsePreparedRuntime() {
		return nil, errors.New("restored computer materialization requires a prepared runtime")
	}
	checkpointID := strings.TrimSpace(request.GetRestoredCheckpointId())
	target, err := computerMountTargetFromProto(request.GetTarget())
	if err != nil {
		return nil, fmt.Errorf("restored computer target: %w", err)
	}
	if waits == nil {
		return nil, errors.New("restored computer requires frozen waits")
	}
	envelope := request.GetEnvelope()
	computerID := strings.TrimSpace(envelope.GetComputerId())
	channelToken := strings.TrimSpace(envelope.GetChannelToken())
	computerInstanceID := strings.TrimSpace(envelope.GetComputerInstanceId())
	mountPath := filepath.Clean(strings.TrimSpace(request.GetMountPath()))
	if computerInstanceID == "" || computerID == "" || channelToken == "" ||
		checkpointID == "" || envelope.GetWriterGeneration() == 0 || envelope.GetWriterGeneration() > math.MaxInt64 || mountPath == "." ||
		mountPath == string(filepath.Separator) || !filepath.IsAbs(mountPath) {
		return nil, errors.New("restored computer materialization authority is incomplete")
	}
	r.mu.Lock()
	var entry *computerMountEntry
	for _, candidate := range r.entries {
		if candidate == nil || (entry != nil && candidate != entry) {
			r.mu.Unlock()
			return nil, errors.New("restored computer has ambiguous mounted identity")
		}
		entry = candidate
	}
	var prepared *preparedComputerRuntime
	if entry == nil {
		prepared = r.preparedRuntime
		if prepared == nil {
			r.mu.Unlock()
			return nil, errors.New("restored computer has no retained filesystem")
		}
		entry = &computerMountEntry{
			computerID: prepared.computerID, computerInstanceID: prepared.computerInstanceID,
			writerGeneration: prepared.writerGeneration, imageRoot: prepared.imageRoot,
			imageConfig: prepared.imageConfig, runtimeUser: prepared.runtimeUser,
			computerMount: prepared.computerMount, computerRoot: prepared.computerRoot,
		}
	}
	r.mu.Unlock()
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	waits.mu.Lock()
	defer waits.mu.Unlock()
	if entry.retired || entry.computerID != computerID || filepath.Clean(entry.computerMount) != mountPath || r.captureRequest == nil || r.captureRequest.CheckpointId != checkpointID {
		return nil, errors.New("restored computer does not match the sealed capture")
	}
	if r.restoredMaterialization != nil {
		if !proto.Equal(r.restoredMaterialization, request) || r.entries[computerInstanceID] != entry || entry.computerInstanceID != computerInstanceID || entry.currentWriterGeneration() != envelope.GetWriterGeneration() || entry.channelToken != channelToken || entry.baseComputerDiskVersionID != target.GetBaseComputerDiskVersionId() {
			return nil, errors.New("restored computer materialization conflicts with its receipt")
		}
		return []*computerv0.ComputerMountPhase{computerMountPhase("guest_restore_materialize_replay", started, 0, nil)}, nil
	}
	capture := r.captureRequest
	identity := &computerv0.ComputerRestoreIdentity{ComputerId: capture.ComputerId, SourceComputerInstanceId: capture.ComputerInstanceId, WriterGeneration: capture.WriterGeneration, CheckpointId: capture.CheckpointId}
	for _, member := range capture.Runs {
		if member == nil {
			return nil, errors.New("captured member is missing")
		}
		slot := waits.slots[member.RunWaitId]
		if slot == nil {
			return nil, errors.New("captured member has no frozen wait")
		}
		identity.Runs = append(identity.Runs, &computerv0.CapturedRun{RunId: member.RunId, AttemptNumber: member.AttemptNumber, RunWaitId: member.RunWaitId, RunLeaseId: member.RunLeaseId, CorrelationId: slot.correlationID})
	}
	sourceEntry := entry
	if prepared != nil {
		sourceEntry = nil
	}
	if err := verifyFrozenComputerLocked(r, waits, sourceEntry, identity); err != nil {
		return nil, err
	}
	if r.materializations != 0 || (prepared == nil && r.entries[entry.computerInstanceID] != entry) || (prepared != nil && (r.preparedRuntime != prepared || len(r.entries) != 0)) {
		return nil, errors.New("restored computer source changed")
	}
	currentGeneration := entry.currentWriterGeneration()
	if computerInstanceID == entry.computerInstanceID || envelope.GetWriterGeneration() <= currentGeneration {
		return nil, errors.New("restored Computer requires a new Instance and writer generation")
	}
	if current := r.entries[computerInstanceID]; current != nil && current != entry {
		return nil, errors.New("restored computer materialization target mount is already registered")
	}
	for id, current := range r.entries {
		if current == entry {
			delete(r.entries, id)
		}
	}
	if prepared != nil {
		entry.cleanup = prepared.cleanup
		r.preparedRuntime = nil
	}
	entry.channelToken = channelToken
	entry.computerInstanceID = computerInstanceID
	entry.baseComputerDiskVersionID = target.GetBaseComputerDiskVersionId()
	entry.setWriterGeneration(envelope.GetWriterGeneration())
	r.entries[computerInstanceID] = entry
	r.restoredMaterialization = proto.Clone(request).(*computerv0.MaterializeComputerRequest)
	return []*computerv0.ComputerMountPhase{
		computerMountPhase("guest_restore_materialize", started, 0, nil),
	}, nil
}

func handleComputerRuntimePrepareConnection(_ context.Context, conn io.ReadWriter, logger *slog.Logger, registry *computerOperationRegistry) error {
	releaseMutation, mutationErr := registry.reserveMaterialization()
	if mutationErr != nil {
		return mutationErr
	}
	defer releaseMutation()

	if logger == nil {
		logger = slog.Default()
	}
	totalStarted := time.Now()
	var request computerv0.PrepareComputerRuntimeRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
		return fmt.Errorf("read computer runtime prepare request: %w", err)
	}
	runtime, phases, err := restorePreparedComputerRuntime(conn, &request, logger)
	if err != nil {
		phases = appendComputerMountFailurePhase(phases, "guest_runtime_prepare", totalStarted, err)
		writeErr := frameio.WriteProtoFrame(conn, &computerv0.PrepareComputerRuntimeResponse{
			Status:             "failed",
			ComputerInstanceId: request.GetComputerInstanceId(),
			Phases:             phases,
		})
		if writeErr != nil {
			return errors.Join(fmt.Errorf("restore prepared computer runtime: %w", err), fmt.Errorf("write computer runtime prepare failure response: %w", writeErr))
		}
		return fmt.Errorf("restore prepared computer runtime: %w", err)
	}
	if err := registry.setPreparedRuntime(runtime); err != nil {
		if runtime.cleanup != nil {
			runtime.cleanup()
		}
		return err
	}
	logger.Info("computer runtime prepared", "computer_instance_id_hash", computerInstanceLogID(request.GetComputerInstanceId()))
	return frameio.WriteProtoFrame(conn, &computerv0.PrepareComputerRuntimeResponse{
		Status:             "prepared",
		ComputerInstanceId: request.GetComputerInstanceId(),
		Phases:             phases,
	})
}

func computerInstanceLogID(computerInstanceID string) string {
	hash := sha256sum.HexBytes([]byte(computerInstanceID))
	if len(hash) < 16 {
		return hash
	}
	return hash[:16]
}

func restorePreparedComputerRuntime(conn io.Reader, request *computerv0.PrepareComputerRuntimeRequest, logger *slog.Logger) (*preparedComputerRuntime, []*computerv0.ComputerMountPhase, error) {
	var phases []*computerv0.ComputerMountPhase
	computerInstanceID := request.GetComputerInstanceId()
	if strings.TrimSpace(computerInstanceID) == "" || strings.TrimSpace(request.GetComputerId()) == "" || request.GetWriterGeneration() <= 0 {
		return nil, phases, errors.New("computer runtime prepare identity is required")
	}
	mountPath := filepath.Clean(strings.TrimSpace(request.GetMountPath()))
	if mountPath == "" || mountPath == "." || mountPath == string(filepath.Separator) || !filepath.IsAbs(mountPath) {
		return nil, phases, fmt.Errorf("computer runtime prepare mount_path %q is invalid", request.GetMountPath())
	}
	computerImage := request.GetComputerImage()
	if strings.TrimSpace(os.Getenv("HELMR_GUESTD_COMPUTER_ROOT")) == "" {
		if computerImage == nil {
			return nil, phases, errors.New("computer runtime prepare computer_image is required")
		}
		if strings.TrimSpace(computerImage.GetDigest()) == "" {
			return nil, phases, errors.New("computer runtime prepare computer_image digest is required")
		}
		if computerImage.GetMediaType() != computerImageMediaType {
			return nil, phases, fmt.Errorf("computer runtime prepare computer_image media_type %q is not supported", computerImage.GetMediaType())
		}
		if computerImage.GetEncoding() != computerImageEncoding {
			return nil, phases, fmt.Errorf("computer runtime prepare computer_image encoding %q is not supported", computerImage.GetEncoding())
		}
		if computerImage.GetSizeBytes() == 0 {
			return nil, phases, errors.New("computer runtime prepare computer_image size_bytes is required")
		}
	}
	phaseStarted := time.Now()
	image, cleanupImage, err := restorePreparedComputerImage(conn, request)
	phases = append(phases, computerMountPhase("guest_computer_image_restore", phaseStarted, computerImage.GetSizeBytes(), err))
	logger.Info("computer runtime prepare computer image restored", "computer_instance_id_hash", computerInstanceLogID(computerInstanceID), "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", computerImage.GetSizeBytes(), "error", errorText(err))
	if err != nil {
		return nil, phases, err
	}
	cleanup := cleanupImage
	phaseStarted = time.Now()
	runtimeUser, err := resolveRuntimeUser(image.RootfsDir, image.Config.User)
	phases = append(phases, computerMountPhase("guest_runtime_user_resolve", phaseStarted, 0, err))
	if err != nil {
		cleanup()
		return nil, phases, fmt.Errorf("resolve prepared runtime user: %w", err)
	}
	phaseStarted = time.Now()
	computerRoot, err := computerRootForImage(image.RootfsDir, mountPath)
	phases = append(phases, computerMountPhase("guest_computer_root_resolve", phaseStarted, 0, err))
	if err != nil {
		cleanup()
		return nil, phases, fmt.Errorf("resolve prepared runtime computer mount: %w", err)
	}
	return &preparedComputerRuntime{
		computerID: request.GetComputerId(), writerGeneration: request.GetWriterGeneration(),
		computerInstanceID:  computerInstanceID,
		computerImageDigest: strings.TrimSpace(computerImage.GetDigest()),
		imageRoot:           image.RootfsDir,
		imageConfig:         image.Config,
		runtimeUser:         runtimeUser,
		computerMount:       mountPath,
		computerRoot:        computerRoot,
		cleanup:             cleanup,
	}, phases, nil
}

func restoreComputerMount(request *computerv0.MaterializeComputerRequest, registry *computerOperationRegistry) (*computerMountEntry, error) {
	entry := &computerMountEntry{}
	envelope := request.GetEnvelope()
	computerInstanceID := strings.TrimSpace(envelope.GetComputerInstanceId())
	if computerInstanceID == "" {
		return nil, errors.New("computer materialize computer_instance_id is required")
	}
	entry.computerInstanceID = computerInstanceID
	mountPath := filepath.Clean(strings.TrimSpace(request.GetMountPath()))
	if mountPath == "" || mountPath == "." || mountPath == string(filepath.Separator) || !filepath.IsAbs(mountPath) {
		return nil, fmt.Errorf("computer materialize mount_path %q is invalid", request.GetMountPath())
	}
	target, err := computerMountTargetFromProto(request.GetTarget())
	if err != nil {
		return nil, fmt.Errorf("computer materialize target: %w", err)
	}
	entry.baseComputerDiskVersionID = target.GetBaseComputerDiskVersionId()
	computerImage := request.GetComputerImage()
	if computerImage == nil {
		return nil, errors.New("computer materialize computer_image is required")
	}
	if strings.TrimSpace(computerImage.GetDigest()) == "" {
		return nil, errors.New("computer materialize computer_image digest is required")
	}
	if computerImage.GetSizeBytes() == 0 {
		return nil, errors.New("computer materialize computer_image size_bytes is required")
	}
	if !request.GetUsePreparedRuntime() {
		return nil, errors.New("computer mount requires a prepared runtime")
	}
	// Materialization binds the already prepared filesystem; it does not decode
	// an image stream. Format admission belongs to the preparation path.
	prepared, ok := registry.takePreparedRuntime(computerInstanceID, envelope.GetComputerId(), computerImage.GetDigest(), mountPath, envelope.GetWriterGeneration())
	if !ok {
		return nil, errors.New("prepared computer runtime is not available")
	}
	entry.writerGeneration = prepared.writerGeneration
	entry.imageRoot, entry.imageConfig = prepared.imageRoot, prepared.imageConfig
	entry.runtimeUser, entry.computerMount = prepared.runtimeUser, prepared.computerMount
	entry.computerRoot, entry.cleanup = prepared.computerRoot, prepared.cleanup
	return entry, nil
}

func computerMountTargetFromProto(target *computerv0.ComputerMountTarget) (*computerv0.ComputerMountTarget, error) {
	if target == nil || strings.TrimSpace(target.GetBaseComputerDiskVersionId()) == "" {
		return nil, errors.New("computer mount version is required")
	}
	return target, nil
}

func computerMountPhase(name string, started time.Time, sizeBytes uint64, err error) *computerv0.ComputerMountPhase {
	return &computerv0.ComputerMountPhase{
		Name:       name,
		DurationMs: uint64(time.Since(started).Milliseconds()),
		SizeBytes:  sizeBytes,
		Error:      errorText(err),
	}
}

func appendComputerMountFailurePhase(phases []*computerv0.ComputerMountPhase, name string, started time.Time, err error) []*computerv0.ComputerMountPhase {
	if err == nil {
		return phases
	}
	for i := len(phases) - 1; i >= 0; i-- {
		if phases[i] != nil && strings.TrimSpace(phases[i].GetError()) != "" {
			return phases
		}
	}
	return append(phases, computerMountPhase(name, started, 0, err))
}

func restorePreparedComputerImage(conn io.Reader, request *computerv0.PrepareComputerRuntimeRequest) (ociImage, func(), error) {
	cleanup := func() {}
	if root := strings.TrimSpace(os.Getenv("HELMR_GUESTD_COMPUTER_ROOT")); root != "" {
		config := request.GetMountedImageConfig()
		if config == nil {
			return ociImage{}, cleanup, errors.New("computer preparation requires admitted image config")
		}
		return ociImage{RootfsDir: root, Config: oci.RuntimeConfig{Env: config.GetEnv(), WorkingDir: config.GetWorkingDir(), User: config.GetUser(), Entrypoint: config.GetEntrypoint(), Cmd: config.GetCmd()}}, cleanup, nil
	}
	if request.GetMountedImageConfig() != nil {
		return ociImage{}, cleanup, errors.New("prepared image config requires a mounted Computer")
	}
	header, bodyLen, err := wire.ReadStreamFrameHeader(conn)
	if err != nil {
		return ociImage{}, cleanup, fmt.Errorf("read prepared computer image stream header: %w", err)
	}
	if header.Type != wire.StreamTypeRunImage {
		drainStreamBody(conn, bodyLen)
		return ociImage{}, cleanup, fmt.Errorf("unsupported computer runtime prepare input type %q", header.Type)
	}
	computerImage := request.GetComputerImage()
	if computerImage.GetSizeBytes() != bodyLen {
		drainStreamBody(conn, bodyLen)
		return ociImage{}, cleanup, fmt.Errorf("prepared computer image size_bytes %d does not match frame size %d", computerImage.GetSizeBytes(), bodyLen)
	}
	frameDigest := ""
	if header.BodyDigest != nil {
		frameDigest = strings.TrimSpace(*header.BodyDigest)
	}
	if frameDigest != "" && frameDigest != strings.TrimSpace(computerImage.GetDigest()) {
		drainStreamBody(conn, bodyLen)
		return ociImage{}, cleanup, fmt.Errorf("prepared computer image digest %q does not match frame digest %q", computerImage.GetDigest(), frameDigest)
	}
	body := &io.LimitedReader{R: conn, N: int64(bodyLen)}
	hashedBody := newDigestingReader(body)
	imageRoot, err := mkdirGuestdTemp("helmr-prepared-computer-image-*")
	if err != nil {
		drainStreamBody(conn, bodyLen)
		return ociImage{}, cleanup, fmt.Errorf("create prepared computer image root: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(imageRoot) }
	image, err := unpackOCIImage(hashedBody, imageRoot)
	if err != nil {
		if _, drainErr := io.Copy(io.Discard, hashedBody); drainErr != nil {
			cleanup()
			return ociImage{}, func() {}, errors.Join(fmt.Errorf("extract prepared computer image: %w", err), fmt.Errorf("drain prepared computer image: %w", drainErr))
		}
		cleanup()
		return ociImage{}, func() {}, fmt.Errorf("extract prepared computer image: %w", err)
	}
	if _, err := io.Copy(io.Discard, hashedBody); err != nil {
		cleanup()
		return ociImage{}, func() {}, fmt.Errorf("drain prepared computer image: %w", err)
	}
	if digest := hashedBody.Digest(); digest != strings.TrimSpace(computerImage.GetDigest()) {
		cleanup()
		return ociImage{}, func() {}, fmt.Errorf("prepared computer image body digest %q does not match declared digest %q", digest, computerImage.GetDigest())
	}
	return image, cleanup, nil
}

func handleComputerBasicExecConnection(
	ctx context.Context,
	conn io.ReadWriter,
	registry *computerOperationRegistry,
) error {
	var request computerv0.ComputerBasicExecRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
		return fmt.Errorf("read computer BasicExec request: %w", err)
	}
	envelope := request.GetEnvelope()
	fingerprint := ""
	if envelope != nil {
		fingerprint = strings.TrimSpace(envelope.GetRequestFingerprint())
	}
	fail := func(code string, err error) error {
		return frameio.WriteProtoFrame(
			conn,
			commandResultEvent(computerBasicCommandFailure(fingerprint, code, err)),
		)
	}
	if envelope == nil {
		return fail(
			"computer_command_invalid",
			errors.New("computer BasicExec envelope is required"),
		)
	}
	if strings.TrimSpace(envelope.OperationId) == "" ||
		strings.TrimSpace(envelope.ComputerInstanceId) == "" ||
		strings.TrimSpace(envelope.ComputerId) == "" {
		return fail(
			"computer_command_invalid",
			errors.New("computer BasicExec identity is incomplete"),
		)
	}
	entry, release, ok := registry.acquireCommandInstance(
		envelope.ComputerInstanceId,
		envelope.ComputerId,
		envelope.ChannelToken,
	)
	if !ok {
		return fail(
			"computer_command_fenced",
			errors.New("computer BasicExec authority is invalid"),
		)
	}
	defer release()
	if envelope.OperationExpiresAtUnixNano <= 0 {
		return fail(
			"computer_command_invalid",
			errors.New("computer BasicExec expiry is required"),
		)
	}
	if time.Now().UnixNano() >= envelope.OperationExpiresAtUnixNano {
		return fail(
			"computer_command_expired",
			errors.New("computer BasicExec claim expired"),
		)
	}
	if fingerprint == "" {
		return fail(
			"computer_command_invalid",
			errors.New("computer BasicExec fingerprint is required"),
		)
	}
	execution, code, err := registry.startComputerBasicExec(ctx, entry, &request)
	if err != nil {
		return fail(code, err)
	}
	return execution.streamOutput(ctx, conn)
}

func computerRootForImage(imageRoot, mountPath string) (string, error) {
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
			"computer mount path is not a directory: %s",
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

// Instance identity is separate from the mount registry's internal key.
func (r *computerOperationRegistry) acquireCommandInstance(instanceID, computerID, token string) (*computerMountEntry, func(), bool) {
	r.mu.Lock()
	for _, entry := range r.entries {
		if !entry.retired && entry.computerInstanceID == instanceID && computerEntryMatches(entry, entry.computerInstanceID, computerID, token) {
			entry.active++
			r.mu.Unlock()
			return entry, func() { r.release(entry) }, true
		}
	}
	r.mu.Unlock()
	return nil, func() {}, false
}
