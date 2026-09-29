package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/snapshot"
	"github.com/helmrdotdev/helmr/internal/artifact/verify"
	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/ids"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const (
	defaultPreparedRuntimeControlTimeout = 15 * time.Second
)

var errPreparedRuntimeCapacityBusy = errors.New("prepared runtime local capacity is temporarily full")

type PreparedComputerInstanceClient interface {
	MarkComputerInstanceReady(context.Context, workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error)
	MarkComputerInstanceClosed(context.Context, workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error)
	MarkComputerInstanceFailed(context.Context, workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error)
}

type ComputerPreparationClient interface {
	InitialGenerationClient
	InitialComputerKey(context.Context, workerapi.InitialComputerKeyRequest) (workerapi.ComputerKeyMaterial, error)
	PublishInitialComputerGeneration(context.Context, workerapi.InitialComputerGenerationRequest) (workerapi.InitialComputerGenerationResponse, error)
	ComputerSource(context.Context, workerapi.ComputerSourceRequest) (workerapi.ComputerSourceMaterial, error)
}

type RuntimeReconcileClient interface {
	PreparedComputerInstanceClient
	ListRuntimeReconcileTargets(context.Context) (workerapi.RuntimeReconcileResponse, error)
}

type runtimeReconcileResult struct {
	ref preparedRuntimeRef
	err error
}

type PreparedRuntimePool struct {
	captureCleanup        map[preparedRuntimeRef]*computerCheckpointer
	ComputerCaptures      *ComputerCaptureRuns
	Checkpoints           ComputerCheckpointClient
	checkedOutEntries     map[preparedRuntimeRef]preparedRuntimeEntry
	Connector             vm.Cleaner
	CAS                   cas.Store
	ComputerObjects       cas.ImmutableStore
	ComputerPreparation   ComputerPreparationClient
	ComputerRanges        blockformat.RangeSource
	ComputerHelper        string
	ComputerDevices       []string
	ComputerStagingBytes  int64
	TempDir               string
	ArtifactCacheDir      string
	ArtifactCacheMaxBytes int64
	CheckpointEncryptor   *checkpoint.Encryptor
	Size                  int
	ComputerInstances     PreparedComputerInstanceClient
	Log                   *slog.Logger
	AdmitRuntimeStart     func(context.Context) error
	Capacity              *capacity.Ledger
	PlatformStore         cas.Reader
	RuntimeArchitecture   definition.RuntimeArchitecture
	VerifierCgroupRoot    string

	computerDevices   map[preparedRuntimeRef]vm.ComputerDevice
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
	verifiedRuntimes  map[artifact.RuntimeDescriptor]artifact.RuntimeIndex
	programDescriptor artifact.ProgramDescriptor
	programIndex      *artifact.ProgramIndex
}

type preparedRuntimeEntry struct {
	session            vm.Session
	poolKey            string
	computerInstanceID string
	runtimeEpoch       int64
	target             workerapi.RuntimeReconcileTarget
	exit               *preparedRuntimeSignal
	ready              *preparedRuntimeSignal
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

func (p *PreparedRuntimePool) Checkout(ctx context.Context, mount workerapi.ComputerInstanceAssignment) (vm.Session, string, bool) {
	if p == nil || p.Size <= 0 {
		return nil, "", false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := computerInstanceIDFromComputerMount(mount)
	computerInstanceID := strings.TrimSpace(mount.ComputerInstanceID)
	if computerInstanceID == "" {
		p.logInfo("prepared runtime pool miss", "reason", "computer_instance_missing")
		return nil, key, false
	}
	if mount.RuntimeEpoch <= 0 {
		p.logInfo("prepared runtime pool miss", "computer_instance_id", computerInstanceID, "reason", "runtime_epoch_missing")
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
		if entries[i].computerInstanceID == computerInstanceID && entries[i].runtimeEpoch == mount.RuntimeEpoch {
			index = i
			break
		}
	}
	if index < 0 {
		p.mu.Unlock()
		p.logInfo("prepared runtime pool miss", "computer_instance_id", computerInstanceID, "runtime_epoch", mount.RuntimeEpoch, "reason", "reserved_session_missing")
		return nil, key, false
	}
	entry := entries[index]
	if entry.target.Source.Computer == nil || entry.target.Source.ComputerID != mount.ComputerID ||
		entry.target.Source.Computer.VersionID != mount.Target.BaseComputerDiskVersionID || strings.TrimSpace(mount.Target.BaseComputerDiskVersionID) == "" {
		p.mu.Unlock()
		p.logInfo("prepared runtime pool miss", "computer_instance_id", computerInstanceID, "reason", "computer_source_mismatch")
		return nil, key, false
	}
	if err, exited := entry.exit.finished(); exited {
		p.mu.Unlock()
		p.removeReadyEntryAndFail(key, entry, preparedRuntimeExitCause(err), true)
		p.logInfo("prepared runtime pool miss", "computer_instance_id", computerInstanceID, "reason", "reserved_session_exited")
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
		p.logInfo("prepared runtime pool miss", "computer_instance_id", computerInstanceID, "reason", reason, "error", err.Error())
		return nil, key, false
	}
	if err, exited := entry.exit.finished(); exited {
		if p.forgetReadyEntry(key, entry) {
			p.cleanupClaimedEntryAsync(entry, preparedRuntimeExitCause(err))
		}
		p.logInfo("prepared runtime pool miss", "computer_instance_id", computerInstanceID, "reason", "reserved_session_exited", "error", errorString(err))
		return nil, key, false
	}
	p.mu.Lock()
	entries = p.entries[key]
	index = -1
	for i := range entries {
		if entries[i].computerInstanceID == computerInstanceID && entries[i].runtimeEpoch == mount.RuntimeEpoch {
			index = i
			break
		}
	}
	if index < 0 {
		p.mu.Unlock()
		p.logInfo("prepared runtime pool miss", "computer_instance_id", computerInstanceID, "runtime_epoch", mount.RuntimeEpoch, "reason", "reserved_session_claimed")
		return nil, key, false
	}
	entry = entries[index]
	if err, exited := entry.exit.finished(); exited {
		p.removeReadyEntryAtLocked(key, entries, index)
		p.mu.Unlock()
		p.cleanupClaimedEntryAsync(entry, preparedRuntimeExitCause(err))
		p.logInfo("prepared runtime pool miss", "computer_instance_id", computerInstanceID, "reason", "reserved_session_exited", "error", errorString(err))
		return nil, key, false
	}
	p.removeReadyEntryAtLocked(key, entries, index)
	p.markRuntimeCheckedOutLocked(computerInstanceID, mount.RuntimeEpoch)
	if p.checkedOutEntries == nil {
		p.checkedOutEntries = map[preparedRuntimeRef]preparedRuntimeEntry{}
	}
	p.checkedOutEntries[preparedRuntimeRef{id: computerInstanceID, epoch: mount.RuntimeEpoch}] = entry
	if entry.target.Source.Restore != nil {
		p.checkedOutRestore[preparedRuntimeRef{id: computerInstanceID, epoch: mount.RuntimeEpoch}] =
			strings.TrimSpace(entry.target.Source.Restore.CheckpointID)
	}
	available := p.readyCountLocked()
	p.mu.Unlock()
	p.logInfo("prepared runtime pool hit", "computer_instance_id", computerInstanceID, "available", available)
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
		if diagnostic, ok := verify.LocalDiagnostic(result.err); ok {
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
	client PreparedComputerInstanceClient,
	target workerapi.RuntimeReconcileTarget,
	admitted func(),
) error {
	switch {
	case target.Action == workerapi.RuntimeReconcileCapture:
		admitted()
		return p.captureRuntimeTarget(ctx, client, target)
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

func (p *PreparedRuntimePool) ReclaimFailedRuntimeTarget(ctx context.Context, client PreparedComputerInstanceClient, target workerapi.RuntimeReconcileTarget) error {
	if p == nil || client == nil {
		return errors.New("failed runtime reclaim requires pool and control plane client")
	}
	computerInstanceID := strings.TrimSpace(target.ID)
	if computerInstanceID == "" || target.WorkerEpoch <= 0 {
		return errors.New("failed runtime reclaim target id and worker_epoch are required")
	}
	if p.Connector == nil {
		return errors.New("runtime connector does not support exact failed-runtime cleanup")
	}
	entry, ready := p.claimReadyEntry(computerInstanceID, target.WorkerEpoch)
	var closeErr error
	if ready && entry.session != nil {
		closeCtx, cancel := preparedRuntimeControlContext(ctx)
		closeErr = entry.session.Close(closeCtx)
		cancel()
	}
	cleanupCtx, cancel := preparedRuntimeControlContext(ctx)
	err := p.Connector.Cleanup(cleanupCtx, vm.Owner{Kind: vm.OwnerRuntime, ID: computerInstanceID})
	cancel()
	if err != nil {
		return fmt.Errorf("reconcile failed runtime physical cleanup: %w", errors.Join(closeErr, err))
	}
	if err := p.releaseRuntimeAfterPhysicalCleanup(computerInstanceID, target.WorkerEpoch); err != nil {
		return err
	}
	request := runtimeTargetStatusRequest(target, errors.New("runtime physical cleanup reconciled"))
	request.CleanupProof = &workerapi.RuntimeCleanupProof{Method: workerapi.RuntimeCleanupHostReconciled, CompletedAt: time.Now().UTC()}
	if _, err := client.MarkComputerInstanceFailed(ctx, request); err != nil {
		return fmt.Errorf("persist failed runtime cleanup proof: %w", err)
	}
	return nil
}

func (p *PreparedRuntimePool) StopRuntimeTarget(ctx context.Context, client PreparedComputerInstanceClient, target workerapi.RuntimeReconcileTarget) error {
	if p == nil {
		return nil
	}
	computerInstanceID := strings.TrimSpace(target.ID)
	if computerInstanceID == "" {
		return errors.New("runtime stop target id is required")
	}
	if target.WorkerEpoch <= 0 {
		return errors.New("runtime stop target worker_epoch is required")
	}
	p.mu.Lock()
	capture := p.captureCleanup[preparedRuntimeRef{id: computerInstanceID, epoch: target.WorkerEpoch}]
	p.mu.Unlock()
	if capture != nil {
		if err := capture.ReleaseCheckpointSource(ctx); err != nil {
			return err
		}
		if err := p.releaseRuntimeAfterPhysicalCleanup(computerInstanceID, target.WorkerEpoch); err != nil {
			return err
		}
		request := runtimeTargetStatusRequest(target, nil)
		request.CleanupProof = &workerapi.RuntimeCleanupProof{Method: workerapi.RuntimeCleanupSessionClosed, CompletedAt: time.Now().UTC()}
		_, err := client.MarkComputerInstanceClosed(ctx, request)
		return err
	}
	stoppedEntry, ok := p.claimReadyEntry(computerInstanceID, target.WorkerEpoch)
	proofMethod := ""
	if !ok {
		if p.runtimeCheckedOut(computerInstanceID, target.WorkerEpoch) {
			return nil
		}
		if p.Connector == nil {
			return errors.New("runtime connector does not support exact runtime cleanup")
		}
		cleanupCtx, cancel := preparedRuntimeControlContext(ctx)
		err := p.Connector.Cleanup(cleanupCtx, vm.Owner{Kind: vm.OwnerRuntime, ID: computerInstanceID})
		cancel()
		if err != nil {
			return fmt.Errorf("reconcile runtime physical cleanup: %w", err)
		}
		if err := p.releaseRuntimeCapacity(computerInstanceID, target.WorkerEpoch); err != nil {
			return err
		}
		proofMethod = workerapi.RuntimeCleanupHostReconciled
	} else if stoppedEntry.session != nil {
		if err := stoppedEntry.session.Close(ctx); err != nil {
			return p.markRuntimeTargetFailed(ctx, client, target, err)
		}
		if err := p.releaseRuntimeCapacity(computerInstanceID, target.WorkerEpoch); err != nil {
			return err
		}
		proofMethod = workerapi.RuntimeCleanupSessionClosed
	} else {
		if p.Connector == nil {
			return errors.New("runtime connector does not support exact runtime cleanup")
		}
		cleanupCtx, cancel := preparedRuntimeControlContext(ctx)
		err := p.Connector.Cleanup(cleanupCtx, vm.Owner{Kind: vm.OwnerRuntime, ID: computerInstanceID})
		cancel()
		if err != nil {
			return fmt.Errorf("reconcile runtime physical cleanup: %w", err)
		}
		if err := p.releaseRuntimeCapacity(computerInstanceID, target.WorkerEpoch); err != nil {
			return err
		}
		proofMethod = workerapi.RuntimeCleanupHostReconciled
	}
	request := runtimeTargetStatusRequest(target, nil)
	request.CleanupProof = &workerapi.RuntimeCleanupProof{Method: proofMethod, CompletedAt: time.Now().UTC()}
	_, err := client.MarkComputerInstanceClosed(ctx, request)
	if err != nil {
		return err
	}
	p.logInfo("runtime desired close reconciled", "computer_instance_id", computerInstanceID)
	return nil
}

func (p *PreparedRuntimePool) warmRuntimeTarget(
	ctx context.Context,
	client PreparedComputerInstanceClient,
	target workerapi.RuntimeReconcileTarget,
	admitted func(),
) error {
	if p == nil {
		return nil
	}
	if client == nil {
		return errors.New("prepared runtime instance client is required")
	}
	if err := validateComputerPreparationSource(target); err != nil {
		return err
	}
	if target.Source.Restore != nil {
		if _, err := validatePreparedRuntimeRestore(target, p.RuntimeArchitecture); err != nil {
			return err
		}
	}
	if target.PreparationExpiresAt.IsZero() {
		return errors.New("runtime preparation deadline is required")
	}
	ctx, cancelPrepare := context.WithDeadline(ctx, target.PreparationExpiresAt)
	defer cancelPrepare()
	if p.AdmitRuntimeStart != nil {
		if err := p.AdmitRuntimeStart(ctx); err != nil {
			return err
		}
	}
	mount := preparedRuntimeComputerMountFromSource(target.Source)
	if strings.TrimSpace(mount.ComputerSpecID) == "" {
		return errors.New("prepared runtime warm command source is required")
	}
	mount.ComputerInstanceID = strings.TrimSpace(target.ID)
	key := computerInstanceIDFromComputerMount(mount)
	computerInstanceID := strings.TrimSpace(target.ID)
	runtimeEpoch := target.WorkerEpoch
	if computerInstanceID == "" || runtimeEpoch <= 0 {
		return errors.New("runtime reconcile target id and worker_epoch are required")
	}
	if p.Size <= 0 || p.Connector == nil || p.CAS == nil {
		reason := errors.New("prepared runtime pool is not configured")
		p.logInfo("prepared runtime warm skipped", "computer_instance_id", key, "reason", reason.Error())
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
		p.logInfo("prepared runtime warm skipped", "computer_instance_id", key, "reason", reason.Error())
		stateCtx, cancelState := preparedRuntimeControlContext(ctx)
		defer cancelState()
		return p.markRuntimeTargetFailedWithProof(stateCtx, client, target, reason, workerapi.RuntimeCleanupNotMaterialized)
	}
	if p.reservedCountLocked() >= p.Size {
		p.mu.Unlock()
		p.logInfo("prepared runtime warm deferred", "computer_instance_id", key, "reason", errPreparedRuntimeCapacityBusy.Error())
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
		if releaseErr := p.releaseRuntimeCapacity(entry.computerInstanceID, entry.runtimeEpoch); releaseErr != nil {
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
	if p.ComputerInstances == nil {
		return errors.New("prepared runtime instance client is required")
	}
	_, err := p.ComputerInstances.MarkComputerInstanceFailed(ctx, runtimeTargetStatusRequest(target, failure))
	return err
}

func (p *PreparedRuntimePool) retainCloseRetry(entry preparedRuntimeEntry) {
	if entry.session == nil {
		return
	}
	p.mu.Lock()
	key := entry.poolKey
	if strings.TrimSpace(key) == "" {
		key = entry.computerInstanceID
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
	mount workerapi.ComputerInstanceAssignment,
	target workerapi.RuntimeReconcileTarget,
	admitted func(),
) (retErr error) {
	computerInstanceID := strings.TrimSpace(target.ID)
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
			if closeErr := p.releaseComputerDevice(computerInstanceID, runtimeEpoch); closeErr != nil {
				failure := errors.Join(err, closeErr)
				return errors.Join(failure, p.reportRuntimeTargetFailedWithProof(stateCtx, p.ComputerInstances, target, failure, ""))
			}
			proofMethod = workerapi.RuntimeCleanupNotMaterialized
		}
		if markErr := p.reportRuntimeTargetFailedWithProof(stateCtx, p.ComputerInstances, target, err, proofMethod); markErr != nil {
			p.logInfo("prepared runtime pool instance fail transition failed", "computer_instance_id", computerInstanceID, "error", markErr.Error())
			return markErr
		}
		var fatal interface{ FatalWorker() bool }
		if errors.As(err, &fatal) && fatal.FatalWorker() {
			return err
		}
		return nil
	}
	topology := vm.RuntimeTopology{Computer: &vm.RuntimeComputer{
		ComputerID: target.Source.ComputerID,
		VersionID:  target.Source.Computer.VersionID, SizeBytes: target.Source.Computer.LogicalBytes,
	}}
	if err := p.reserveRuntimeCapacity(target, topology); err != nil {
		if errors.Is(err, errPreparedRuntimeCapacityBusy) {
			return err
		}
		return failInstance(err)
	}
	defer func() {
		if !materializeAttempted {
			retErr = errors.Join(retErr, p.releaseRuntimeCapacity(computerInstanceID, runtimeEpoch))
		}
	}()
	admitted()
	tempDir := strings.TrimSpace(p.TempDir)
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	if err := os.MkdirAll(tempDir, 0700); err != nil {
		return failInstance(err)
	}
	device, err := p.prepareComputerDevice(ctx, target)
	if err != nil {
		if errors.Is(err, errPreparedRuntimeCapacityBusy) {
			return err
		}
		return failInstance(err)
	}
	topology.Computer.Device = device
	config := target.Source.Computer.Config
	mountedImageConfig := &computerv0.RuntimeImageConfig{Env: config.Env, WorkingDir: config.WorkingDir, User: config.User, Entrypoint: config.Entrypoint, Cmd: config.Cmd}
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
	started := time.Now()
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
			ID: computerInstanceID, OwnerKind: vm.OwnerRuntime, RootfsDigest: mount.RootfsDigest,
			Binding:           runtimeTargetWorkloadBinding(target),
			ComputerMountPath: mount.ComputerMountPath, BaseComputerDiskVersionID: mount.Target.BaseComputerDiskVersionID,
			Resources: compute.ResourceVector{MilliCPU: mount.RequestedMilliCPU, MemoryMiB: mount.RequestedMemoryMiB,
				DiskMiB: mount.RequestedDiskMiB, Slots: mount.RequestedExecutionSlots},
			VMVCPUCount: target.Source.VMVCPUCount, CPUConfigDigest: target.Source.CPUConfigDigest,
			Topology: topology, ReadOnlyDrives: readOnlyDrives, RecordPhase: phases.Record,
		})
	}
	for _, phase := range phases.Snapshot() {
		p.logInfo(phaseLogMessage, "computer_instance_id", computerInstanceID,
			"phase", phase.Name, "duration_ms", phase.DurationMs, "error_class", phase.ErrorClass)
	}
	closeProgramErr := closeProgram()
	programArtifactsOpen = false
	err = errors.Join(materializeErr, closeProgramErr)
	if err != nil && session != nil {
		err = errors.Join(err, p.closeSession(ctx, session))
	}
	p.logInfo("prepared runtime pool session materialized", "computer_instance_id", computerInstanceID, "duration_ms", time.Since(started).Milliseconds(), "error", errorString(err))
	if err != nil {
		return failInstance(err)
	}
	keepSession := false
	defer func() {
		if !keepSession {
			if closeErr := p.closeSession(ctx, session); closeErr == nil {
				_ = p.releaseRuntimeCapacity(computerInstanceID, runtimeEpoch)
			}
		}
	}()
	if target.Source.Restore == nil {
		if err := p.prepareGuestRuntime(ctx, session, key, target.Source.WriterGeneration, mount, "", mountedImageConfig); err != nil {
			p.logInfo("prepared runtime pool guest prepare failed", "computer_instance_id", computerInstanceID, "error", err.Error())
			return failInstance(err)
		}
	}
	entry := preparedRuntimeEntry{
		session:            session,
		poolKey:            key,
		computerInstanceID: computerInstanceID,
		runtimeEpoch:       runtimeEpoch,
		target:             target,
		exit:               newPreparedRuntimeSignal(),
		ready:              newPreparedRuntimeSignal(),
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
			if err := p.releaseRuntimeCapacity(computerInstanceID, runtimeEpoch); err != nil {
				return failInstance(err)
			}
			keepSession = true
			p.logInfo("prepared runtime warm deferred", "computer_instance_id", computerInstanceID, "reason", errPreparedRuntimeCapacityBusy.Error())
			return errPreparedRuntimeCapacityBusy
		}
		stateCtx, cancelState := preparedRuntimeControlContext(ctx)
		defer cancelState()
		if err := p.markRuntimeTargetFailed(stateCtx, p.ComputerInstances, target, errors.New("runtime pool capacity changed")); err != nil {
			p.logInfo("prepared runtime pool instance close transition failed", "computer_instance_id", computerInstanceID, "error", err.Error())
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
	readyRequest := runtimeTargetStatusRequest(target, nil)
	readyRequest.VMVCPUCount = target.Source.VMVCPUCount
	readyRequest.CPUConfigDigest = target.Source.CPUConfigDigest
	readyCtx, cancelReady := preparedRuntimeControlContext(ctx)
	_, readyErr := p.ComputerInstances.MarkComputerInstanceReady(readyCtx, readyRequest)
	cancelReady()
	if err := readyErr; err != nil {
		entry.ready.finish(err)
		p.logInfo("prepared runtime pool instance ready transition failed", "computer_instance_id", computerInstanceID, "error", err.Error())
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
		if candidate.computerInstanceID == entry.computerInstanceID && candidate.runtimeEpoch == entry.runtimeEpoch {
			stillReady = true
			break
		}
	}
	p.mu.Unlock()
	if !stillReady {
		return nil
	}
	p.logInfo("prepared runtime pool refilled", "computer_instance_id", computerInstanceID, "available", available)
	return nil
}

func runtimeTargetWorkloadBinding(target workerapi.RuntimeReconcileTarget) vm.WorkloadBinding {
	return vm.WorkloadBinding{
		WorkerEpoch:        target.WorkerEpoch,
		OwnerID:            target.ID,
		Generation:         1,
		ComputerInstanceID: target.ID,
		VMPlatformID:       target.Source.VMPlatformID,
	}
}

func (p *PreparedRuntimePool) prepareProgram(
	ctx context.Context,
	tempDir string,
	target workerapi.RuntimeReconcileTarget,
) ([]vm.ReadOnlyDrive, func() error, error) {
	program := target.Source.Program
	if string(p.RuntimeArchitecture) != target.Source.ComputerArchitecture {
		return nil, func() error { return nil }, fmt.Errorf(
			"worker architecture %q does not match computer architecture %q",
			p.RuntimeArchitecture,
			target.Source.ComputerArchitecture,
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
	runtimeDescriptor := artifact.RuntimeDescriptor{
		Architecture:    p.RuntimeArchitecture,
		Digest:          program.Runtime.Digest,
		FormatVersion:   artifact.RuntimeDescriptorFormatVersion,
		MediaType:       program.Runtime.MediaType,
		RuntimeContract: definition.RuntimeContract,
		SizeBytes:       program.Runtime.SizeBytes,
	}
	runtimeSnapshot, err := snapshot.RuntimeObject(
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
	runtimeIndex, memoHit, err := p.verifyRuntime(runtimeDescriptor, func() (artifact.RuntimeIndex, error) {
		return verify.Runtime(
			ctx,
			p.VerifierCgroupRoot,
			target.ID,
			runtimeSnapshot,
		)
	})
	p.logInfo("prepared runtime artifact verified",
		"computer_instance_id", target.ID,
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
	expectedRuntimeIndex := artifact.RuntimeIndex{
		Architecture:    runtimeDescriptor.Architecture,
		RuntimeContract: runtimeDescriptor.RuntimeContract,
	}
	if runtimeIndex != expectedRuntimeIndex {
		return nil, func() error { return nil }, errors.Join(
			errors.New("managed runtime index does not match its descriptor"),
			closeSnapshots(),
		)
	}
	programDescriptor := artifact.ProgramDescriptor{
		Digest: program.Artifact.Digest, SizeBytes: program.Artifact.SizeBytes, MediaType: program.Artifact.MediaType,
	}
	programSnapshot, err := snapshot.ProgramObject(
		ctx,
		p.CAS,
		tempDir,
		programDescriptor,
	)
	if err != nil {
		return nil, func() error { return nil }, errors.Join(err, closeSnapshots())
	}
	closeSnapshots = func() error {
		return errors.Join(runtimeSnapshot.Close(), programSnapshot.Close())
	}
	programIndex, err := p.verifyProgram(ctx, programDescriptor, func() (artifact.ProgramIndex, error) {
		return verify.Program(ctx, p.VerifierCgroupRoot, target.ID, programSnapshot)
	})
	if err != nil {
		return nil, func() error { return nil }, errors.Join(
			fmt.Errorf("verify program: %w", err),
			closeSnapshots(),
		)
	}
	if programIndex.RuntimeDigest != runtimeDescriptor.Digest ||
		programIndex.RuntimeContract != runtimeDescriptor.RuntimeContract ||
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

// Callers must verify a fresh snapshot against descriptor before every call,
// including memo hits.
func (p *PreparedRuntimePool) verifyProgram(
	ctx context.Context,
	descriptor artifact.ProgramDescriptor,
	verify func() (artifact.ProgramIndex, error),
) (artifact.ProgramIndex, error) {
	if err := ctx.Err(); err != nil {
		return artifact.ProgramIndex{}, err
	}
	p.mu.Lock()
	cached := p.programIndex
	if p.programDescriptor != descriptor {
		cached = nil
	}
	p.mu.Unlock()
	if cached != nil {
		return cached.Clone(), nil
	}
	index, err := verify()
	if err != nil {
		return artifact.ProgramIndex{}, err
	}
	owned := index.Clone()
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return artifact.ProgramIndex{}, err
	}
	p.programDescriptor = descriptor
	p.programIndex = &owned
	return index, nil
}

func (p *PreparedRuntimePool) verifyRuntime(
	descriptor artifact.RuntimeDescriptor,
	verify func() (artifact.RuntimeIndex, error),
) (artifact.RuntimeIndex, bool, error) {
	p.mu.Lock()
	index, ok := p.verifiedRuntimes[descriptor]
	p.mu.Unlock()
	if ok {
		return index, true, nil
	}
	index, err := verify()
	if err != nil {
		return artifact.RuntimeIndex{}, false, err
	}

	p.mu.Lock()
	if p.verifiedRuntimes == nil {
		p.verifiedRuntimes = make(map[artifact.RuntimeDescriptor]artifact.RuntimeIndex)
	}
	p.verifiedRuntimes[descriptor] = index
	p.mu.Unlock()
	return index, false, nil
}

func verifyProgramIndexDigest(
	index artifact.ProgramIndex,
	expectedDigest string,
) error {
	indexBytes, err := artifact.CanonicalProgramIndex(index)
	if err != nil {
		return fmt.Errorf("canonicalize verified program index: %w", err)
	}
	if sha256sum.DigestBytes(indexBytes) != expectedDigest {
		return errors.New("program index does not match deployment authority")
	}
	return nil
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

func (p *PreparedRuntimePool) markRuntimeCheckedOutLocked(computerInstanceID string, runtimeEpoch int64) {
	if p.checkedOut == nil {
		p.checkedOut = map[preparedRuntimeRef]struct{}{}
	}
	p.checkedOut[preparedRuntimeRef{id: computerInstanceID, epoch: runtimeEpoch}] = struct{}{}
}

func (p *PreparedRuntimePool) runtimeCheckedOut(computerInstanceID string, runtimeEpoch int64) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.checkedOut[preparedRuntimeRef{id: computerInstanceID, epoch: runtimeEpoch}]
	return ok
}

func (p *PreparedRuntimePool) checkedOutWriterGeneration(instanceID string, epoch int64) int64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.checkedOutEntries[preparedRuntimeRef{id: instanceID, epoch: epoch}].target.Source.WriterGeneration
}

func (p *PreparedRuntimePool) checkedOutRestoreCheckpoint(computerInstanceID string, runtimeEpoch int64) string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.checkedOutRestore[preparedRuntimeRef{id: strings.TrimSpace(computerInstanceID), epoch: runtimeEpoch}]
}

// relinquishCheckout hands an exited materializer back to reconciliation without
// releasing capacity or device ownership before physical cleanup is proved.
func (p *PreparedRuntimePool) relinquishCheckout(computerInstanceID string, runtimeEpoch int64) {
	if p == nil {
		return
	}
	ref := preparedRuntimeRef{id: strings.TrimSpace(computerInstanceID), epoch: runtimeEpoch}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.checkedOut, ref)
	delete(p.checkedOutEntries, ref)
	delete(p.checkedOutRestore, ref)
}

func (p *PreparedRuntimePool) ReleaseCheckout(computerInstanceID string, runtimeEpoch int64) error {
	if p == nil {
		return nil
	}
	ref := preparedRuntimeRef{id: strings.TrimSpace(computerInstanceID), epoch: runtimeEpoch}
	p.mu.Lock()
	_, checkedOut := p.checkedOut[ref]
	p.mu.Unlock()
	if !checkedOut {
		return nil
	}
	defer p.relinquishCheckout(ref.id, ref.epoch)
	if err := p.releaseRuntimeCapacity(ref.id, ref.epoch); err != nil {
		return err
	}
	return nil
}

func (p *PreparedRuntimePool) removeReadyEntryAndFail(key string, entry preparedRuntimeEntry, cause error, closeSession bool) error {
	if !p.forgetReadyEntry(key, entry) {
		return nil
	}
	if closeSession && entry.session != nil {
		if closeErr := p.closeSession(context.Background(), entry.session); closeErr == nil {
			if releaseErr := p.releaseRuntimeCapacity(entry.computerInstanceID, entry.runtimeEpoch); releaseErr != nil {
				cause = errors.Join(cause, releaseErr)
			}
		} else {
			cause = errors.Join(cause, closeErr)
		}
	}
	stateCtx, cancelState := preparedRuntimeControlContext(context.Background())
	defer cancelState()
	if err := p.markRuntimeTargetFailed(stateCtx, p.ComputerInstances, entry.target, cause); err != nil {
		p.logInfo("prepared runtime pool instance fail transition failed", "computer_instance_id", entry.computerInstanceID, "error", err.Error())
		return err
	}
	p.logInfo("prepared runtime pool entry failed", "computer_instance_id", entry.computerInstanceID, "error", errorString(cause))
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
			if releaseErr := p.releaseRuntimeCapacity(entry.computerInstanceID, entry.runtimeEpoch); releaseErr != nil {
				cause = errors.Join(cause, releaseErr)
			}
		} else {
			cause = errors.Join(cause, closeErr)
		}
	}
	stateCtx, cancelState := preparedRuntimeControlContext(context.Background())
	defer cancelState()
	if err := p.markRuntimeTargetFailed(stateCtx, p.ComputerInstances, entry.target, cause); err != nil {
		p.logInfo("prepared runtime pool instance fail transition failed", "computer_instance_id", entry.computerInstanceID, "error", err.Error())
		return
	}
	p.logInfo("prepared runtime pool claimed entry failed", "computer_instance_id", entry.computerInstanceID, "error", errorString(cause))
}

func (p *PreparedRuntimePool) forgetReadyEntry(key string, entry preparedRuntimeEntry) bool {
	removed := false
	p.mu.Lock()
	entries := p.entries[key]
	for i := range entries {
		if entries[i].computerInstanceID == entry.computerInstanceID && entries[i].runtimeEpoch == entry.runtimeEpoch {
			p.removeReadyEntryAtLocked(key, entries, i)
			removed = true
			break
		}
	}
	p.mu.Unlock()
	return removed
}

func (p *PreparedRuntimePool) claimReadyEntry(computerInstanceID string, runtimeEpoch int64) (preparedRuntimeEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, entries := range p.entries {
		for i, entry := range entries {
			if entry.computerInstanceID != computerInstanceID || entry.runtimeEpoch != runtimeEpoch {
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

func preparedRuntimeComputerMountFromSource(source workerapi.RuntimeSource) workerapi.ComputerInstanceAssignment {
	return workerapi.ComputerInstanceAssignment{

		ComputerID:              strings.TrimSpace(source.ComputerID),
		ComputerSpecID:          strings.TrimSpace(source.ComputerSpecID),
		Target:                  workerapi.ComputerMountTarget{BaseComputerDiskVersionID: source.Computer.VersionID},
		VMPlatformID:            strings.TrimSpace(source.VMPlatformID),
		ComputerImage:           source.ComputerImage,
		RootfsDigest:            strings.TrimSpace(source.RootfsDigest),
		ComputerMountPath:       "/workspace",
		RequestedMilliCPU:       int64(source.ReservedCPUMillis),
		RequestedMemoryMiB:      int64(source.ReservedMemoryMiB),
		RequestedDiskMiB:        source.ReservedDiskMiB,
		RequestedExecutionSlots: source.ReservedExecutionSlots,
		VMRuntimeContract:       strings.TrimSpace(source.VMRuntimeContract),
	}
}

func (p *PreparedRuntimePool) prepareGuestRuntime(ctx context.Context, session vm.Session, key string, writerGeneration int64, mount workerapi.ComputerInstanceAssignment, computerImagePath string, mountedImageConfig *computerv0.RuntimeImageConfig) error {
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return fmt.Errorf("open prepared runtime stream: %w", err)
	}
	defer stream.Close()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:       wire.StreamTypeComputerRuntimePrepare,
		ComputerID: mount.ComputerID,
	}, 0); err != nil {
		return fmt.Errorf("write prepared runtime header: %w", err)
	}
	request := &computerv0.PrepareComputerRuntimeRequest{
		ComputerInstanceId: key,
		ComputerId:         mount.ComputerID, WriterGeneration: writerGeneration,
		MountedImageConfig: mountedImageConfig,
		MountPath:          strings.TrimSpace(mount.ComputerMountPath),
		ComputerImage: &computerv0.ComputerArtifact{
			Digest:    strings.TrimSpace(mount.ComputerImage.Digest),
			MediaType: strings.TrimSpace(mount.ComputerImage.MediaType),
			Encoding:  "oci-tar",
			SizeBytes: uint64(mount.ComputerImage.SizeBytes),
		},
	}
	if err := frameio.WriteProtoFrame(stream, request); err != nil {
		return fmt.Errorf("write prepared runtime request: %w", err)
	}
	started := time.Now()
	if mountedImageConfig == nil {
		if err := writeFileFrameWithMetadataContext(ctx, session, stream, wire.StreamHeader{
			Type:       wire.StreamTypeRunImage,
			ComputerID: mount.ComputerID,
		}, computerImagePath, strings.TrimSpace(mount.ComputerImage.Digest), mount.ComputerImage.SizeBytes); err != nil {
			// The guest can reject the request while the host is still streaming a
			// large image. Preserve the guest's structured failure instead of
			// reporting only the resulting broken pipe.
			responseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var response computerv0.PrepareComputerRuntimeResponse
			if responseErr := readProtoFrameFromReaderContext(responseCtx, session, stream, &response); responseErr == nil {
				if phaseError := computerMountPhaseError(response.GetPhases()); phaseError != "" {
					return fmt.Errorf("prepared runtime rejected computer image: %s: %w", phaseError, err)
				}
				return fmt.Errorf("prepared runtime returned state %q while writing computer image: %w", response.GetStatus(), err)
			}
			return fmt.Errorf("write prepared runtime computer image: %w", err)
		}
		p.logInfo("prepared runtime pool computer image sent", "computer_instance_id", key, "duration_ms", time.Since(started).Milliseconds(), "size_bytes", mount.ComputerImage.SizeBytes)
	}

	var response computerv0.PrepareComputerRuntimeResponse
	started = time.Now()
	if err := readProtoFrameFromReaderContext(ctx, session, stream, &response); err != nil {
		return fmt.Errorf("read prepared runtime response: %w", err)
	}
	p.logInfo("prepared runtime pool response read", "computer_instance_id", key, "duration_ms", time.Since(started).Milliseconds(), "state", strings.TrimSpace(response.Status))
	for _, guestPhase := range response.GetPhases() {
		if guestPhase == nil {
			continue
		}
		p.logInfo("prepared runtime pool guest phase",
			"computer_instance_id", key,
			"guest_phase", strings.TrimSpace(guestPhase.GetName()),
			"duration_ms", guestPhase.GetDurationMs(),
			"size_bytes", guestPhase.GetSizeBytes(),
			"entry_count", guestPhase.GetEntryCount(),
			"error", strings.TrimSpace(guestPhase.GetError()),
		)
	}
	if response.Status != "prepared" {
		if phaseError := computerMountPhaseError(response.GetPhases()); phaseError != "" {
			return fmt.Errorf("prepared runtime returned state %q: %s", response.Status, phaseError)
		}
		return fmt.Errorf("prepared runtime returned state %q", response.Status)
	}
	if strings.TrimSpace(response.ComputerInstanceId) != key {
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
	if len(topologies) == 1 && topologies[0].Computer != nil {
		projectionBytes = topologies[0].Computer.SizeBytes
	}
	retained, staging, err := p.checkpointRestoreCapacity(target)
	if err != nil {
		return err
	}
	if retained > math.MaxInt64-projectionBytes {
		return capacity.ErrOverflow
	}
	projectionBytes += retained
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
	if err != nil {
		return err
	}
	if staging > 0 {
		created, err = p.Capacity.Reserve(restoreStagingKey(target.ID, target.WorkerEpoch), capacity.Vector{GuestEphemeralDiskBytes: staging})
		if err != nil || !created {
			releaseErr := p.Capacity.Release(runtimeCapacityKey(target.ID, target.WorkerEpoch))
			if errors.Is(err, capacity.ErrCapacityExceeded) || err == nil {
				err = errPreparedRuntimeCapacityBusy
			}
			return errors.Join(err, releaseErr)
		}
	}
	return err
}

func (p *PreparedRuntimePool) releaseRuntimeCapacity(computerInstanceID string, runtimeEpoch int64) error {
	if err := p.releaseComputerDevice(computerInstanceID, runtimeEpoch); err != nil {
		return err
	}
	if p == nil || p.Capacity == nil {
		return nil
	}
	if ids.Validate(computerInstanceID) == nil && runtimeEpoch > 0 {
		if err := os.RemoveAll(p.computerPreparationDirectory(computerInstanceID, runtimeEpoch)); err != nil {
			return err
		}
		if err := os.RemoveAll(p.restorePreparationDirectory(computerInstanceID, runtimeEpoch)); err != nil {
			return err
		}
	}
	if err := p.Capacity.Release(computerStagingKey(computerInstanceID, runtimeEpoch)); err != nil {
		return err
	}
	if err := p.Capacity.Release(restoreStagingKey(computerInstanceID, runtimeEpoch)); err != nil {
		return err
	}
	if err := p.Capacity.Release(runtimeCapacityKey(computerInstanceID, runtimeEpoch)); err != nil {
		return err
	}
	p.mu.Lock()
	delete(p.computerDevices, preparedRuntimeRef{id: computerInstanceID, epoch: runtimeEpoch})
	p.mu.Unlock()
	return nil
}

func (p *PreparedRuntimePool) releaseRuntimeAfterPhysicalCleanup(computerInstanceID string, runtimeEpoch int64) error {
	ref := preparedRuntimeRef{id: strings.TrimSpace(computerInstanceID), epoch: runtimeEpoch}
	p.mu.Lock()
	capture := p.captureCleanup[ref]
	p.mu.Unlock()
	if capture != nil {
		if err := capture.cleanupAfterSourceStopped(); err != nil {
			return err
		}
	}

	if err := p.releaseRuntimeCapacity(computerInstanceID, runtimeEpoch); err != nil {
		return err
	}
	p.mu.Lock()
	delete(p.checkedOut, preparedRuntimeRef{id: strings.TrimSpace(computerInstanceID), epoch: runtimeEpoch})
	delete(p.checkedOutEntries, preparedRuntimeRef{id: strings.TrimSpace(computerInstanceID), epoch: runtimeEpoch})
	delete(p.captureCleanup, ref)
	delete(p.checkedOutRestore, preparedRuntimeRef{id: strings.TrimSpace(computerInstanceID), epoch: runtimeEpoch})
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

func runtimeTargetStatusRequest(target workerapi.RuntimeReconcileTarget, failure error) workerapi.ComputerInstanceStateRequest {
	request := workerapi.ComputerInstanceStateRequest{
		ID: target.ID, WorkerEpoch: target.WorkerEpoch, DesiredVersion: target.DesiredVersion,
		ExpectedObservedVersion: target.ObservedVersion, ReasonCode: "desired_state_reconciled",
	}
	if failure != nil {
		request.ReasonCode = workerapi.RuntimeFailureReconcile
		message := failure.Error()
		var sourceFailure *computer.SourceFailure
		if errors.As(failure, &sourceFailure) {
			request.ReasonCode = workerapi.RuntimeFailureComputerSource
		}
		var fatal interface{ FatalWorker() bool }
		if errors.As(failure, &fatal) && fatal.FatalWorker() {
			request.ReasonCode = workerapi.RuntimeFailureWorkerInvalid
			message = "worker runtime infrastructure failed"
		}
		request.Error, _ = json.Marshal(map[string]string{"message": message})
	}
	return request
}

func (p *PreparedRuntimePool) markRuntimeTargetFailed(ctx context.Context, client PreparedComputerInstanceClient, target workerapi.RuntimeReconcileTarget, failure error) error {
	return p.markRuntimeTargetFailedWithProof(ctx, client, target, failure, "")
}

func (p *PreparedRuntimePool) markRuntimeTargetFailedWithProof(ctx context.Context, client PreparedComputerInstanceClient, target workerapi.RuntimeReconcileTarget, failure error, proofMethod string) error {
	if err := p.reportRuntimeTargetFailedWithProof(ctx, client, target, failure, proofMethod); err != nil {
		return errors.Join(failure, err)
	}
	return failure
}

func (p *PreparedRuntimePool) reportRuntimeTargetFailedWithProof(ctx context.Context, client PreparedComputerInstanceClient, target workerapi.RuntimeReconcileTarget, failure error, proofMethod string) error {
	request := runtimeTargetStatusRequest(target, failure)
	if proofMethod != "" {
		request.CleanupProof = &workerapi.RuntimeCleanupProof{Method: proofMethod, CompletedAt: time.Now().UTC()}
	}
	_, err := client.MarkComputerInstanceFailed(ctx, request)
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
