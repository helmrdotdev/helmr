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
	"uuid"

	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/frameio"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const (
	defaultPreparedRuntimeControlTimeout = 15 * time.Second
)

var errPreparedRuntimeCapacityBusy = errors.New("prepared runtime local capacity is temporarily full")

type PreparedRuntimeInstanceClient interface {
	MarkRuntimeInstanceReady(context.Context, workerapi.RuntimeInstanceStateRequest) (workerapi.RuntimeInstance, error)
	MarkRuntimeInstanceClosed(context.Context, workerapi.RuntimeInstanceStateRequest) (workerapi.RuntimeInstance, error)
	MarkRuntimeInstanceFailed(context.Context, workerapi.RuntimeInstanceStateRequest) (workerapi.RuntimeInstance, error)
}

type RuntimeReconcileClient interface {
	PreparedRuntimeInstanceClient
	ListRuntimeReconcileTargets(context.Context) (workerapi.RuntimeReconcileResponse, error)
}

type runtimeReconcileResult struct {
	ref preparedRuntimeRef
	err error
}

type PreparedRuntimePool struct {
	Connector             vm.Cleaner
	CAS                   cas.Store
	TempDir               string
	ArtifactCacheDir      string
	ArtifactCacheMaxBytes int64
	Substrates            RuntimeSubstrateResolver
	RuntimeSubstrates     RuntimeSubstrateRegistrar
	CheckpointEncryptor   *checkpoint.Encryptor
	Size                  int
	RuntimeInstances      PreparedRuntimeInstanceClient
	Log                   *slog.Logger
	AdmitRuntimeStart     func(context.Context) error
	Capacity              *capacity.Ledger
	PlatformStore         cas.Reader
	RuntimeArchitecture   deployment.RuntimeArchitecture
	VerifierCgroupRoot    string

	mu                sync.Mutex
	closeMu           sync.Mutex
	entries           map[string][]preparedRuntimeEntry
	filling           map[string]int
	checkedOut        map[preparedRuntimeRef]struct{}
	checkedOutRestore map[preparedRuntimeRef]string
	ctx               context.Context
	cancel            context.CancelFunc
	activity          int
	activityWake      chan struct{}
	closed            bool
	verifiedRuntimes  map[deployment.RuntimeDescriptor]deployment.RuntimeIndex
}

type preparedRuntimeEntry struct {
	session           vm.Session
	poolKey           string
	runtimeInstanceID string
	runtimeEpoch      int64
	target            workerapi.RuntimeReconcileTarget
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

func NewPreparedRuntimePool(connector vm.Cleaner, store cas.Store, size int, log *slog.Logger) *PreparedRuntimePool {
	ctx, cancel := context.WithCancel(context.Background())
	return &PreparedRuntimePool{
		Connector:         connector,
		CAS:               store,
		Size:              size,
		Log:               log,
		entries:           map[string][]preparedRuntimeEntry{},
		filling:           map[string]int{},
		checkedOut:        map[preparedRuntimeRef]struct{}{},
		checkedOutRestore: map[preparedRuntimeRef]string{},
		ctx:               ctx,
		cancel:            cancel,
		activityWake:      make(chan struct{}),
	}
}

func (p *PreparedRuntimePool) Checkout(ctx context.Context, mount workerapi.WorkspaceMount) (vm.Session, string, bool) {
	if p == nil || p.Size <= 0 {
		return nil, "", false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := runtimeInstanceIDFromWorkspaceMount(mount)
	runtimeInstanceID := strings.TrimSpace(mount.RuntimeInstanceID)
	if runtimeInstanceID == "" {
		p.logInfo("prepared runtime pool miss", "reason", "runtime_instance_missing")
		return nil, key, false
	}
	if mount.RuntimeEpoch <= 0 {
		p.logInfo("prepared runtime pool miss", "runtime_instance_id", runtimeInstanceID, "reason", "runtime_epoch_missing")
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
		p.logInfo("prepared runtime pool miss", "runtime_instance_id", runtimeInstanceID, "runtime_epoch", mount.RuntimeEpoch, "reason", "reserved_session_missing")
		return nil, key, false
	}
	entry := entries[index]
	if err, exited := entry.exit.finished(); exited {
		p.mu.Unlock()
		p.removeReadyEntryAndFail(key, entry, preparedRuntimeExitCause(err), true)
		p.logInfo("prepared runtime pool miss", "runtime_instance_id", runtimeInstanceID, "reason", "reserved_session_exited")
		return nil, key, false
	}
	p.mu.Unlock()
	if err, readyFinished := entry.ready.wait(ctx); err != nil {
		reason := "runtime_ready_failed"
		if readyFinished {
			if p.forgetReadyEntry(key, entry) {
				p.cleanupClaimedEntryAsync(entry, err)
			}
		} else {
			reason = "runtime_ready_wait_canceled"
		}
		p.logInfo("prepared runtime pool miss", "runtime_instance_id", runtimeInstanceID, "reason", reason, "error", err.Error())
		return nil, key, false
	}
	if err, exited := entry.exit.finished(); exited {
		if p.forgetReadyEntry(key, entry) {
			p.cleanupClaimedEntryAsync(entry, preparedRuntimeExitCause(err))
		}
		p.logInfo("prepared runtime pool miss", "runtime_instance_id", runtimeInstanceID, "reason", "reserved_session_exited", "error", errorString(err))
		return nil, key, false
	}
	p.mu.Lock()
	entries = p.entries[key]
	index = -1
	for i := range entries {
		if entries[i].runtimeInstanceID == runtimeInstanceID && entries[i].runtimeEpoch == mount.RuntimeEpoch {
			index = i
			break
		}
	}
	if index < 0 {
		p.mu.Unlock()
		p.logInfo("prepared runtime pool miss", "runtime_instance_id", runtimeInstanceID, "runtime_epoch", mount.RuntimeEpoch, "reason", "reserved_session_claimed")
		return nil, key, false
	}
	entry = entries[index]
	if err, exited := entry.exit.finished(); exited {
		p.removeReadyEntryAtLocked(key, entries, index)
		p.mu.Unlock()
		p.cleanupClaimedEntryAsync(entry, preparedRuntimeExitCause(err))
		p.logInfo("prepared runtime pool miss", "runtime_instance_id", runtimeInstanceID, "reason", "reserved_session_exited", "error", errorString(err))
		return nil, key, false
	}
	p.removeReadyEntryAtLocked(key, entries, index)
	p.markRuntimeCheckedOutLocked(runtimeInstanceID, mount.RuntimeEpoch)
	if entry.target.Source.Restore != nil {
		p.checkedOutRestore[preparedRuntimeRef{id: runtimeInstanceID, epoch: mount.RuntimeEpoch}] =
			strings.TrimSpace(entry.target.Source.Restore.CheckpointID)
	}
	available := p.readyCountLocked()
	p.mu.Unlock()
	p.logInfo("prepared runtime pool hit", "runtime_instance_id", runtimeInstanceID, "available", available)
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
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	active := make(map[preparedRuntimeRef]struct{}, p.Size)
	decisions := make(chan bool, p.Size)
	results := make(chan runtimeReconcileResult, p.Size)
	var attempts sync.WaitGroup
	stop := func(err error) error {
		cancel()
		attempts.Wait()
		return err
	}
	handleResult := func(result runtimeReconcileResult) error {
		delete(active, result.ref)
		if result.err == nil {
			return nil
		}
		if diagnostic, ok := deployment.VerifierLocalDiagnostic(result.err); ok {
			p.logInfo("artifact verifier bootstrap failed", "diagnostic", diagnostic)
		}
		var fatal interface{ FatalWorker() bool }
		if errors.As(result.err, &fatal) && fatal.FatalWorker() {
			return result.err
		}
		p.logInfo("runtime reconciliation failed", "error", result.err.Error())
		return nil
	}
	for {
		for {
			select {
			case result := <-results:
				if err := handleResult(result); err != nil {
					return stop(err)
				}
			default:
				goto drained
			}
		}
	drained:
		if len(active) >= p.Size {
			select {
			case result := <-results:
				if err := handleResult(result); err != nil {
					return stop(err)
				}
				continue
			case <-ctx.Done():
				return stop(ctx.Err())
			}
		}

		response, err := client.ListRuntimeReconcileTargets(workCtx)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stop(ctxErr)
		}
		delay := time.Second
		if err != nil {
			p.logInfo("runtime desired-state poll failed", "error", err.Error())
		} else {
			launched := 0
			for _, target := range response.Items {
				if len(active) >= p.Size {
					break
				}
				ref := preparedRuntimeRef{id: strings.TrimSpace(target.ID), epoch: target.WorkerEpoch}
				if _, ok := active[ref]; ok {
					continue
				}
				active[ref] = struct{}{}
				launched++
				attempts.Go(func() {
					admitted := false
					err := p.reconcileRuntimeTarget(workCtx, client, target, func() {
						admitted = true
						decisions <- true
					})
					if !admitted {
						decisions <- false
					}
					results <- runtimeReconcileResult{ref: ref, err: err}
				})
			}
			productive := false
			for decided := 0; decided < launched; {
				select {
				case admitted := <-decisions:
					productive = productive || admitted
					decided++
				case result := <-results:
					if err := handleResult(result); err != nil {
						return stop(err)
					}
				case <-ctx.Done():
					return stop(ctx.Err())
				}
			}
			if productive {
				delay = 100 * time.Millisecond
			}
		}
		if err := sleepWithContext(ctx, delay); err != nil {
			return stop(err)
		}
	}
}

func (p *PreparedRuntimePool) reconcileRuntimeTarget(
	ctx context.Context,
	client PreparedRuntimeInstanceClient,
	target workerapi.RuntimeReconcileTarget,
	admitted func(),
) error {
	switch {
	case target.Action == workerapi.RuntimeReconcileReclaim:
		admitted()
		return p.ReclaimFailedRuntimeTarget(ctx, client, target)
	case target.Action == workerapi.RuntimeReconcileClose:
		admitted()
		return p.StopRuntimeTarget(ctx, client, target)
	case target.Action == workerapi.RuntimeReconcilePrepare:
		return p.warmRuntimeTarget(ctx, client, target, admitted)
	default:
		return fmt.Errorf("unsupported runtime reconcile action %q", target.Action)
	}
}

func (p *PreparedRuntimePool) ReclaimFailedRuntimeTarget(ctx context.Context, client PreparedRuntimeInstanceClient, target workerapi.RuntimeReconcileTarget) error {
	if p == nil || client == nil {
		return errors.New("failed runtime reclaim requires pool and control plane client")
	}
	runtimeInstanceID := strings.TrimSpace(target.ID)
	if runtimeInstanceID == "" || target.WorkerEpoch <= 0 {
		return errors.New("failed runtime reclaim target id and worker_epoch are required")
	}
	if p.Connector == nil {
		return errors.New("runtime connector does not support exact failed-runtime cleanup")
	}
	entry, ready := p.claimReadyEntry(runtimeInstanceID, target.WorkerEpoch)
	var closeErr error
	if ready && entry.session != nil {
		closeCtx, cancel := preparedRuntimeControlContext(ctx)
		closeErr = entry.session.Close(closeCtx)
		cancel()
	}
	cleanupCtx, cancel := preparedRuntimeControlContext(ctx)
	err := p.Connector.Cleanup(cleanupCtx, vm.Owner{Kind: vm.OwnerRuntime, ID: runtimeInstanceID})
	cancel()
	if err != nil {
		return fmt.Errorf("reconcile failed runtime physical cleanup: %w", errors.Join(closeErr, err))
	}
	if err := p.releaseRuntimeAfterPhysicalCleanup(runtimeInstanceID, target.WorkerEpoch); err != nil {
		return err
	}
	request := runtimeTargetStateRequest(target, errors.New("runtime physical cleanup reconciled"))
	request.CleanupProof = &workerapi.RuntimeCleanupProof{Method: workerapi.RuntimeCleanupHostReconciled, CompletedAt: time.Now().UTC()}
	if _, err := client.MarkRuntimeInstanceFailed(ctx, request); err != nil {
		return fmt.Errorf("persist failed runtime cleanup proof: %w", err)
	}
	return nil
}

func (p *PreparedRuntimePool) StopRuntimeTarget(ctx context.Context, client PreparedRuntimeInstanceClient, target workerapi.RuntimeReconcileTarget) error {
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
	stoppedEntry, ok := p.claimReadyEntry(runtimeInstanceID, target.WorkerEpoch)
	proofMethod := ""
	if !ok {
		if p.runtimeCheckedOut(runtimeInstanceID, target.WorkerEpoch) {
			return nil
		}
		if p.Connector == nil {
			return errors.New("runtime connector does not support exact runtime cleanup")
		}
		cleanupCtx, cancel := preparedRuntimeControlContext(ctx)
		err := p.Connector.Cleanup(cleanupCtx, vm.Owner{Kind: vm.OwnerRuntime, ID: runtimeInstanceID})
		cancel()
		if err != nil {
			return fmt.Errorf("reconcile runtime physical cleanup: %w", err)
		}
		if err := p.releaseRuntimeCapacity(runtimeInstanceID, target.WorkerEpoch); err != nil {
			return err
		}
		proofMethod = workerapi.RuntimeCleanupHostReconciled
	} else if stoppedEntry.session != nil {
		if err := stoppedEntry.session.Close(ctx); err != nil {
			return p.markRuntimeTargetFailed(ctx, client, target, err)
		}
		if err := p.releaseRuntimeCapacity(runtimeInstanceID, target.WorkerEpoch); err != nil {
			return err
		}
		proofMethod = workerapi.RuntimeCleanupSessionClosed
	} else {
		if p.Connector == nil {
			return errors.New("runtime connector does not support exact runtime cleanup")
		}
		cleanupCtx, cancel := preparedRuntimeControlContext(ctx)
		err := p.Connector.Cleanup(cleanupCtx, vm.Owner{Kind: vm.OwnerRuntime, ID: runtimeInstanceID})
		cancel()
		if err != nil {
			return fmt.Errorf("reconcile runtime physical cleanup: %w", err)
		}
		if err := p.releaseRuntimeCapacity(runtimeInstanceID, target.WorkerEpoch); err != nil {
			return err
		}
		proofMethod = workerapi.RuntimeCleanupHostReconciled
	}
	request := runtimeTargetStateRequest(target, nil)
	request.CleanupProof = &workerapi.RuntimeCleanupProof{Method: proofMethod, CompletedAt: time.Now().UTC()}
	_, err := client.MarkRuntimeInstanceClosed(ctx, request)
	if err != nil {
		return err
	}
	p.logInfo("runtime desired close reconciled", "runtime_instance_id", runtimeInstanceID)
	return nil
}

func (p *PreparedRuntimePool) warmRuntimeTarget(
	ctx context.Context,
	client PreparedRuntimeInstanceClient,
	target workerapi.RuntimeReconcileTarget,
	admitted func(),
) error {
	if p == nil {
		return nil
	}
	if client == nil {
		return errors.New("prepared runtime instance client is required")
	}
	if target.Source.WorkspaceTarget == nil {
		return errors.New("prepared runtime warm command workspace target is required")
	}
	if p.AdmitRuntimeStart != nil {
		if err := p.AdmitRuntimeStart(ctx); err != nil {
			return err
		}
	}
	mount := preparedRuntimeWorkspaceMountFromSource(target.Source)
	if strings.TrimSpace(mount.DeploymentDefinitionID) == "" {
		return errors.New("prepared runtime warm command source is required")
	}
	mount.RuntimeInstanceID = strings.TrimSpace(target.ID)
	key := runtimeInstanceIDFromWorkspaceMount(mount)
	runtimeInstanceID := strings.TrimSpace(target.ID)
	runtimeEpoch := target.WorkerEpoch
	if runtimeInstanceID == "" || runtimeEpoch <= 0 {
		return errors.New("runtime reconcile target id and worker_epoch are required")
	}
	if p.Size <= 0 || p.Connector == nil || p.CAS == nil {
		reason := errors.New("prepared runtime pool is not configured")
		p.logInfo("prepared runtime warm skipped", "runtime_instance_id", key, "reason", reason.Error())
		stateCtx, cancelState := preparedRuntimeControlContext(ctx)
		defer cancelState()
		return p.markRuntimeTargetFailedWithProof(stateCtx, client, target, reason, workerapi.RuntimeCleanupNotMaterialized)
	}
	refillCtx, cancelRefill := p.withPoolContext(ctx)
	defer cancelRefill()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		reason := errors.New("prepared runtime pool closed")
		p.logInfo("prepared runtime warm skipped", "runtime_instance_id", key, "reason", reason.Error())
		stateCtx, cancelState := preparedRuntimeControlContext(ctx)
		defer cancelState()
		return p.markRuntimeTargetFailedWithProof(stateCtx, client, target, reason, workerapi.RuntimeCleanupNotMaterialized)
	}
	if p.reservedCountLocked() >= p.Size {
		p.mu.Unlock()
		p.logInfo("prepared runtime warm deferred", "runtime_instance_id", key, "reason", errPreparedRuntimeCapacityBusy.Error())
		return errPreparedRuntimeCapacityBusy
	}
	p.filling[key]++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.decrementFillingLocked(key)
		p.mu.Unlock()
	}()
	return p.prepareAndStore(refillCtx, key, mount, target, admitted)
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
		if releaseErr := p.releaseRuntimeCapacity(entry.runtimeInstanceID, entry.runtimeEpoch); releaseErr != nil {
			err = errors.Join(err, releaseErr)
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

func (p *PreparedRuntimePool) transitionRuntimeTargetFailed(ctx context.Context, target workerapi.RuntimeReconcileTarget, failure error) error {
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

func (p *PreparedRuntimePool) prepareAndStore(
	ctx context.Context,
	key string,
	mount workerapi.WorkspaceMount,
	target workerapi.RuntimeReconcileTarget,
	admitted func(),
) (retErr error) {
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
			proofMethod = workerapi.RuntimeCleanupNotMaterialized
		}
		if markErr := p.reportRuntimeTargetFailedWithProof(stateCtx, p.RuntimeInstances, target, err, proofMethod); markErr != nil {
			p.logInfo("prepared runtime pool instance fail transition failed", "runtime_instance_id", runtimeInstanceID, "error", markErr.Error())
			return markErr
		}
		var fatal interface{ FatalWorker() bool }
		if errors.As(err, &fatal) && fatal.FatalWorker() {
			return err
		}
		return nil
	}
	materializer := WorkspaceMaterializer{
		CAS:                   p.CAS,
		TempDir:               p.TempDir,
		ArtifactCacheDir:      p.ArtifactCacheDir,
		ArtifactCacheMaxBytes: p.ArtifactCacheMaxBytes,
		Log:                   p.Log,
	}
	tempDir := strings.TrimSpace(p.TempDir)
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return failInstance(err)
	}
	workspaceImagePath, cleanupWorkspaceImage, topology, err := p.restoreWorkspaceImageAndRuntimeSubstrate(ctx, materializer, tempDir, mount,
		"prepared runtime pool workspace image restored",
		"prepared runtime pool substrate resolved",
		"runtime_instance_id", runtimeInstanceID,
	)
	if err != nil {
		return failInstance(err)
	}
	defer cleanupWorkspaceImage()
	if err := p.verifyReservedWorkspaceVersion(ctx, materializer, tempDir, mount); err != nil {
		return failInstance(err)
	}
	readOnlyDrives, closeProgram, err := p.prepareProgram(
		ctx,
		tempDir,
		target,
	)
	if err != nil {
		return failInstance(err)
	}
	programArtifactsOpen := true
	defer func() {
		if programArtifactsOpen {
			retErr = errors.Join(retErr, closeProgram())
		}
	}()
	var runtimeSubstrateIDValue string
	if topology.Substrate != nil {
		started := time.Now()
		registered, err := registerRuntimeSubstrate(
			ctx,
			p.RuntimeSubstrates,
			mount.DeploymentDefinitionID,
			topology.Substrate,
		)
		p.logInfo("prepared runtime pool substrate resolved", "runtime_instance_id", runtimeInstanceID, "duration_ms", time.Since(started).Milliseconds(), "substrate_digest", runtimeSubstrateDigest(topology), "runtime_substrate_id", runtimeSubstrateID(registered), "error", errorString(err))
		if err != nil {
			return failInstance(err)
		}
		runtimeSubstrateIDValue = runtimeSubstrateID(registered)
	}
	started := time.Now()
	if err := p.reserveRuntimeCapacity(target, topology); err != nil {
		if errors.Is(err, errPreparedRuntimeCapacityBusy) {
			p.logInfo("prepared runtime warm deferred", "runtime_instance_id", runtimeInstanceID, "reason", err.Error())
			return err
		}
		return failInstance(err)
	}
	admitted()
	materializeAttempted = true
	var session vm.Session
	var materializeErr error
	phases := &runtimePhaseCollector{}
	phaseLogMessage := "prepared runtime phase"
	if target.Source.Restore != nil {
		phaseLogMessage = "prepared restored runtime phase"
		session, materializeErr = p.restorePreparedRuntime(ctx, target, topology, readOnlyDrives, phases.Record)
	} else {
		connector, ok := p.Connector.(vm.MaterializingConnector)
		if !ok {
			return failInstance(errors.New("connector does not support mount"))
		}
		session, materializeErr = connector.Materialize(ctx, vm.MaterializeRequest{
			ID: runtimeInstanceID, OwnerKind: vm.OwnerRuntime, RootfsDigest: mount.RootfsDigest,
			Binding:            runtimeTargetWorkloadBinding(target),
			WorkspaceMountPath: mount.WorkspaceMountPath, BaseVersionID: mount.Target.BaseWorkspaceVersionID,
			Resources: compute.ResourceVector{MilliCPU: mount.RequestedMilliCPU, MemoryMiB: mount.RequestedMemoryMiB,
				DiskMiB: mount.RequestedDiskMiB, Slots: mount.RequestedExecutionSlots},
			VMVCPUCount: target.Source.VMVCPUCount, CPUConfigDigest: target.Source.CPUConfigDigest,
			Topology: topology, ReadOnlyDrives: readOnlyDrives, RecordPhase: phases.Record,
		})
	}
	for _, phase := range phases.Snapshot() {
		p.logInfo(phaseLogMessage, "runtime_instance_id", runtimeInstanceID,
			"phase", phase.Name, "duration_ms", phase.DurationMs, "error_class", phase.ErrorClass)
	}
	closeProgramErr := closeProgram()
	programArtifactsOpen = false
	err = errors.Join(materializeErr, closeProgramErr)
	p.logInfo("prepared runtime pool session materialized", "runtime_instance_id", runtimeInstanceID, "duration_ms", time.Since(started).Milliseconds(), "error", errorString(err))
	if err != nil {
		return failInstance(err)
	}
	keepSession := false
	defer func() {
		if !keepSession {
			if closeErr := p.closeSession(ctx, session); closeErr == nil {
				_ = p.releaseRuntimeCapacity(runtimeInstanceID, runtimeEpoch)
			}
		}
	}()
	if target.Source.Restore == nil {
		if err := p.prepareGuestRuntime(ctx, session, key, mount, workspaceImagePath); err != nil {
			p.logInfo("prepared runtime pool guest prepare failed", "runtime_instance_id", runtimeInstanceID, "error", err.Error())
			return failInstance(err)
		}
	}
	entry := preparedRuntimeEntry{
		session:           session,
		poolKey:           key,
		runtimeInstanceID: runtimeInstanceID,
		runtimeEpoch:      runtimeEpoch,
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
			if err := p.releaseRuntimeCapacity(runtimeInstanceID, runtimeEpoch); err != nil {
				return failInstance(err)
			}
			keepSession = true
			p.logInfo("prepared runtime warm deferred", "runtime_instance_id", runtimeInstanceID, "reason", errPreparedRuntimeCapacityBusy.Error())
			return errPreparedRuntimeCapacityBusy
		}
		stateCtx, cancelState := preparedRuntimeControlContext(ctx)
		defer cancelState()
		if err := p.markRuntimeTargetFailed(stateCtx, p.RuntimeInstances, target, errors.New("runtime pool capacity changed")); err != nil {
			p.logInfo("prepared runtime pool instance close transition failed", "runtime_instance_id", runtimeInstanceID, "error", err.Error())
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
	readyRequest.VMVCPUCount = target.Source.VMVCPUCount
	readyRequest.CPUConfigDigest = target.Source.CPUConfigDigest
	if _, err := p.RuntimeInstances.MarkRuntimeInstanceReady(ctx, readyRequest); err != nil {
		entry.ready.finish(err)
		p.logInfo("prepared runtime pool instance ready transition failed", "runtime_instance_id", runtimeInstanceID, "error", err.Error())
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
		if candidate.runtimeInstanceID == entry.runtimeInstanceID && candidate.runtimeEpoch == entry.runtimeEpoch {
			stillReady = true
			break
		}
	}
	p.mu.Unlock()
	if !stillReady {
		return nil
	}
	p.logInfo("prepared runtime pool refilled", "runtime_instance_id", runtimeInstanceID, "available", available)
	return nil
}

func runtimeTargetWorkloadBinding(target workerapi.RuntimeReconcileTarget) vm.WorkloadBinding {
	return vm.WorkloadBinding{
		WorkerEpoch:       target.WorkerEpoch,
		OwnerID:           target.ID,
		Generation:        1,
		RuntimeInstanceID: target.ID,
		RuntimeIdentityID: target.Source.RuntimeIdentityID,
	}
}

func (p *PreparedRuntimePool) verifyReservedWorkspaceVersion(
	ctx context.Context,
	materializer WorkspaceMaterializer,
	tempDir string,
	mount workerapi.WorkspaceMount,
) error {
	if strings.TrimSpace(mount.Target.BaseWorkspaceVersionID) == "" {
		return errors.New("runtime reservation base workspace version is required")
	}
	if _, err := workspaceTargetFromWorker(mount.Target); err != nil {
		return fmt.Errorf("runtime reservation workspace target: %w", err)
	}
	if mount.Target.Empty != nil {
		return nil
	}
	artifact := mount.Target.Artifact
	_, cleanup, err := materializer.restoreCASObject(
		ctx,
		tempDir,
		"workspace-version",
		workerapi.CASObject{
			Digest:    artifact.Digest,
			SizeBytes: artifact.SizeBytes,
			MediaType: artifact.MediaType,
		},
	)
	cleanup()
	return err
}

func (p *PreparedRuntimePool) prepareProgram(
	ctx context.Context,
	tempDir string,
	target workerapi.RuntimeReconcileTarget,
) ([]vm.ReadOnlyDrive, func() error, error) {
	program := target.Source.Program
	if string(p.RuntimeArchitecture) != target.Source.WorkspaceArchitecture {
		return nil, func() error { return nil }, fmt.Errorf(
			"worker architecture %q does not match workspace architecture %q",
			p.RuntimeArchitecture,
			target.Source.WorkspaceArchitecture,
		)
	}
	if program == nil {
		return nil, func() error { return nil }, nil
	}
	if strings.TrimSpace(program.DeploymentID) == "" {
		return nil, func() error { return nil }, errors.New("program deployment id is required")
	}
	if p.PlatformStore == nil {
		return nil, func() error { return nil }, errors.New("managed runtime delivery is not configured")
	}
	if string(p.RuntimeArchitecture) != target.Source.WorkspaceArchitecture {
		return nil, func() error { return nil }, fmt.Errorf(
			"managed runtime architecture %q does not match workspace architecture %q",
			p.RuntimeArchitecture,
			target.Source.WorkspaceArchitecture,
		)
	}
	runtimeDescriptor := deployment.RuntimeDescriptor{
		Architecture:    p.RuntimeArchitecture,
		Digest:          program.Runtime.Digest,
		FormatVersion:   deployment.RuntimeDescriptorFormatVersion,
		MediaType:       program.Runtime.MediaType,
		RuntimeContract: deployment.RuntimeContract,
		SizeBytes:       program.Runtime.SizeBytes,
	}
	runtimeSnapshot, err := deployment.SnapshotRuntimeObject(
		ctx,
		p.PlatformStore,
		tempDir,
		runtimeDescriptor,
	)
	if err != nil {
		return nil, func() error { return nil }, err
	}
	closeSnapshots := func() error { return runtimeSnapshot.Close() }
	started := time.Now()
	runtimeIndex, memoHit, err := p.verifyRuntime(runtimeDescriptor, func() (deployment.RuntimeIndex, error) {
		return deployment.VerifyRuntimeArtifact(
			ctx,
			p.VerifierCgroupRoot,
			target.ID,
			runtimeSnapshot,
		)
	})
	p.logInfo("prepared runtime artifact verified",
		"runtime_instance_id", target.ID,
		"duration_ms", time.Since(started).Milliseconds(),
		"memo_hit", memoHit,
		"error", errorString(err),
	)
	if err != nil {
		return nil, func() error { return nil }, errors.Join(
			fmt.Errorf("verify managed runtime: %w", err),
			closeSnapshots(),
		)
	}
	expectedRuntimeIndex := deployment.RuntimeIndex{
		Architecture:    runtimeDescriptor.Architecture,
		RuntimeContract: runtimeDescriptor.RuntimeContract,
	}
	if runtimeIndex != expectedRuntimeIndex {
		return nil, func() error { return nil }, errors.Join(
			errors.New("managed runtime index does not match its descriptor"),
			closeSnapshots(),
		)
	}
	programSnapshot, err := deployment.SnapshotProgram(
		ctx,
		p.CAS,
		tempDir,
		deployment.ProgramDescriptor{
			Digest: program.Artifact.Digest, SizeBytes: program.Artifact.SizeBytes, MediaType: program.Artifact.MediaType,
		},
	)
	if err != nil {
		return nil, func() error { return nil }, errors.Join(err, closeSnapshots())
	}
	closeSnapshots = func() error {
		return errors.Join(runtimeSnapshot.Close(), programSnapshot.Close())
	}
	programIndex, err := deployment.VerifyProgram(
		ctx,
		p.VerifierCgroupRoot,
		target.ID,
		programSnapshot,
	)
	if err != nil {
		return nil, func() error { return nil }, errors.Join(
			fmt.Errorf("verify program: %w", err),
			closeSnapshots(),
		)
	}
	if programIndex.RuntimeContract != runtimeDescriptor.RuntimeContract ||
		programIndex.Architecture != runtimeDescriptor.Architecture {
		return nil, func() error { return nil }, errors.Join(
			errors.New("program index does not match runtime reservation authority"),
			closeSnapshots(),
		)
	}
	if err := verifyProgramIndexDigest(programIndex, program.IndexDigest); err != nil {
		return nil, func() error { return nil }, errors.Join(
			err,
			closeSnapshots(),
		)
	}
	return []vm.ReadOnlyDrive{
		{
			ID: vm.ProgramRuntimeDrive, Digest: runtimeDescriptor.Digest,
			SizeBytes: runtimeDescriptor.SizeBytes, MediaType: runtimeDescriptor.MediaType,
			Source: runtimeSnapshot,
		},
		{
			ID: vm.ProgramDrive, Digest: program.Artifact.Digest,
			SizeBytes: program.Artifact.SizeBytes, MediaType: program.Artifact.MediaType,
			Source: programSnapshot,
		},
	}, closeSnapshots, nil
}

func (p *PreparedRuntimePool) verifyRuntime(
	descriptor deployment.RuntimeDescriptor,
	verify func() (deployment.RuntimeIndex, error),
) (deployment.RuntimeIndex, bool, error) {
	p.mu.Lock()
	index, ok := p.verifiedRuntimes[descriptor]
	p.mu.Unlock()
	if ok {
		return index, true, nil
	}
	index, err := verify()
	if err != nil {
		return deployment.RuntimeIndex{}, false, err
	}

	p.mu.Lock()
	if p.verifiedRuntimes == nil {
		p.verifiedRuntimes = make(map[deployment.RuntimeDescriptor]deployment.RuntimeIndex)
	}
	p.verifiedRuntimes[descriptor] = index
	p.mu.Unlock()
	return index, false, nil
}

func verifyProgramIndexDigest(
	index deployment.ProgramIndex,
	expectedDigest string,
) error {
	indexBytes, err := deployment.CanonicalProgramIndex(index)
	if err != nil {
		return fmt.Errorf("canonicalize verified program index: %w", err)
	}
	if sha256sum.DigestBytes(indexBytes) != expectedDigest {
		return errors.New("program index does not match deployment authority")
	}
	return nil
}

func (p *PreparedRuntimePool) restoreWorkspaceImageAndRuntimeSubstrate(ctx context.Context, materializer WorkspaceMaterializer, tempDir string, mount workerapi.WorkspaceMount, imageLogMessage string, substrateLogMessage string, logAttrs ...any) (string, func(), vm.RuntimeTopology, error) {
	started := time.Now()
	workspaceImagePath, cleanupWorkspaceImage, err := materializer.restoreCASObject(ctx, tempDir, "workspace-image", mount.WorkspaceImage)
	p.logInfo(imageLogMessage, append(append([]any{}, logAttrs...),
		"duration_ms", time.Since(started).Milliseconds(),
		"size_bytes", mount.WorkspaceImage.SizeBytes,
		"error", errorString(err),
	)...)
	if err != nil {
		cleanupWorkspaceImage()
		return "", func() {}, vm.RuntimeTopology{}, err
	}
	started = time.Now()
	topology, err := runtimeSubstrateTopology(ctx, p.Substrates, workspaceImagePath, mount)
	p.logInfo(substrateLogMessage, append(append([]any{}, logAttrs...),
		"duration_ms", time.Since(started).Milliseconds(),
		"substrate_digest", runtimeSubstrateDigest(topology),
		"error", errorString(err),
	)...)
	if err != nil {
		cleanupWorkspaceImage()
		return "", func() {}, vm.RuntimeTopology{}, err
	}
	return workspaceImagePath, cleanupWorkspaceImage, topology, nil
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

func (p *PreparedRuntimePool) checkedOutRestoreCheckpoint(runtimeInstanceID string, runtimeEpoch int64) string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.checkedOutRestore[preparedRuntimeRef{id: strings.TrimSpace(runtimeInstanceID), epoch: runtimeEpoch}]
}

func (p *PreparedRuntimePool) ReleaseCheckout(runtimeInstanceID string, runtimeEpoch int64) error {
	if p == nil {
		return nil
	}
	ref := preparedRuntimeRef{id: strings.TrimSpace(runtimeInstanceID), epoch: runtimeEpoch}
	p.mu.Lock()
	_, checkedOut := p.checkedOut[ref]
	p.mu.Unlock()
	if !checkedOut {
		return nil
	}
	if err := p.releaseRuntimeCapacity(ref.id, ref.epoch); err != nil {
		return err
	}
	p.mu.Lock()
	delete(p.checkedOut, ref)
	delete(p.checkedOutRestore, ref)
	p.mu.Unlock()
	return nil
}

func (p *PreparedRuntimePool) removeReadyEntryAndFail(key string, entry preparedRuntimeEntry, cause error, closeSession bool) error {
	if !p.forgetReadyEntry(key, entry) {
		return nil
	}
	if closeSession && entry.session != nil {
		if closeErr := p.closeSession(context.Background(), entry.session); closeErr == nil {
			if releaseErr := p.releaseRuntimeCapacity(entry.runtimeInstanceID, entry.runtimeEpoch); releaseErr != nil {
				cause = errors.Join(cause, releaseErr)
			}
		} else {
			cause = errors.Join(cause, closeErr)
		}
	}
	stateCtx, cancelState := preparedRuntimeControlContext(context.Background())
	defer cancelState()
	if err := p.markRuntimeTargetFailed(stateCtx, p.RuntimeInstances, entry.target, cause); err != nil {
		p.logInfo("prepared runtime pool instance fail transition failed", "runtime_instance_id", entry.runtimeInstanceID, "error", err.Error())
		return err
	}
	p.logInfo("prepared runtime pool entry failed", "runtime_instance_id", entry.runtimeInstanceID, "error", errorString(cause))
	return nil
}

func (p *PreparedRuntimePool) cleanupClaimedEntryAsync(entry preparedRuntimeEntry, cause error) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if !p.beginActivityLocked() {
		p.mu.Unlock()
		p.cleanupClaimedEntry(entry, cause)
		return
	}
	p.mu.Unlock()
	go func() {
		defer p.endActivity()
		p.cleanupClaimedEntry(entry, cause)
	}()
}

func (p *PreparedRuntimePool) cleanupClaimedEntry(entry preparedRuntimeEntry, cause error) {
	if entry.session != nil {
		if closeErr := p.closeSession(context.Background(), entry.session); closeErr == nil {
			if releaseErr := p.releaseRuntimeCapacity(entry.runtimeInstanceID, entry.runtimeEpoch); releaseErr != nil {
				cause = errors.Join(cause, releaseErr)
			}
		} else {
			cause = errors.Join(cause, closeErr)
		}
	}
	stateCtx, cancelState := preparedRuntimeControlContext(context.Background())
	defer cancelState()
	if err := p.markRuntimeTargetFailed(stateCtx, p.RuntimeInstances, entry.target, cause); err != nil {
		p.logInfo("prepared runtime pool instance fail transition failed", "runtime_instance_id", entry.runtimeInstanceID, "error", err.Error())
		return
	}
	p.logInfo("prepared runtime pool claimed entry failed", "runtime_instance_id", entry.runtimeInstanceID, "error", errorString(cause))
}

func (p *PreparedRuntimePool) forgetReadyEntry(key string, entry preparedRuntimeEntry) bool {
	removed := false
	p.mu.Lock()
	entries := p.entries[key]
	for i := range entries {
		if entries[i].runtimeInstanceID == entry.runtimeInstanceID && entries[i].runtimeEpoch == entry.runtimeEpoch {
			p.removeReadyEntryAtLocked(key, entries, i)
			removed = true
			break
		}
	}
	p.mu.Unlock()
	return removed
}

func (p *PreparedRuntimePool) claimReadyEntry(runtimeInstanceID string, runtimeEpoch int64) (preparedRuntimeEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, entries := range p.entries {
		for i, entry := range entries {
			if entry.runtimeInstanceID != runtimeInstanceID || entry.runtimeEpoch != runtimeEpoch {
				continue
			}
			p.removeReadyEntryAtLocked(key, entries, i)
			return entry, true
		}
	}
	return preparedRuntimeEntry{}, false
}

func (p *PreparedRuntimePool) removeReadyEntryAtLocked(key string, entries []preparedRuntimeEntry, index int) {
	entries = append(entries[:index], entries[index+1:]...)
	if len(entries) == 0 {
		delete(p.entries, key)
		return
	}
	p.entries[key] = entries
}

func preparedRuntimeWorkspaceMountFromSource(source workerapi.RuntimeSource) workerapi.WorkspaceMount {
	return workerapi.WorkspaceMount{
		ID:                      uuid.NewV7().String(),
		WorkspaceID:             strings.TrimSpace(source.WorkspaceID),
		DeploymentDefinitionID:  strings.TrimSpace(source.DeploymentDefinitionID),
		Target:                  *source.WorkspaceTarget,
		RuntimeIdentityID:       strings.TrimSpace(source.RuntimeIdentityID),
		WorkspaceImage:          source.WorkspaceImage,
		RootfsDigest:            strings.TrimSpace(source.RootfsDigest),
		WorkspaceMountPath:      "/workspace",
		RequestedMilliCPU:       int64(source.ReservedCPUMillis),
		RequestedMemoryMiB:      int64(source.ReservedMemoryMiB),
		RequestedDiskMiB:        source.ReservedDiskMiB,
		RequestedExecutionSlots: source.ReservedExecutionSlots,
		VMRuntimeContract:       strings.TrimSpace(source.VMRuntimeContract),
	}
}

func (p *PreparedRuntimePool) prepareGuestRuntime(ctx context.Context, session vm.Session, key string, mount workerapi.WorkspaceMount, workspaceImagePath string) error {
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
		RuntimeInstanceId: key,
		MountPath:         strings.TrimSpace(mount.WorkspaceMountPath),
		WorkspaceImage: &workspacev0.WorkspaceArtifact{
			Digest:    strings.TrimSpace(mount.WorkspaceImage.Digest),
			MediaType: strings.TrimSpace(mount.WorkspaceImage.MediaType),
			Encoding:  "oci-tar",
			SizeBytes: uint64(mount.WorkspaceImage.SizeBytes),
		},
	}
	if err := frameio.WriteProtoFrame(stream, request); err != nil {
		return fmt.Errorf("write prepared runtime request: %w", err)
	}
	started := time.Now()
	if err := writeFileFrameWithMetadataContext(ctx, session, stream, wire.StreamHeader{
		Type:        wire.StreamTypeRunImage,
		WorkspaceID: mount.WorkspaceID,
	}, workspaceImagePath, strings.TrimSpace(mount.WorkspaceImage.Digest), mount.WorkspaceImage.SizeBytes); err != nil {
		// The guest can reject the request while the host is still streaming a
		// large image. Preserve the guest's structured failure instead of
		// reporting only the resulting broken pipe.
		responseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var response workspacev0.PrepareWorkspaceRuntimeResponse
		if responseErr := readProtoFrameFromReaderContext(responseCtx, session, stream, &response); responseErr == nil {
			if phaseError := workspaceMountPhaseError(response.GetPhases()); phaseError != "" {
				return fmt.Errorf("prepared runtime rejected workspace image: %s: %w", phaseError, err)
			}
			return fmt.Errorf("prepared runtime returned state %q while writing workspace image: %w", response.GetState(), err)
		}
		return fmt.Errorf("write prepared runtime workspace image: %w", err)
	}
	p.logInfo("prepared runtime pool workspace image sent", "runtime_instance_id", key, "duration_ms", time.Since(started).Milliseconds(), "size_bytes", mount.WorkspaceImage.SizeBytes)
	var response workspacev0.PrepareWorkspaceRuntimeResponse
	started = time.Now()
	if err := readProtoFrameFromReaderContext(ctx, session, stream, &response); err != nil {
		return fmt.Errorf("read prepared runtime response: %w", err)
	}
	p.logInfo("prepared runtime pool response read", "runtime_instance_id", key, "duration_ms", time.Since(started).Milliseconds(), "state", strings.TrimSpace(response.State))
	for _, guestPhase := range response.GetPhases() {
		if guestPhase == nil {
			continue
		}
		p.logInfo("prepared runtime pool guest phase",
			"runtime_instance_id", key,
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
	if strings.TrimSpace(response.RuntimeInstanceId) != key {
		return errors.New("prepared runtime instance id mismatch")
	}
	return nil
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

func (p *PreparedRuntimePool) reserveRuntimeCapacity(
	target workerapi.RuntimeReconcileTarget,
	topologies ...vm.RuntimeTopology,
) error {
	if p == nil || p.Capacity == nil {
		return errors.New("prepared runtime capacity ledger is required")
	}
	projectionBytes := int64(0)
	if len(topologies) > 1 {
		return errors.New("runtime capacity accepts at most one topology")
	}
	if len(topologies) == 1 && topologies[0].Substrate != nil {
		projectionBytes = topologies[0].Substrate.SizeBytes
	}
	request, err := runtimeCapacityVectorWithProjection(
		int64(target.Source.ReservedCPUMillis),
		int64(target.Source.ReservedMemoryMiB),
		target.Source.ReservedDiskMiB,
		projectionBytes,
	)
	if err != nil {
		return err
	}
	created, err := p.Capacity.Reserve(runtimeCapacityKey(target.ID, target.WorkerEpoch), request)
	if errors.Is(err, capacity.ErrCapacityExceeded) || err == nil && !created {
		return errPreparedRuntimeCapacityBusy
	}
	return err
}

func (p *PreparedRuntimePool) releaseRuntimeCapacity(runtimeInstanceID string, runtimeEpoch int64) error {
	if p == nil || p.Capacity == nil {
		return nil
	}
	return p.Capacity.Release(runtimeCapacityKey(runtimeInstanceID, runtimeEpoch))
}

func (p *PreparedRuntimePool) releaseRuntimeAfterPhysicalCleanup(runtimeInstanceID string, runtimeEpoch int64) error {
	if err := p.releaseRuntimeCapacity(runtimeInstanceID, runtimeEpoch); err != nil {
		return err
	}
	p.mu.Lock()
	delete(p.checkedOut, preparedRuntimeRef{id: strings.TrimSpace(runtimeInstanceID), epoch: runtimeEpoch})
	delete(p.checkedOutRestore, preparedRuntimeRef{id: strings.TrimSpace(runtimeInstanceID), epoch: runtimeEpoch})
	p.mu.Unlock()
	return nil
}

func (p *PreparedRuntimePool) closeSession(parent context.Context, session vm.Session) error {
	if session == nil {
		return nil
	}
	ctx, cancel := preparedRuntimeControlContext(parent)
	defer cancel()
	return session.Close(ctx)
}

func runtimeTargetStateRequest(target workerapi.RuntimeReconcileTarget, failure error) workerapi.RuntimeInstanceStateRequest {
	request := workerapi.RuntimeInstanceStateRequest{
		ID: target.ID, WorkerEpoch: target.WorkerEpoch, DesiredVersion: target.DesiredVersion,
		ExpectedObservedVersion: target.ObservedVersion, ReasonCode: "desired_state_reconciled",
	}
	if failure != nil {
		request.ReasonCode = workerapi.RuntimeFailureReconcile
		message := failure.Error()
		var fatal interface{ FatalWorker() bool }
		if errors.As(failure, &fatal) && fatal.FatalWorker() {
			request.ReasonCode = workerapi.RuntimeFailureWorkerInvalid
			message = "worker runtime infrastructure failed"
		}
		request.Error, _ = json.Marshal(map[string]string{"message": message})
	}
	return request
}

func (p *PreparedRuntimePool) markRuntimeTargetFailed(ctx context.Context, client PreparedRuntimeInstanceClient, target workerapi.RuntimeReconcileTarget, failure error) error {
	return p.markRuntimeTargetFailedWithProof(ctx, client, target, failure, "")
}

func (p *PreparedRuntimePool) markRuntimeTargetFailedWithProof(ctx context.Context, client PreparedRuntimeInstanceClient, target workerapi.RuntimeReconcileTarget, failure error, proofMethod string) error {
	if err := p.reportRuntimeTargetFailedWithProof(ctx, client, target, failure, proofMethod); err != nil {
		return errors.Join(failure, err)
	}
	return failure
}

func (p *PreparedRuntimePool) reportRuntimeTargetFailedWithProof(ctx context.Context, client PreparedRuntimeInstanceClient, target workerapi.RuntimeReconcileTarget, failure error, proofMethod string) error {
	request := runtimeTargetStateRequest(target, failure)
	if proofMethod != "" {
		request.CleanupProof = &workerapi.RuntimeCleanupProof{Method: proofMethod, CompletedAt: time.Now().UTC()}
	}
	_, err := client.MarkRuntimeInstanceFailed(ctx, request)
	if err != nil {
		return fmt.Errorf("report runtime preparation failure: %w", err)
	}
	return nil
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
