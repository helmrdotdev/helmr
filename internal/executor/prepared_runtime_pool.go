package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/frameio"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/runtime"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
)

const (
	defaultPreparedRuntimeControlTimeout = 15 * time.Second
)

type PreparedRuntimeBackpressureKind string

const (
	PreparedRuntimeBackpressureForeground PreparedRuntimeBackpressureKind = "foreground_busy"
	PreparedRuntimeBackpressureCapacity   PreparedRuntimeBackpressureKind = "capacity_busy"
)

type PreparedRuntimeBackpressureError struct {
	Kind PreparedRuntimeBackpressureKind
}

func (e *PreparedRuntimeBackpressureError) Error() string {
	switch e.Kind {
	case PreparedRuntimeBackpressureForeground:
		return "prepared runtime temporarily blocked by foreground work"
	case PreparedRuntimeBackpressureCapacity:
		return "prepared runtime local capacity is temporarily full"
	default:
		return "prepared runtime temporarily unavailable"
	}
}

func (e *PreparedRuntimeBackpressureError) Retryable() bool { return true }

var (
	errPreparedRuntimeBackgroundBusy = &PreparedRuntimeBackpressureError{Kind: PreparedRuntimeBackpressureForeground}
	errPreparedRuntimeCapacityBusy   = &PreparedRuntimeBackpressureError{Kind: PreparedRuntimeBackpressureCapacity}
)

type PreparedRuntimeInstanceClient interface {
	MarkRuntimeInstanceReady(context.Context, api.WorkerRuntimeInstanceStateRequest) (api.WorkerRuntimeInstance, error)
	MarkRuntimeInstanceClosed(context.Context, api.WorkerRuntimeInstanceStateRequest) (api.WorkerRuntimeInstance, error)
	MarkRuntimeInstanceFailed(context.Context, api.WorkerRuntimeInstanceStateRequest) (api.WorkerRuntimeInstance, error)
}

type RuntimeReconcileClient interface {
	PreparedRuntimeInstanceClient
	NextRuntimeReconcileTarget(context.Context) (api.WorkerRuntimeReconcileResponse, error)
}

type PreparedRuntimePool struct {
	Connector             vm.Connector
	CAS                   cas.Store
	TempDir               string
	ArtifactCacheDir      string
	ArtifactCacheMaxBytes int64
	Substrates            RuntimeSubstrateResolver
	RuntimeSubstrates     RuntimeSubstrateRegistrar
	CheckpointEncryptor   *checkpoint.Encryptor
	Network               compute.NetworkPolicy
	Size                  int
	RuntimeInstances      PreparedRuntimeInstanceClient
	Log                   *slog.Logger
	BackgroundGate        *BackgroundWorkGate
	AdmitRuntimeStart     func(context.Context) error

	mu           sync.Mutex
	closeMu      sync.Mutex
	entries      map[string][]preparedRuntimeEntry
	filling      map[string]int
	checkedOut   map[preparedRuntimeRef]struct{}
	ctx          context.Context
	cancel       context.CancelFunc
	activity     int
	activityWake chan struct{}
	closed       bool
}

type preparedRuntimeEntry struct {
	session           vm.Session
	poolKey           string
	runtimeInstanceID string
	runtimeEpoch      int64
	networkSlotID     string
	networkGeneration int64
	target            api.WorkerRuntimeReconcileTarget
	exit              *preparedRuntimeSignal
	ready             *preparedRuntimeSignal
}

type preparedRuntimeRef struct {
	id    string
	epoch int64
}

type preparedRuntimeSignal struct {
	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error
}

func newPreparedRuntimeSignal() *preparedRuntimeSignal {
	return &preparedRuntimeSignal{done: make(chan struct{})}
}

func (s *preparedRuntimeSignal) finish(err error) {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.done)
	})
}

func (s *preparedRuntimeSignal) wait(ctx context.Context) (error, bool) {
	if s == nil {
		return nil, true
	}
	select {
	case <-s.done:
	case <-ctx.Done():
		return ctx.Err(), false
	}
	s.mu.Lock()
	err := s.err
	s.mu.Unlock()
	return err, true
}

func (s *preparedRuntimeSignal) finished() (error, bool) {
	if s == nil {
		return nil, false
	}
	select {
	case <-s.done:
		s.mu.Lock()
		err := s.err
		s.mu.Unlock()
		return err, true
	default:
		return nil, false
	}
}

func NewPreparedRuntimePool(connector vm.Connector, store cas.Store, size int, log *slog.Logger) *PreparedRuntimePool {
	ctx, cancel := context.WithCancel(context.Background())
	return &PreparedRuntimePool{
		Connector:    connector,
		CAS:          store,
		Size:         size,
		Log:          log,
		entries:      map[string][]preparedRuntimeEntry{},
		filling:      map[string]int{},
		checkedOut:   map[preparedRuntimeRef]struct{}{},
		ctx:          ctx,
		cancel:       cancel,
		activityWake: make(chan struct{}),
	}
}

func (p *PreparedRuntimePool) Checkout(ctx context.Context, mount api.WorkerWorkspaceMount) (vm.Session, string, bool) {
	if p == nil || p.Size <= 0 {
		return nil, "", false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := preparedRuntimeKeyFromWorkspaceMount(mount, p.Network)
	keyID := runtime.ID(key)
	runtimeInstanceID := strings.TrimSpace(mount.RuntimeInstanceID)
	if runtimeInstanceID == "" {
		p.logInfo("prepared runtime pool miss", "runtime_key_id", keyID, "reason", "runtime_instance_missing")
		return nil, key, false
	}
	if mount.RuntimeEpoch <= 0 {
		p.logInfo("prepared runtime pool miss", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "reason", "runtime_epoch_missing")
		return nil, key, false
	}
	p.mu.Lock()
	entries := p.entries[key]
	if len(entries) == 0 {
		p.mu.Unlock()
		return nil, key, false
	}
	index := -1
	for i := range entries {
		if entries[i].runtimeInstanceID == runtimeInstanceID && entries[i].runtimeEpoch == mount.RuntimeEpoch {
			index = i
			break
		}
	}
	if index < 0 {
		p.mu.Unlock()
		p.logInfo("prepared runtime pool miss", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "runtime_epoch", mount.RuntimeEpoch, "reason", "reserved_session_missing")
		return nil, key, false
	}
	entry := entries[index]
	if err, exited := entry.exit.finished(); exited {
		p.mu.Unlock()
		p.removeReadyEntryAndFail(key, entry, preparedRuntimeExitCause(err), true)
		p.logInfo("prepared runtime pool miss", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "reason", "reserved_session_exited")
		return nil, key, false
	}
	if entry.networkSlotID != mount.NetworkSlotID || entry.networkGeneration != mount.NetworkSlotGeneration {
		p.mu.Unlock()
		p.logInfo("prepared runtime pool miss", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "reason", "network_slot_generation_mismatch")
		return nil, key, false
	}
	p.mu.Unlock()
	if err, readyFinished := entry.ready.wait(ctx); err != nil {
		reason := "runtime_ready_failed"
		if readyFinished {
			if p.forgetReadyEntry(key, entry) {
				p.cleanupClaimedEntryAsync(key, entry, err)
			}
		} else {
			reason = "runtime_ready_wait_canceled"
		}
		p.logInfo("prepared runtime pool miss", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "reason", reason, "error", err.Error())
		return nil, key, false
	}
	if err, exited := entry.exit.finished(); exited {
		if p.forgetReadyEntry(key, entry) {
			p.cleanupClaimedEntryAsync(key, entry, preparedRuntimeExitCause(err))
		}
		p.logInfo("prepared runtime pool miss", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "reason", "reserved_session_exited", "error", errorString(err))
		return nil, key, false
	}
	p.mu.Lock()
	entries = p.entries[key]
	index = -1
	for i := range entries {
		if entries[i].runtimeInstanceID == runtimeInstanceID && entries[i].runtimeEpoch == mount.RuntimeEpoch && entries[i].networkSlotID == mount.NetworkSlotID && entries[i].networkGeneration == mount.NetworkSlotGeneration {
			index = i
			break
		}
	}
	if index < 0 {
		p.mu.Unlock()
		p.logInfo("prepared runtime pool miss", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "runtime_epoch", mount.RuntimeEpoch, "reason", "reserved_session_claimed")
		return nil, key, false
	}
	entry = entries[index]
	if err, exited := entry.exit.finished(); exited {
		p.removeReadyEntryAtLocked(key, entries, index)
		p.mu.Unlock()
		p.cleanupClaimedEntryAsync(key, entry, preparedRuntimeExitCause(err))
		p.logInfo("prepared runtime pool miss", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "reason", "reserved_session_exited", "error", errorString(err))
		return nil, key, false
	}
	p.removeReadyEntryAtLocked(key, entries, index)
	p.markRuntimeCheckedOutLocked(runtimeInstanceID, mount.RuntimeEpoch)
	available := p.readyCountLocked()
	p.mu.Unlock()
	p.logInfo("prepared runtime pool hit", "runtime_key_id", keyID, "available", available)
	return entry.session, key, true
}

func (p *PreparedRuntimePool) ReconcileDesiredRuntimes(ctx context.Context, client RuntimeReconcileClient) error {
	if p == nil || p.Size <= 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	if client == nil {
		return errors.New("runtime reconcile client is required")
	}
	reconnectDelay := 100 * time.Millisecond
	for {
		response, err := client.NextRuntimeReconcileTarget(ctx)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			p.logInfo("runtime desired-state poll failed", "error", err.Error())
		} else if response.Target == nil {
			reconnectDelay = 100 * time.Millisecond
		} else if response.Target.Action == api.WorkerRuntimeReconcileReclaim {
			err = p.ReclaimFailedRuntimeTarget(ctx, client, *response.Target)
		} else if response.Target.Action == api.WorkerRuntimeReconcileClose || response.Target.DesiredState == "closed" {
			err = p.StopRuntimeTarget(ctx, client, *response.Target)
		} else if response.Target.Action == api.WorkerRuntimeReconcilePrepare || response.Target.DesiredState == "ready" {
			err = p.WarmRuntimeTarget(ctx, client, *response.Target)
		} else {
			err = fmt.Errorf("unsupported runtime desired state %q", response.Target.DesiredState)
		}
		if err != nil {
			p.logInfo("runtime reconciliation failed", "error", err.Error())
		}
		if err := sleepWithContext(ctx, reconnectDelay); err != nil {
			return err
		}
		if reconnectDelay < time.Second {
			reconnectDelay *= 2
		}
	}
}

func (p *PreparedRuntimePool) ReclaimFailedRuntimeTarget(ctx context.Context, client PreparedRuntimeInstanceClient, target api.WorkerRuntimeReconcileTarget) error {
	if p == nil || client == nil {
		return errors.New("failed runtime reclaim requires pool and control client")
	}
	runtimeInstanceID := strings.TrimSpace(target.ID)
	if runtimeInstanceID == "" || target.WorkerEpoch <= 0 {
		return errors.New("failed runtime reclaim target id and worker_epoch are required")
	}
	proofMethod := ""
	if _, entry, ok := p.claimReadyEntry(runtimeInstanceID, target.WorkerEpoch); ok && entry.session != nil {
		closeCtx, cancel := preparedRuntimeControlContext(ctx)
		closeErr := entry.session.Close(closeCtx)
		cancel()
		if closeErr == nil {
			proofMethod = api.WorkerRuntimeCleanupSessionClosed
		}
	}
	if proofMethod == "" {
		cleaner, ok := p.Connector.(vm.RuntimeCleanupConnector)
		if !ok {
			return errors.New("runtime connector does not support exact failed-runtime cleanup")
		}
		cleanupCtx, cancel := preparedRuntimeControlContext(ctx)
		err := cleaner.CleanupRuntime(cleanupCtx, runtimeInstanceID)
		cancel()
		if err != nil {
			return fmt.Errorf("reconcile failed runtime physical cleanup: %w", err)
		}
		proofMethod = api.WorkerRuntimeCleanupHostReconciled
	}
	request := runtimeTargetStateRequest(target, errors.New("runtime physical cleanup reconciled"))
	request.CleanupProof = &api.WorkerRuntimeCleanupProof{Method: proofMethod, CompletedAt: time.Now().UTC()}
	if _, err := client.MarkRuntimeInstanceFailed(ctx, request); err != nil {
		return fmt.Errorf("persist failed runtime cleanup proof: %w", err)
	}
	return nil
}

func (p *PreparedRuntimePool) StopRuntimeTarget(ctx context.Context, client PreparedRuntimeInstanceClient, target api.WorkerRuntimeReconcileTarget) error {
	if p == nil {
		return nil
	}
	runtimeInstanceID := strings.TrimSpace(target.ID)
	if runtimeInstanceID == "" {
		return errors.New("runtime stop target id is required")
	}
	if target.WorkerEpoch <= 0 {
		return errors.New("runtime stop target worker_epoch is required")
	}
	stoppedKey, stoppedEntry, ok := p.claimReadyEntry(runtimeInstanceID, target.WorkerEpoch)
	proofMethod := ""
	if !ok {
		if p.runtimeCheckedOut(runtimeInstanceID, target.WorkerEpoch) {
			return nil
		}
		cleaner, cleanupOK := p.Connector.(vm.RuntimeCleanupConnector)
		if !cleanupOK {
			return errors.New("runtime connector does not support exact runtime cleanup")
		}
		cleanupCtx, cancel := preparedRuntimeControlContext(ctx)
		err := cleaner.CleanupRuntime(cleanupCtx, runtimeInstanceID)
		cancel()
		if err != nil {
			return fmt.Errorf("reconcile runtime physical cleanup: %w", err)
		}
		proofMethod = api.WorkerRuntimeCleanupHostReconciled
	} else if stoppedEntry.session != nil {
		if err := stoppedEntry.session.Close(ctx); err != nil {
			return p.markRuntimeTargetFailed(ctx, client, target, err)
		}
		proofMethod = api.WorkerRuntimeCleanupSessionClosed
	} else {
		cleaner, cleanupOK := p.Connector.(vm.RuntimeCleanupConnector)
		if !cleanupOK {
			return errors.New("runtime connector does not support exact runtime cleanup")
		}
		cleanupCtx, cancel := preparedRuntimeControlContext(ctx)
		err := cleaner.CleanupRuntime(cleanupCtx, runtimeInstanceID)
		cancel()
		if err != nil {
			return fmt.Errorf("reconcile runtime physical cleanup: %w", err)
		}
		proofMethod = api.WorkerRuntimeCleanupHostReconciled
	}
	request := runtimeTargetStateRequest(target, nil)
	request.CleanupProof = &api.WorkerRuntimeCleanupProof{Method: proofMethod, CompletedAt: time.Now().UTC()}
	_, err := client.MarkRuntimeInstanceClosed(ctx, request)
	if err != nil {
		return err
	}
	p.logInfo("runtime desired close reconciled", "runtime_key_id", runtime.ID(stoppedKey), "runtime_instance_id", runtimeInstanceID)
	return nil
}

func (p *PreparedRuntimePool) PrepareRuntimeSubstrateTarget(ctx context.Context, target api.WorkerRuntimeReconcileTarget) error {
	if p == nil || p.CAS == nil || p.Substrates == nil {
		return nil
	}
	mount := preparedRuntimeWorkspaceMountFromSource(target.Source)
	if strings.TrimSpace(mount.DeploymentSandboxID) == "" {
		return errors.New("runtime substrate target source is required")
	}
	backgroundCtx, finish, ok := p.beginBackground(ctx)
	if !ok {
		p.logInfo("runtime substrate prepare skipped", "deployment_sandbox_id", mount.DeploymentSandboxID, "reason", "foreground_workspace_mount_active")
		return errPreparedRuntimeBackgroundBusy
	}
	defer finish()
	tempRoot := strings.TrimSpace(p.TempDir)
	if tempRoot == "" {
		tempRoot = os.TempDir()
	}
	if err := os.MkdirAll(tempRoot, 0o755); err != nil {
		return err
	}
	tempDir, err := os.MkdirTemp(tempRoot, "runtime-substrate-prepare-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)
	materializer := WorkspaceMaterializer{
		CAS:                   p.CAS,
		TempDir:               p.TempDir,
		ArtifactCacheDir:      p.ArtifactCacheDir,
		ArtifactCacheMaxBytes: p.ArtifactCacheMaxBytes,
	}
	_, cleanupSandboxImage, topology, err := p.restoreSandboxImageAndRuntimeSubstrate(backgroundCtx, materializer, tempDir, mount,
		"runtime substrate prepare sandbox image restored",
		"runtime substrate prepared",
		"deployment_sandbox_id", mount.DeploymentSandboxID,
	)
	if err != nil {
		return err
	}
	defer cleanupSandboxImage()
	started := time.Now()
	registered, err := runtimeCheckpointer{
		cas:               p.CAS,
		encryptor:         p.CheckpointEncryptor,
		substrateSource:   runtimeSubstrateSourceFromPreparedSource(target.Source),
		runtimeSubstrates: p.RuntimeSubstrates,
	}.ensureRuntimeSubstrate(backgroundCtx, topology.Substrate)
	p.logInfo("runtime substrate registered", "deployment_sandbox_id", mount.DeploymentSandboxID, "duration_ms", time.Since(started).Milliseconds(), "substrate_digest", runtimeSubstrateDigest(topology), "runtime_substrate_id", runtimeSubstrateID(registered), "error", errorString(err))
	return err
}

func (p *PreparedRuntimePool) WarmRuntimeTarget(ctx context.Context, client PreparedRuntimeInstanceClient, target api.WorkerRuntimeReconcileTarget) error {
	if p == nil {
		return nil
	}
	if client == nil {
		return errors.New("prepared runtime instance client is required")
	}
	if p.AdmitRuntimeStart != nil {
		if err := p.AdmitRuntimeStart(ctx); err != nil {
			return err
		}
	}
	mount := preparedRuntimeWorkspaceMountFromSource(target.Source)
	if strings.TrimSpace(mount.DeploymentSandboxID) == "" {
		return errors.New("prepared runtime warm command source is required")
	}
	key := preparedRuntimeKeyFromWorkspaceMount(mount, p.Network)
	keyID := runtime.ID(key)
	runtimeInstanceID := strings.TrimSpace(target.ID)
	runtimeEpoch := target.WorkerEpoch
	if runtimeInstanceID == "" || runtimeEpoch <= 0 {
		return errors.New("runtime reconcile target id and worker_epoch are required")
	}
	if p.Size <= 0 || p.Connector == nil || p.CAS == nil {
		reason := errors.New("prepared runtime pool is not configured")
		p.logInfo("prepared runtime warm skipped", "runtime_key_id", keyID, "reason", reason.Error())
		stateCtx, cancelState := preparedRuntimeControlContext(ctx)
		defer cancelState()
		return p.markRuntimeTargetFailedWithProof(stateCtx, client, target, reason, api.WorkerRuntimeCleanupNotMaterialized)
	}
	backgroundCtx, finish, ok := p.beginBackground(ctx)
	if !ok {
		p.logInfo("prepared runtime warm deferred", "runtime_key_id", keyID, "reason", errPreparedRuntimeBackgroundBusy.Error())
		return errPreparedRuntimeBackgroundBusy
	}
	defer finish()
	refillCtx, cancelRefill := p.withPoolContext(backgroundCtx)
	defer cancelRefill()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		reason := errors.New("prepared runtime pool closed")
		p.logInfo("prepared runtime warm skipped", "runtime_key_id", keyID, "reason", reason.Error())
		stateCtx, cancelState := preparedRuntimeControlContext(ctx)
		defer cancelState()
		return p.markRuntimeTargetFailedWithProof(stateCtx, client, target, reason, api.WorkerRuntimeCleanupNotMaterialized)
	}
	if p.reservedCountLocked() >= p.Size {
		p.mu.Unlock()
		p.logInfo("prepared runtime warm deferred", "runtime_key_id", keyID, "reason", errPreparedRuntimeCapacityBusy.Error())
		return errPreparedRuntimeCapacityBusy
	}
	p.filling[key]++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.decrementFillingLocked(key)
		p.mu.Unlock()
	}()
	return p.prepareAndStore(refillCtx, key, mount, target)
}

func (p *PreparedRuntimePool) Close(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.closeMu.Lock()
	defer p.closeMu.Unlock()
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		if p.cancel != nil {
			p.cancel()
		}
	}
	var closingEntries []preparedRuntimeEntry
	for key, keyEntries := range p.entries {
		closingEntries = append(closingEntries, keyEntries...)
		delete(p.entries, key)
	}
	p.mu.Unlock()
	var err error
	for _, entry := range closingEntries {
		if closeErr := entry.session.Close(ctx); closeErr != nil {
			transitionErr := p.transitionRuntimeTargetFailed(ctx, entry.target, closeErr)
			p.retainCloseRetry(entry)
			err = errors.Join(err, closeErr, transitionErr)
			continue
		}
		if closeErr := p.transitionRuntimeTargetFailed(ctx, entry.target, errors.New("runtime controller stopped")); closeErr != nil {
			p.retainCloseRetry(entry)
			err = errors.Join(err, closeErr)
		}
	}
	if waitErr := p.waitForActivity(ctx); waitErr != nil {
		err = errors.Join(err, waitErr)
	}
	return err
}

func (p *PreparedRuntimePool) transitionRuntimeTargetFailed(ctx context.Context, target api.WorkerRuntimeReconcileTarget, failure error) error {
	if p.RuntimeInstances == nil {
		return errors.New("prepared runtime instance client is required")
	}
	_, err := p.RuntimeInstances.MarkRuntimeInstanceFailed(ctx, runtimeTargetStateRequest(target, failure))
	return err
}

func (p *PreparedRuntimePool) retainCloseRetry(entry preparedRuntimeEntry) {
	if entry.session == nil {
		return
	}
	p.mu.Lock()
	key := entry.poolKey
	if strings.TrimSpace(key) == "" {
		key = entry.runtimeInstanceID
	}
	p.entries[key] = append(p.entries[key], entry)
	p.mu.Unlock()
}

func (p *PreparedRuntimePool) beginActivityLocked() bool {
	if p.closed {
		return false
	}
	p.activity++
	return true
}

func (p *PreparedRuntimePool) endActivity() {
	p.mu.Lock()
	if p.activity > 0 {
		p.activity--
	}
	close(p.activityWake)
	p.activityWake = make(chan struct{})
	p.mu.Unlock()
}

func (p *PreparedRuntimePool) waitForActivity(ctx context.Context) error {
	for {
		p.mu.Lock()
		remaining := p.activity
		wake := p.activityWake
		p.mu.Unlock()
		if remaining == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("prepared runtime pool close timed out with %d background tasks: %w", remaining, ctx.Err())
		case <-wake:
		}
	}
}

func (p *PreparedRuntimePool) prepareAndStore(ctx context.Context, key string, mount api.WorkerWorkspaceMount, target api.WorkerRuntimeReconcileTarget) error {
	keyID := runtime.ID(key)
	runtimeInstanceID := strings.TrimSpace(target.ID)
	runtimeEpoch := target.WorkerEpoch
	materializeAttempted := false
	failInstance := func(err error) error {
		if err == nil {
			return nil
		}
		stateCtx, cancelState := preparedRuntimeControlContext(ctx)
		defer cancelState()
		proofMethod := ""
		if !materializeAttempted {
			proofMethod = api.WorkerRuntimeCleanupNotMaterialized
		}
		if markErr := p.markRuntimeTargetFailedWithProof(stateCtx, p.RuntimeInstances, target, err, proofMethod); markErr != nil {
			p.logInfo("prepared runtime pool instance fail transition failed", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "error", markErr.Error())
			return errors.Join(err, markErr)
		}
		return nil
	}
	materializer := WorkspaceMaterializer{
		Connector:             p.Connector,
		CAS:                   p.CAS,
		TempDir:               p.TempDir,
		ArtifactCacheDir:      p.ArtifactCacheDir,
		ArtifactCacheMaxBytes: p.ArtifactCacheMaxBytes,
		Substrates:            p.Substrates,
		Network:               p.Network,
		Log:                   p.Log,
	}
	tempDir := strings.TrimSpace(p.TempDir)
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return failInstance(err)
	}
	sandboxImagePath, cleanupSandboxImage, topology, err := p.restoreSandboxImageAndRuntimeSubstrate(ctx, materializer, tempDir, mount,
		"prepared runtime pool sandbox image restored",
		"prepared runtime pool substrate resolved",
		"runtime_key_id", keyID,
	)
	if err != nil {
		return failInstance(err)
	}
	defer cleanupSandboxImage()
	var runtimeSubstrateIDValue string
	if topology.Substrate != nil {
		started := time.Now()
		registered, err := runtimeCheckpointer{
			cas:               p.CAS,
			encryptor:         p.CheckpointEncryptor,
			substrateSource:   runtimeSubstrateSourceFromWorkspaceMount(mount),
			runtimeSubstrates: p.RuntimeSubstrates,
		}.ensureRuntimeSubstrate(ctx, topology.Substrate)
		p.logInfo("prepared runtime pool substrate resolved", "runtime_key_id", keyID, "duration_ms", time.Since(started).Milliseconds(), "substrate_digest", runtimeSubstrateDigest(topology), "runtime_substrate_id", runtimeSubstrateID(registered), "error", errorString(err))
		if err != nil {
			return failInstance(err)
		}
		runtimeSubstrateIDValue = runtimeSubstrateID(registered)
	}
	connector, ok := p.Connector.(vm.MaterializingConnector)
	if !ok {
		err := errors.New("connector does not support mount")
		return failInstance(err)
	}
	started := time.Now()
	materializeAttempted = true
	session, err := connector.Materialize(ctx, vm.MaterializeRequest{
		ID:                 runtimeInstanceID,
		OwnerKind:          vm.RuntimeOwnerRuntime,
		RootfsDigest:       mount.RootfsDigest,
		ImageDigest:        mount.ImageDigest,
		ImageFormat:        mount.ImageFormat,
		WorkspaceMountPath: mount.WorkspaceMountPath,
		Resources: compute.ResourceVector{
			MilliCPU:  mount.RequestedMilliCPU,
			MemoryMiB: mount.RequestedMemoryMiB,
			DiskMiB:   mount.RequestedDiskMiB,
			Slots:     mount.RequestedExecutionSlots,
		},
		Network:  p.Network,
		Topology: topology,
	})
	p.logInfo("prepared runtime pool session materialized", "runtime_key_id", keyID, "duration_ms", time.Since(started).Milliseconds(), "error", errorString(err))
	if err != nil {
		return failInstance(err)
	}
	keepSession := false
	defer func() {
		if !keepSession {
			p.closeSession(ctx, session)
		}
	}()
	if err := p.prepareGuestRuntime(ctx, session, key, mount, sandboxImagePath); err != nil {
		p.logInfo("prepared runtime pool guest prepare failed", "runtime_key_id", keyID, "error", err.Error())
		return failInstance(err)
	}
	entry := preparedRuntimeEntry{
		session:           session,
		poolKey:           key,
		runtimeInstanceID: runtimeInstanceID,
		runtimeEpoch:      runtimeEpoch,
		networkSlotID:     target.NetworkSlotID,
		networkGeneration: target.NetworkSlotGeneration,
		target:            target,
		exit:              newPreparedRuntimeSignal(),
		ready:             newPreparedRuntimeSignal(),
	}
	p.mu.Lock()
	closed := p.closed
	capacityBusy := !closed && p.readyCountLocked() >= p.Size
	if closed || capacityBusy {
		p.mu.Unlock()
		if capacityBusy {
			closeCtx, cancelClose := preparedRuntimeControlContext(ctx)
			closeErr := session.Close(closeCtx)
			cancelClose()
			if closeErr != nil {
				return failInstance(closeErr)
			}
			keepSession = true
			p.logInfo("prepared runtime warm deferred", "runtime_key_id", keyID, "reason", errPreparedRuntimeCapacityBusy.Error())
			return errPreparedRuntimeCapacityBusy
		}
		stateCtx, cancelState := preparedRuntimeControlContext(ctx)
		defer cancelState()
		if err := p.markRuntimeTargetFailed(stateCtx, p.RuntimeInstances, target, errors.New("runtime pool capacity changed")); err != nil {
			p.logInfo("prepared runtime pool instance close transition failed", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "error", err.Error())
			return err
		}
		return nil
	}
	p.entries[key] = append(p.entries[key], entry)
	p.monitorReadyEntryLocked(key, entry)
	p.mu.Unlock()
	keepSession = true
	if err, exited := entry.exit.finished(); exited {
		entry.ready.finish(preparedRuntimeExitCause(err))
		if failErr := p.removeReadyEntryAndFail(key, entry, preparedRuntimeExitCause(err), true); failErr != nil {
			return errors.Join(preparedRuntimeExitCause(err), failErr)
		}
		return nil
	}
	readyRequest := runtimeTargetStateRequest(target, nil)
	readyRequest.RuntimeSubstrateID = runtimeSubstrateIDValue
	networkSession, ok := session.(vm.NetworkFactSession)
	if !ok {
		err := errors.New("materialized runtime does not expose CNI network facts")
		entry.ready.finish(err)
		if failErr := p.removeReadyEntryAndFail(key, entry, err, true); failErr != nil {
			return errors.Join(err, failErr)
		}
		return nil
	}
	networkFacts, err := networkSession.NetworkFacts()
	if err != nil {
		entry.ready.finish(err)
		if failErr := p.removeReadyEntryAndFail(key, entry, err, true); failErr != nil {
			return errors.Join(err, failErr)
		}
		return nil
	}
	readyRequest.NetworkFacts = &api.WorkerNetworkFacts{
		HostInterfaceName: networkFacts.HostInterfaceName,
		GuestAddress:      networkFacts.GuestAddress,
		GatewayAddress:    networkFacts.GatewayAddress,
		Subnet:            networkFacts.Subnet,
		TapName:           networkFacts.TapName,
		NetNSName:         networkFacts.NetNSName,
		GuestMAC:          networkFacts.GuestMAC,
	}
	if _, err := p.RuntimeInstances.MarkRuntimeInstanceReady(ctx, readyRequest); err != nil {
		entry.ready.finish(err)
		p.logInfo("prepared runtime pool instance ready transition failed", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "error", err.Error())
		if failErr := p.removeReadyEntryAndFail(key, entry, err, true); failErr != nil {
			return errors.Join(err, failErr)
		}
		return nil
	}
	entry.ready.finish(nil)

	p.mu.Lock()
	available := p.readyCountLocked()
	stillReady := false
	for _, candidate := range p.entries[key] {
		if candidate.runtimeInstanceID == entry.runtimeInstanceID && candidate.runtimeEpoch == entry.runtimeEpoch && candidate.networkSlotID == entry.networkSlotID && candidate.networkGeneration == entry.networkGeneration {
			stillReady = true
			break
		}
	}
	p.mu.Unlock()
	if !stillReady {
		return nil
	}
	p.logInfo("prepared runtime pool refilled", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "available", available)
	return nil
}

func (p *PreparedRuntimePool) restoreSandboxImageAndRuntimeSubstrate(ctx context.Context, materializer WorkspaceMaterializer, tempDir string, mount api.WorkerWorkspaceMount, sandboxLogMessage string, substrateLogMessage string, logAttrs ...any) (string, func(), vm.RuntimeTopology, error) {
	started := time.Now()
	sandboxImagePath, cleanupSandboxImage, err := materializer.restoreCASObject(ctx, tempDir, "sandbox-image", mount.SandboxImageArtifact)
	p.logInfo(sandboxLogMessage, append(append([]any{}, logAttrs...),
		"duration_ms", time.Since(started).Milliseconds(),
		"size_bytes", mount.SandboxImageArtifact.SizeBytes,
		"error", errorString(err),
	)...)
	if err != nil {
		cleanupSandboxImage()
		return "", func() {}, vm.RuntimeTopology{}, err
	}
	started = time.Now()
	topology, err := runtimeSubstrateTopology(ctx, p.Substrates, sandboxImagePath, mount)
	p.logInfo(substrateLogMessage, append(append([]any{}, logAttrs...),
		"duration_ms", time.Since(started).Milliseconds(),
		"substrate_digest", runtimeSubstrateDigest(topology),
		"error", errorString(err),
	)...)
	if err != nil {
		cleanupSandboxImage()
		return "", func() {}, vm.RuntimeTopology{}, err
	}
	return sandboxImagePath, cleanupSandboxImage, topology, nil
}

func (p *PreparedRuntimePool) monitorReadyEntryLocked(key string, entry preparedRuntimeEntry) {
	if p == nil || p.closed || entry.session == nil || entry.exit == nil {
		return
	}
	if !p.beginActivityLocked() {
		return
	}
	go func() {
		defer p.endActivity()
		err := entry.session.Wait(p.ctx)
		entry.exit.finish(err)
		entry.ready.finish(preparedRuntimeExitCause(err))
		if p.ctx != nil && p.ctx.Err() != nil && errors.Is(err, context.Canceled) {
			return
		}
		p.removeReadyEntryAndFail(key, entry, preparedRuntimeExitCause(err), false)
	}()
}

func preparedRuntimeExitCause(err error) error {
	if err != nil {
		return err
	}
	return errors.New("prepared runtime session exited")
}

func (p *PreparedRuntimePool) readyCountLocked() int {
	total := 0
	for _, entries := range p.entries {
		total += len(entries)
	}
	return total
}

func (p *PreparedRuntimePool) fillingCountLocked() int {
	total := 0
	for _, count := range p.filling {
		total += count
	}
	return total
}

func (p *PreparedRuntimePool) decrementFillingLocked(key string) {
	count := p.filling[key] - 1
	if count <= 0 {
		delete(p.filling, key)
		return
	}
	p.filling[key] = count
}

func (p *PreparedRuntimePool) reservedCountLocked() int {
	return p.readyCountLocked() + p.fillingCountLocked() + len(p.checkedOut)
}

func (p *PreparedRuntimePool) markRuntimeCheckedOutLocked(runtimeInstanceID string, runtimeEpoch int64) {
	if p.checkedOut == nil {
		p.checkedOut = map[preparedRuntimeRef]struct{}{}
	}
	p.checkedOut[preparedRuntimeRef{id: runtimeInstanceID, epoch: runtimeEpoch}] = struct{}{}
}

func (p *PreparedRuntimePool) runtimeCheckedOut(runtimeInstanceID string, runtimeEpoch int64) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.checkedOut[preparedRuntimeRef{id: runtimeInstanceID, epoch: runtimeEpoch}]
	return ok
}

func (p *PreparedRuntimePool) ReleaseCheckout(runtimeInstanceID string, runtimeEpoch int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	delete(p.checkedOut, preparedRuntimeRef{id: strings.TrimSpace(runtimeInstanceID), epoch: runtimeEpoch})
	p.mu.Unlock()
}

func (p *PreparedRuntimePool) removeReadyEntryAndFail(key string, entry preparedRuntimeEntry, cause error, closeSession bool) error {
	keyID := runtime.ID(key)
	if !p.forgetReadyEntry(key, entry) {
		return nil
	}
	if closeSession && entry.session != nil {
		p.closeSession(context.Background(), entry.session)
	}
	stateCtx, cancelState := preparedRuntimeControlContext(context.Background())
	defer cancelState()
	if err := p.markRuntimeTargetFailed(stateCtx, p.RuntimeInstances, entry.target, cause); err != nil {
		p.logInfo("prepared runtime pool instance fail transition failed", "runtime_key_id", keyID, "runtime_instance_id", entry.runtimeInstanceID, "error", err.Error())
		return err
	}
	p.logInfo("prepared runtime pool entry failed", "runtime_key_id", keyID, "runtime_instance_id", entry.runtimeInstanceID, "error", errorString(cause))
	return nil
}

func (p *PreparedRuntimePool) cleanupClaimedEntryAsync(key string, entry preparedRuntimeEntry, cause error) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if !p.beginActivityLocked() {
		p.mu.Unlock()
		p.cleanupClaimedEntry(key, entry, cause)
		return
	}
	p.mu.Unlock()
	go func() {
		defer p.endActivity()
		p.cleanupClaimedEntry(key, entry, cause)
	}()
}

func (p *PreparedRuntimePool) cleanupClaimedEntry(key string, entry preparedRuntimeEntry, cause error) {
	keyID := runtime.ID(key)
	if entry.session != nil {
		p.closeSession(context.Background(), entry.session)
	}
	stateCtx, cancelState := preparedRuntimeControlContext(context.Background())
	defer cancelState()
	if err := p.markRuntimeTargetFailed(stateCtx, p.RuntimeInstances, entry.target, cause); err != nil {
		p.logInfo("prepared runtime pool instance fail transition failed", "runtime_key_id", keyID, "runtime_instance_id", entry.runtimeInstanceID, "error", err.Error())
		return
	}
	p.logInfo("prepared runtime pool claimed entry failed", "runtime_key_id", keyID, "runtime_instance_id", entry.runtimeInstanceID, "error", errorString(cause))
}

func (p *PreparedRuntimePool) forgetReadyEntry(key string, entry preparedRuntimeEntry) bool {
	removed := false
	p.mu.Lock()
	entries := p.entries[key]
	for i := range entries {
		if entries[i].runtimeInstanceID == entry.runtimeInstanceID && entries[i].runtimeEpoch == entry.runtimeEpoch && entries[i].networkSlotID == entry.networkSlotID && entries[i].networkGeneration == entry.networkGeneration {
			p.removeReadyEntryAtLocked(key, entries, i)
			removed = true
			break
		}
	}
	p.mu.Unlock()
	return removed
}

func (p *PreparedRuntimePool) claimReadyEntry(runtimeInstanceID string, runtimeEpoch int64) (string, preparedRuntimeEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, entries := range p.entries {
		for i, entry := range entries {
			if entry.runtimeInstanceID != runtimeInstanceID || entry.runtimeEpoch != runtimeEpoch {
				continue
			}
			p.removeReadyEntryAtLocked(key, entries, i)
			return key, entry, true
		}
	}
	return "", preparedRuntimeEntry{}, false
}

func (p *PreparedRuntimePool) removeReadyEntryAtLocked(key string, entries []preparedRuntimeEntry, index int) {
	entries = append(entries[:index], entries[index+1:]...)
	if len(entries) == 0 {
		delete(p.entries, key)
		return
	}
	p.entries[key] = entries
}

func preparedRuntimeWorkspaceMountFromSource(source api.WorkerPreparedRuntimeSource) api.WorkerWorkspaceMount {
	return api.WorkerWorkspaceMount{
		ID:                         uuid.Must(uuid.NewV7()).String(),
		WorkspaceID:                uuid.Must(uuid.NewV7()).String(),
		DeploymentSandboxID:        strings.TrimSpace(source.DeploymentSandboxID),
		RuntimeID:                  strings.TrimSpace(source.RuntimeID),
		SandboxImageArtifact:       source.SandboxImageArtifact,
		SandboxImageArtifactFormat: strings.TrimSpace(source.SandboxImageArtifactFormat),
		RootfsDigest:               strings.TrimSpace(source.RootfsDigest),
		ImageDigest:                strings.TrimSpace(source.ImageDigest),
		ImageFormat:                strings.TrimSpace(source.ImageFormat),
		WorkspaceMountPath:         strings.TrimSpace(source.WorkspaceMountPath),
		RequestedMilliCPU:          int64(source.ReservedCpuMillis),
		RequestedMemoryMiB:         int64(source.ReservedMemoryMiB),
		RequestedDiskMiB:           source.ReservedDiskMiB,
		RequestedExecutionSlots:    source.ReservedExecutionSlots,
		RuntimeABI:                 strings.TrimSpace(source.RuntimeABI),
		GuestdABI:                  strings.TrimSpace(source.GuestdABI),
		AdapterABI:                 strings.TrimSpace(source.AdapterABI),
	}
}

func (p *PreparedRuntimePool) prepareGuestRuntime(ctx context.Context, session vm.Session, key string, mount api.WorkerWorkspaceMount, sandboxImagePath string) error {
	keyID := runtime.ID(key)
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return fmt.Errorf("open prepared runtime stream: %w", err)
	}
	defer stream.Close()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:        wire.StreamTypeWorkspaceRuntimePrepare,
		WorkspaceID: mount.WorkspaceID,
	}, 0); err != nil {
		return fmt.Errorf("write prepared runtime header: %w", err)
	}
	request := &workspacev0.PrepareWorkspaceRuntimeRequest{
		RuntimeKey: key,
		MountPath:  strings.TrimSpace(mount.WorkspaceMountPath),
		SandboxArtifact: &workspacev0.WorkspaceArtifact{
			Digest:    strings.TrimSpace(mount.SandboxImageArtifact.Digest),
			MediaType: strings.TrimSpace(mount.SandboxImageArtifact.MediaType),
			Encoding:  strings.TrimSpace(mount.SandboxImageArtifactFormat),
			SizeBytes: uint64(mount.SandboxImageArtifact.SizeBytes),
		},
	}
	if err := frameio.WriteProtoFrame(stream, request); err != nil {
		return fmt.Errorf("write prepared runtime request: %w", err)
	}
	started := time.Now()
	if err := writeFileFrameWithMetadataContext(ctx, session, stream, wire.StreamHeader{
		Type:        wire.StreamTypeRunImage,
		WorkspaceID: mount.WorkspaceID,
	}, sandboxImagePath, strings.TrimSpace(mount.SandboxImageArtifact.Digest), mount.SandboxImageArtifact.SizeBytes); err != nil {
		// The guest can reject the request while the host is still streaming a
		// large image. Preserve the guest's structured failure instead of
		// reporting only the resulting broken pipe.
		responseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var response workspacev0.PrepareWorkspaceRuntimeResponse
		if responseErr := readProtoFrameFromReaderContext(responseCtx, session, stream, &response); responseErr == nil {
			if phaseError := workspaceMountPhaseError(response.GetPhases()); phaseError != "" {
				return fmt.Errorf("prepared runtime rejected sandbox image: %s: %w", phaseError, err)
			}
			return fmt.Errorf("prepared runtime returned state %q while writing sandbox image: %w", response.GetState(), err)
		}
		return fmt.Errorf("write prepared runtime sandbox image: %w", err)
	}
	p.logInfo("prepared runtime pool sandbox image sent", "runtime_key_id", keyID, "duration_ms", time.Since(started).Milliseconds(), "size_bytes", mount.SandboxImageArtifact.SizeBytes)
	var response workspacev0.PrepareWorkspaceRuntimeResponse
	started = time.Now()
	if err := readProtoFrameFromReaderContext(ctx, session, stream, &response); err != nil {
		return fmt.Errorf("read prepared runtime response: %w", err)
	}
	p.logInfo("prepared runtime pool response read", "runtime_key_id", keyID, "duration_ms", time.Since(started).Milliseconds(), "state", strings.TrimSpace(response.State))
	for _, guestPhase := range response.GetPhases() {
		if guestPhase == nil {
			continue
		}
		p.logInfo("prepared runtime pool guest phase",
			"runtime_key_id", keyID,
			"guest_phase", strings.TrimSpace(guestPhase.GetName()),
			"duration_ms", guestPhase.GetDurationMs(),
			"size_bytes", guestPhase.GetSizeBytes(),
			"entry_count", guestPhase.GetEntryCount(),
			"error", strings.TrimSpace(guestPhase.GetError()),
		)
	}
	if response.State != "prepared" {
		if phaseError := workspaceMountPhaseError(response.GetPhases()); phaseError != "" {
			return fmt.Errorf("prepared runtime returned state %q: %s", response.State, phaseError)
		}
		return fmt.Errorf("prepared runtime returned state %q", response.State)
	}
	if strings.TrimSpace(response.RuntimeKey) != key {
		return errors.New("prepared runtime key mismatch")
	}
	return nil
}

func (p *PreparedRuntimePool) beginBackground(ctx context.Context) (context.Context, func(), bool) {
	if p == nil || p.BackgroundGate == nil {
		return ctx, func() {}, true
	}
	return p.BackgroundGate.BeginBackground(ctx)
}

func (p *PreparedRuntimePool) withPoolContext(parent context.Context) (context.Context, context.CancelFunc) {
	if p == nil || p.ctx == nil {
		return context.WithCancel(parent)
	}
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-p.ctx.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func preparedRuntimeControlContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	} else {
		parent = context.WithoutCancel(parent)
	}
	return context.WithTimeout(parent, defaultPreparedRuntimeControlTimeout)
}

func (p *PreparedRuntimePool) closeSession(parent context.Context, session vm.Session) {
	if session == nil {
		return
	}
	ctx, cancel := preparedRuntimeControlContext(parent)
	defer cancel()
	_ = session.Close(ctx)
}

func runtimeTargetStateRequest(target api.WorkerRuntimeReconcileTarget, failure error) api.WorkerRuntimeInstanceStateRequest {
	request := api.WorkerRuntimeInstanceStateRequest{
		ID: target.ID, WorkerEpoch: target.WorkerEpoch, NetworkSlotID: target.NetworkSlotID,
		NetworkSlotGeneration: target.NetworkSlotGeneration, DesiredVersion: target.DesiredVersion,
		ExpectedObservedVersion: target.ObservedVersion, ReasonCode: "desired_state_reconciled",
	}
	if target.Source.RuntimeSubstrate != nil {
		request.RuntimeSubstrateID = target.Source.RuntimeSubstrate.ID
	}
	if failure != nil {
		request.ReasonCode = "runtime_reconcile_failed"
		request.Error, _ = json.Marshal(map[string]string{"message": failure.Error()})
	}
	return request
}

func (p *PreparedRuntimePool) markRuntimeTargetFailed(ctx context.Context, client PreparedRuntimeInstanceClient, target api.WorkerRuntimeReconcileTarget, failure error) error {
	return p.markRuntimeTargetFailedWithProof(ctx, client, target, failure, "")
}

func (p *PreparedRuntimePool) markRuntimeTargetFailedWithProof(ctx context.Context, client PreparedRuntimeInstanceClient, target api.WorkerRuntimeReconcileTarget, failure error, proofMethod string) error {
	request := runtimeTargetStateRequest(target, failure)
	if proofMethod != "" {
		request.CleanupProof = &api.WorkerRuntimeCleanupProof{Method: proofMethod, CompletedAt: time.Now().UTC()}
	}
	_, err := client.MarkRuntimeInstanceFailed(ctx, request)
	if err != nil {
		return errors.Join(failure, err)
	}
	return failure
}

func writeFileFrameWithMetadataContext(ctx context.Context, session vm.Session, w io.Writer, header wire.StreamHeader, path string, digest string, size int64) error {
	header.BodyDigest = &digest
	if err := wire.WriteStreamFrameHeader(w, header, uint64(size)); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	result := make(chan error, 1)
	go func() {
		_, err := io.Copy(w, file)
		result <- err
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		closeCtx, cancel := preparedRuntimeControlContext(ctx)
		defer cancel()
		_ = session.Close(closeCtx)
		return ctx.Err()
	}
}

func (p *PreparedRuntimePool) logInfo(message string, attrs ...any) {
	if p == nil || p.Log == nil {
		return
	}
	p.Log.Info(message, attrs...)
}
