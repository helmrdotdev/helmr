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
	"sync/atomic"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/oci"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"google.golang.org/protobuf/proto"
)

type computerOperationRegistry struct {
	setWallClock     func(time.Time) error
	agentPrograms    agentProgramStore
	agentSessions    map[string]*agentRelay
	agentStarting    map[string]bool
	agentCaptureMu   sync.Mutex
	agentCapture     *agentComputerCapture
	writeback        *computerWriteback
	mu               sync.RWMutex
	entries          map[string]*computerMountEntry
	preparedRuntime  *preparedComputerRuntime
	materializations int
}

type computerMountEntry struct {
	authorityClock            atomic.Pointer[computerAuthorityClock]
	writerGeneration          int64
	channelCredential         string
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
	preparation               *computerPreparation
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
	computerID         string
	writerGeneration   int64
	computerInstanceID string
	imageRoot          string
	imageConfig        ociRuntimeConfig
	runtimeUser        *resolvedRuntimeUser
	computerMount      string
	computerRoot       string
	cleanup            func()
}

func newComputerOperationRegistry() *computerOperationRegistry {
	return &computerOperationRegistry{entries: map[string]*computerMountEntry{}, agentSessions: map[string]*agentRelay{}, agentStarting: map[string]bool{}}
}

func (r *computerOperationRegistry) setPreparedRuntime(runtime *preparedComputerRuntime) error {
	if runtime == nil {
		return nil
	}
	r.mu.Lock()
	if r.captureSealedLocked() {
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

func (r *computerOperationRegistry) takePreparedRuntime(computerInstanceID string, computerID string, computerMount string, writerGeneration uint64) (*preparedComputerRuntime, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prepared := r.preparedRuntime
	if prepared == nil || r.captureSealedLocked() {
		return nil, false
	}
	if writerGeneration == 0 || uint64(prepared.writerGeneration) != writerGeneration || prepared.computerID != computerID || prepared.computerInstanceID != computerInstanceID ||
		strings.TrimSpace(prepared.computerMount) != strings.TrimSpace(computerMount) {
		return nil, false
	}
	r.preparedRuntime = nil
	return prepared, true
}

func (r *computerOperationRegistry) register(computerInstanceID string, entry *computerMountEntry) error {
	for {
		r.mu.Lock()
		if r.captureSealedLocked() {
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
		if r.captureSealedLocked() || r.entries[computerInstanceID] != previous {
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
		subtle.ConstantTimeCompare([]byte(entry.channelCredential), []byte(token)) == 1) {
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
		subtle.ConstantTimeCompare([]byte(entry.channelCredential), []byte(token)) == 1
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

func handleComputerMaterializeConnection(_ context.Context, conn io.ReadWriter, logger *slog.Logger, registry *computerOperationRegistry) error {
	if logger == nil {
		logger = slog.Default()
	}
	totalStarted := time.Now()
	var request computerv0.MaterializeComputerRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
		return fmt.Errorf("read computer materialize request: %w", err)
	}
	releaseMutation, err := registry.reserveMaterialization()
	if err != nil {
		return err
	}
	defer releaseMutation()
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
	if strings.TrimSpace(envelope.ChannelCredential) == "" {
		return errors.New("computer materialize channel_credential is required")
	}
	if envelope.WriterGeneration == 0 {
		return errors.New("computer materialize writer_generation is required")
	}
	computerInstanceID := strings.TrimSpace(envelope.ComputerInstanceId)
	computerID := strings.TrimSpace(envelope.ComputerId)
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
	entry.channelCredential = envelope.ChannelCredential
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
		Status:                     "running",
		GuestChannelCredentialHash: sha256sum.HexBytes([]byte(strings.TrimSpace(envelope.ChannelCredential))),
		Phases:                     phases,
		Target:                     proto.Clone(request.GetTarget()).(*computerv0.ComputerMountTarget),
	})
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
	runtime, phases, err := restorePreparedComputerRuntime(&request, logger)
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

func restorePreparedComputerRuntime(request *computerv0.PrepareComputerRuntimeRequest, logger *slog.Logger) (*preparedComputerRuntime, []*computerv0.ComputerMountPhase, error) {
	var phases []*computerv0.ComputerMountPhase
	computerInstanceID := request.GetComputerInstanceId()
	if strings.TrimSpace(computerInstanceID) == "" || strings.TrimSpace(request.GetComputerId()) == "" || request.GetWriterGeneration() <= 0 {
		return nil, phases, errors.New("computer runtime prepare identity is required")
	}
	mountPath := filepath.Clean(strings.TrimSpace(request.GetMountPath()))
	if mountPath == "" || mountPath == "." || mountPath == string(filepath.Separator) || !filepath.IsAbs(mountPath) {
		return nil, phases, fmt.Errorf("computer runtime prepare mount_path %q is invalid", request.GetMountPath())
	}
	phaseStarted := time.Now()
	image, cleanupImage, err := restorePreparedComputerImage(request)
	phases = append(phases, computerMountPhase("guest_computer_image_restore", phaseStarted, 0, err))
	logger.Info("computer runtime prepare computer image restored", "computer_instance_id_hash", computerInstanceLogID(computerInstanceID), "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", 0, "error", errorText(err))
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
		computerInstanceID: computerInstanceID,
		imageRoot:          image.RootfsDir,
		imageConfig:        image.Config,
		runtimeUser:        runtimeUser,
		computerMount:      mountPath,
		computerRoot:       computerRoot,
		cleanup:            cleanup,
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
	// Materialization binds the already prepared filesystem; it does not decode
	// an image stream. Format admission belongs to the preparation path.
	prepared, ok := registry.takePreparedRuntime(computerInstanceID, envelope.GetComputerId(), mountPath, envelope.GetWriterGeneration())
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

func restorePreparedComputerImage(request *computerv0.PrepareComputerRuntimeRequest) (ociImage, func(), error) {
	cleanup := func() {}
	root := strings.TrimSpace(os.Getenv("HELMR_GUESTD_COMPUTER_ROOT"))
	if root == "" {
		return ociImage{}, cleanup, errors.New("prepared image config requires a mounted Computer")
	}
	config := request.GetMountedImageConfig()
	if config == nil {
		return ociImage{}, cleanup, errors.New("computer preparation requires admitted image config")
	}
	return ociImage{RootfsDir: root, Config: oci.RuntimeConfig{Env: config.GetEnv(), WorkingDir: config.GetWorkingDir(), User: config.GetUser(), Entrypoint: config.GetEntrypoint(), Cmd: config.GetCmd()}}, cleanup, nil
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
		envelope.ChannelCredential,
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
	if entry.authorityNow().UnixNano() >= envelope.OperationExpiresAtUnixNano {
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
