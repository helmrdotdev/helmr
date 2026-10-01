package computerhost

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
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/ids"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

const (
	defaultPreparedMachineControlTimeout = 15 * time.Second
)

var errPreparedMachineCapacityBusy = errors.New("prepared machine local capacity is temporarily full")

type PreparedComputerInstanceClient interface {
	MarkComputerInstanceReady(context.Context, workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error)
	MarkComputerInstanceClosed(context.Context, workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error)
	MarkComputerInstanceFailed(context.Context, workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error)
}

type ComputerPreparationClient interface {
	InitialVersionClient
	InitialComputerKey(context.Context, workerapi.InitialComputerKeyRequest) (workerapi.ComputerKeyMaterial, error)
	PublishInitialComputerVersion(context.Context, workerapi.InitialComputerVersionRequest) (workerapi.InitialComputerVersionResponse, error)
	ComputerSource(context.Context, workerapi.ComputerSourceRequest) (workerapi.ComputerSourceMaterial, error)
}

type InstanceReconcileClient interface {
	PreparedComputerInstanceClient
	ListInstanceReconcileTargets(context.Context) (workerapi.InstanceReconcileResponse, error)
}

type instanceReconcileResult struct {
	ref preparedMachineRef
	err error
}

type PreparedMachines struct {
	ComputerCaptures      *CaptureRuns
	Checkpoints           ComputerCheckpointClient
	Backend               vm.Backend
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
	CheckpointEncryptor   *CheckpointEncryptor
	Size                  int
	ComputerInstances     PreparedComputerInstanceClient
	Log                   *slog.Logger
	AdmitInstanceStart    func(context.Context) error
	Reservations          *reservation.Ledger
	PlatformStore         cas.Reader
	RuntimeArchitecture   definition.RuntimeArchitecture
	VerifierCgroupRoot    string

	// sourceReleaseTimeout bounds each release attempt of a capture-owned
	// source; zero means the prepared-machine control timeout.
	sourceReleaseTimeout time.Duration

	computerDevices   map[preparedMachineRef]vm.ComputerDevice
	mu                sync.Mutex
	closeMu           sync.Mutex
	entries           map[string][]preparedMachineEntry
	filling           map[string]int
	claims            map[preparedMachineRef]*machineClaim
	claimGen          uint64
	ctx               context.Context
	cancel            context.CancelFunc
	activity          int
	activityWake      chan struct{}
	closed            bool
	verifiedRuntimes  map[artifact.RuntimeDescriptor]artifact.RuntimeIndex
	programDescriptor artifact.ProgramDescriptor
	programIndex      *artifact.ProgramIndex
}

type preparedMachineEntry struct {
	machine            liveCaptureMachine
	machineKey         string
	computerInstanceID string
	workerEpoch        int64
	target             workerapi.InstanceReconcileTarget
	exit               *preparedMachineSignal
	ready              *preparedMachineSignal
}

type preparedMachineRef struct {
	id    string
	epoch int64
}

// machineClaimKind names the holder of a checked-out prepared machine.
type machineClaimKind uint8

const (
	// serverClaim is held by a Server through its machineCheckout.
	serverClaim machineClaimKind = iota + 1
	// captureClaim is held by checkpoint capture, which then owns source
	// exclusion and the Instance's closure report.
	captureClaim
	// orphanClaim has no holder: its Server gave it up, or physical cleanup
	// took it over, while the mount's save owner had not joined. It keeps the
	// mount so that only physical cleanup, which joins that save owner before
	// finalizing, ends it.
	orphanClaim
)

// machineClaim is one exclusive claim on a checked-out prepared machine. Each
// holder gets a fresh gen; a handle acts only while its gen is current, so a
// stale handle never affects a later claim on the same key.
type machineClaim struct {
	gen   uint64
	kind  machineClaimKind
	entry preparedMachineEntry
	// mount is a served machine's managed mount. Capture releases the source
	// through it so Computer saves are joined before the machine closes.
	mount *instanceMount
	// checkpointer is retained from the start of physical capture until source
	// exclusion and checkpoint staging cleanup have succeeded.
	checkpointer *computerCheckpointer
	// teardown marks a Server claim whose holder has committed to closing the
	// machine, releasing the claim and reporting the Instance itself. Capture
	// may not take it over.
	teardown bool
	// release is the claim's single active resource release. Every other
	// releaser joins it instead of releasing the same resources again.
	release *claimRelease
}

type claimRelease struct {
	done chan struct{}
	err  error
}

// beginReleaseLocked makes the caller the claim's release owner.
func (c *machineClaim) beginReleaseLocked() *claimRelease {
	c.release = &claimRelease{done: make(chan struct{})}
	return c.release
}

type preparedMachineSignal struct {
	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error
}

func newPreparedMachineSignal() *preparedMachineSignal {
	return &preparedMachineSignal{done: make(chan struct{})}
}

func (s *preparedMachineSignal) finish(err error) {
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

func (s *preparedMachineSignal) wait(ctx context.Context) (error, bool) {
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

func (s *preparedMachineSignal) finished() (error, bool) {
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

func NewPreparedMachines(backend vm.Backend, store cas.Store, size int, log *slog.Logger) *PreparedMachines {
	ctx, cancel := context.WithCancel(context.Background())
	return &PreparedMachines{
		Backend:      backend,
		CAS:          store,
		Size:         size,
		Log:          log,
		entries:      map[string][]preparedMachineEntry{},
		filling:      map[string]int{},
		claims:       map[preparedMachineRef]*machineClaim{},
		ctx:          ctx,
		cancel:       cancel,
		activityWake: make(chan struct{}),
	}
}

// checkout claims the prepared machine reserved for mount. On success the
// caller holds the returned checkout and must end it with Release or Relinquish.
func (p *PreparedMachines) checkout(ctx context.Context, mount workerapi.ComputerInstanceAssignment) (*machineCheckout, string, bool) {
	if p == nil || p.Size <= 0 {
		return nil, "", false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := computerInstanceIDFromComputerMount(mount)
	computerInstanceID := strings.TrimSpace(mount.ComputerInstanceID)
	if computerInstanceID == "" {
		p.logInfo("prepared machines miss", "reason", "computer_instance_missing")
		return nil, key, false
	}
	if mount.WorkerEpoch <= 0 {
		p.logInfo("prepared machines miss", "computer_instance_id", computerInstanceID, "reason", "worker_epoch_missing")
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
		if entries[i].computerInstanceID == computerInstanceID && entries[i].workerEpoch == mount.WorkerEpoch {
			index = i
			break
		}
	}
	if index < 0 {
		p.mu.Unlock()
		p.logInfo("prepared machines miss", "computer_instance_id", computerInstanceID, "worker_epoch", mount.WorkerEpoch, "reason", "reserved_machine_missing")
		return nil, key, false
	}
	entry := entries[index]
	if entry.target.Source.Computer == nil || entry.target.Source.ComputerID != mount.ComputerID ||
		entry.target.Source.Computer.VersionID != mount.Target.BaseComputerDiskVersionID || strings.TrimSpace(mount.Target.BaseComputerDiskVersionID) == "" {
		p.mu.Unlock()
		p.logInfo("prepared machines miss", "computer_instance_id", computerInstanceID, "reason", "computer_source_mismatch")
		return nil, key, false
	}
	if err, exited := entry.exit.finished(); exited {
		p.mu.Unlock()
		p.removeReadyEntryAndFail(key, entry, preparedMachineExitCause(err), true)
		p.logInfo("prepared machines miss", "computer_instance_id", computerInstanceID, "reason", "reserved_machine_exited")
		return nil, key, false
	}
	p.mu.Unlock()
	if err, readyFinished := entry.ready.wait(ctx); err != nil {
		reason := "instance_ready_failed"
		if readyFinished {
			if p.forgetReadyEntry(key, entry) {
				p.cleanupClaimedEntryAsync(entry, err)
			}
		} else {
			reason = "instance_ready_wait_canceled"
		}
		p.logInfo("prepared machines miss", "computer_instance_id", computerInstanceID, "reason", reason, "error", err.Error())
		return nil, key, false
	}
	if err, exited := entry.exit.finished(); exited {
		if p.forgetReadyEntry(key, entry) {
			p.cleanupClaimedEntryAsync(entry, preparedMachineExitCause(err))
		}
		p.logInfo("prepared machines miss", "computer_instance_id", computerInstanceID, "reason", "reserved_machine_exited", "error", errorString(err))
		return nil, key, false
	}
	p.mu.Lock()
	entries = p.entries[key]
	index = -1
	for i := range entries {
		if entries[i].computerInstanceID == computerInstanceID && entries[i].workerEpoch == mount.WorkerEpoch {
			index = i
			break
		}
	}
	if index < 0 {
		p.mu.Unlock()
		p.logInfo("prepared machines miss", "computer_instance_id", computerInstanceID, "worker_epoch", mount.WorkerEpoch, "reason", "reserved_machine_claimed")
		return nil, key, false
	}
	entry = entries[index]
	if err, exited := entry.exit.finished(); exited {
		p.removeReadyEntryAtLocked(key, entries, index)
		p.mu.Unlock()
		p.cleanupClaimedEntryAsync(entry, preparedMachineExitCause(err))
		p.logInfo("prepared machines miss", "computer_instance_id", computerInstanceID, "reason", "reserved_machine_exited", "error", errorString(err))
		return nil, key, false
	}
	p.removeReadyEntryAtLocked(key, entries, index)
	ref := preparedMachineRef{id: computerInstanceID, epoch: mount.WorkerEpoch}
	claim := p.claimLocked(ref, serverClaim, entry)
	claim.mount = newInstanceMount(entry.machine)
	checkout := &machineCheckout{
		machines: p, ref: ref, gen: claim.gen, machine: entry.machine, mount: claim.mount,
		writerGeneration: entry.target.Source.WriterGeneration,
	}
	if entry.target.Source.Restore != nil {
		checkout.restoreCheckpointID = strings.TrimSpace(entry.target.Source.Restore.CheckpointID)
	}
	available := p.readyCountLocked()
	p.mu.Unlock()
	p.logInfo("prepared machines hit", "computer_instance_id", computerInstanceID, "available", available)
	return checkout, key, true
}

func (p *PreparedMachines) ReconcileDesiredInstances(ctx context.Context, client InstanceReconcileClient) error {
	if p == nil || p.Size <= 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	if client == nil {
		return errors.New("instance reconcile client is required")
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	active := make(map[preparedMachineRef]struct{}, p.Size)
	decisions := make(chan bool, p.Size)
	results := make(chan instanceReconcileResult, p.Size)
	var attempts sync.WaitGroup
	stop := func(err error) error {
		cancel()
		attempts.Wait()
		return err
	}
	handleResult := func(result instanceReconcileResult) error {
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
		p.logInfo("instance reconciliation failed", "error", result.err.Error())
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

		response, err := client.ListInstanceReconcileTargets(workCtx)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stop(ctxErr)
		}
		delay := time.Second
		if err != nil {
			p.logInfo("instance desired-state poll failed", "error", err.Error())
		} else {
			launched := 0
			for _, target := range response.Items {
				if len(active) >= p.Size {
					break
				}
				ref := preparedMachineRef{id: strings.TrimSpace(target.ID), epoch: target.WorkerEpoch}
				if _, ok := active[ref]; ok {
					continue
				}
				active[ref] = struct{}{}
				launched++
				attempts.Go(func() {
					admitted := false
					err := p.reconcileInstanceTarget(workCtx, client, target, func() {
						admitted = true
						decisions <- true
					})
					if !admitted {
						decisions <- false
					}
					results <- instanceReconcileResult{ref: ref, err: err}
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

func (p *PreparedMachines) reconcileInstanceTarget(
	ctx context.Context,
	client PreparedComputerInstanceClient,
	target workerapi.InstanceReconcileTarget,
	admitted func(),
) error {
	switch {
	case target.Action == workerapi.InstanceReconcileCapture:
		admitted()
		return p.captureInstanceTarget(ctx, client, target)
	case target.Action == workerapi.InstanceReconcileReclaim:
		admitted()
		return p.reclaimFailedInstanceTarget(ctx, client, target)
	case target.Action == workerapi.InstanceReconcileClose:
		admitted()
		return p.stopInstanceTarget(ctx, client, target)
	case target.Action == workerapi.InstanceReconcilePrepare:
		return p.warmInstanceTarget(ctx, client, target, admitted)
	default:
		return fmt.Errorf("unsupported instance reconcile action %q", target.Action)
	}
}

func (p *PreparedMachines) reclaimFailedInstanceTarget(ctx context.Context, client PreparedComputerInstanceClient, target workerapi.InstanceReconcileTarget) error {
	if p == nil || client == nil {
		return errors.New("failed instance reclaim requires prepared machines and control plane client")
	}
	computerInstanceID := strings.TrimSpace(target.ID)
	if computerInstanceID == "" || target.WorkerEpoch <= 0 {
		return errors.New("failed instance reclaim target id and worker_epoch are required")
	}
	if p.Backend == nil {
		return errors.New("VM backend does not support exact failed-instance cleanup")
	}
	// One critical section decides who owns the instance before it is stopped:
	// a ready entry is taken, a live Server claim is orphaned, and a committed
	// Server teardown is left alone. A checkout cannot slip in between.
	p.mu.Lock()
	if claim := p.claims[preparedMachineRef{id: computerInstanceID, epoch: target.WorkerEpoch}]; claim != nil && claim.kind == serverClaim {
		if claim.teardown || claim.release != nil {
			// The Server has committed to closing, releasing and reporting
			// this instance itself, within its bounded teardown. Racing it would
			// duplicate that work, so this attempt does nothing; a later one
			// finds the claim ended, or orphaned if the Server's close failed.
			p.mu.Unlock()
			return errors.New("instance is being torn down by its server")
		}
		// Revoke the Server's claim before stopping its machine, so the Server
		// cannot observe the exit and then report or release the instance.
		p.orphanLocked(claim)
	}
	entry, ready := p.claimReadyEntryLocked(computerInstanceID, target.WorkerEpoch)
	p.mu.Unlock()
	var closeErr error
	if ready && entry.machine != nil {
		closeCtx, cancel := preparedMachineControlContext(ctx)
		closeErr = entry.machine.Close(closeCtx)
		cancel()
	}
	cleanupCtx, cancel := preparedMachineControlContext(ctx)
	err := p.Backend.Cleanup(cleanupCtx, vm.Owner{Kind: vm.OwnerRuntime, ID: computerInstanceID})
	cancel()
	if err != nil {
		return fmt.Errorf("reconcile failed instance physical cleanup: %w", errors.Join(closeErr, err))
	}
	if err := p.releaseInstanceAfterPhysicalCleanup(ctx, computerInstanceID, target.WorkerEpoch); err != nil {
		return err
	}
	request := instanceTargetStatusRequest(target, errors.New("instance physical cleanup reconciled"))
	request.CleanupProof = &workerapi.InstanceCleanupProof{Method: workerapi.InstanceCleanupHostReconciled, CompletedAt: time.Now().UTC()}
	if _, err := client.MarkComputerInstanceFailed(ctx, request); err != nil {
		return fmt.Errorf("persist failed instance cleanup proof: %w", err)
	}
	return nil
}

func (p *PreparedMachines) stopInstanceTarget(ctx context.Context, client PreparedComputerInstanceClient, target workerapi.InstanceReconcileTarget) error {
	if p == nil {
		return nil
	}
	computerInstanceID := strings.TrimSpace(target.ID)
	if computerInstanceID == "" {
		return errors.New("instance stop target id is required")
	}
	if target.WorkerEpoch <= 0 {
		return errors.New("instance stop target worker_epoch is required")
	}
	p.mu.Lock()
	var capture *computerCheckpointer
	orphaned := false
	if claim := p.claims[preparedMachineRef{id: computerInstanceID, epoch: target.WorkerEpoch}]; claim != nil {
		capture = claim.checkpointer
		orphaned = claim.kind == orphanClaim
	}
	p.mu.Unlock()
	if capture != nil {
		proofMethod, err := p.excludeCaptureSource(ctx, computerInstanceID, capture)
		if err != nil {
			return err
		}
		if err := p.releaseInstanceAfterPhysicalCleanup(ctx, computerInstanceID, target.WorkerEpoch); err != nil {
			return err
		}
		request := instanceTargetStatusRequest(target, nil)
		request.CleanupProof = &workerapi.InstanceCleanupProof{Method: proofMethod, CompletedAt: time.Now().UTC()}
		_, err = client.MarkComputerInstanceClosed(ctx, request)
		return err
	}
	if orphaned {
		// No holder will close this instance; stop it physically and finalize
		// once its save owner has joined.
		if p.Backend == nil {
			return errors.New("VM backend does not support exact instance cleanup")
		}
		cleanupCtx, cancel := preparedMachineControlContext(ctx)
		err := p.Backend.Cleanup(cleanupCtx, vm.Owner{Kind: vm.OwnerRuntime, ID: computerInstanceID})
		cancel()
		if err != nil {
			return fmt.Errorf("reconcile instance physical cleanup: %w", err)
		}
		if err := p.releaseInstanceAfterPhysicalCleanup(ctx, computerInstanceID, target.WorkerEpoch); err != nil {
			return err
		}
		request := instanceTargetStatusRequest(target, nil)
		request.CleanupProof = &workerapi.InstanceCleanupProof{Method: workerapi.InstanceCleanupHostReconciled, CompletedAt: time.Now().UTC()}
		_, err = client.MarkComputerInstanceClosed(ctx, request)
		return err
	}
	stoppedEntry, ok := p.claimReadyEntry(computerInstanceID, target.WorkerEpoch)
	proofMethod := ""
	if !ok {
		if p.instanceCheckedOut(computerInstanceID, target.WorkerEpoch) {
			return nil
		}
		if p.Backend == nil {
			return errors.New("VM backend does not support exact instance cleanup")
		}
		cleanupCtx, cancel := preparedMachineControlContext(ctx)
		err := p.Backend.Cleanup(cleanupCtx, vm.Owner{Kind: vm.OwnerRuntime, ID: computerInstanceID})
		cancel()
		if err != nil {
			return fmt.Errorf("reconcile instance physical cleanup: %w", err)
		}
		if err := p.releaseInstanceCapacity(computerInstanceID, target.WorkerEpoch); err != nil {
			return err
		}
		proofMethod = workerapi.InstanceCleanupHostReconciled
	} else if stoppedEntry.machine != nil {
		if err := stoppedEntry.machine.Close(ctx); err != nil {
			return p.markInstanceTargetFailed(ctx, client, target, err)
		}
		if err := p.releaseInstanceCapacity(computerInstanceID, target.WorkerEpoch); err != nil {
			return err
		}
		proofMethod = workerapi.InstanceCleanupMachineClosed
	} else {
		if p.Backend == nil {
			return errors.New("VM backend does not support exact instance cleanup")
		}
		cleanupCtx, cancel := preparedMachineControlContext(ctx)
		err := p.Backend.Cleanup(cleanupCtx, vm.Owner{Kind: vm.OwnerRuntime, ID: computerInstanceID})
		cancel()
		if err != nil {
			return fmt.Errorf("reconcile instance physical cleanup: %w", err)
		}
		if err := p.releaseInstanceCapacity(computerInstanceID, target.WorkerEpoch); err != nil {
			return err
		}
		proofMethod = workerapi.InstanceCleanupHostReconciled
	}
	request := instanceTargetStatusRequest(target, nil)
	request.CleanupProof = &workerapi.InstanceCleanupProof{Method: proofMethod, CompletedAt: time.Now().UTC()}
	_, err := client.MarkComputerInstanceClosed(ctx, request)
	if err != nil {
		return err
	}
	p.logInfo("instance desired close reconciled", "computer_instance_id", computerInstanceID)
	return nil
}

func (p *PreparedMachines) warmInstanceTarget(
	ctx context.Context,
	client PreparedComputerInstanceClient,
	target workerapi.InstanceReconcileTarget,
	admitted func(),
) error {
	if p == nil {
		return nil
	}
	if client == nil {
		return errors.New("prepared machine instance client is required")
	}
	if err := validateComputerPreparationSource(target); err != nil {
		return err
	}
	if target.Source.Restore != nil {
		if _, err := validatePreparedMachineRestore(target, p.RuntimeArchitecture); err != nil {
			return err
		}
	}
	if target.PreparationExpiresAt.IsZero() {
		return errors.New("instance preparation deadline is required")
	}
	ctx, cancelPrepare := context.WithDeadline(ctx, target.PreparationExpiresAt)
	defer cancelPrepare()
	if p.AdmitInstanceStart != nil {
		if err := p.AdmitInstanceStart(ctx); err != nil {
			return err
		}
	}
	mount := preparedMachineComputerMountFromSource(target.Source)
	if strings.TrimSpace(mount.ComputerSpecID) == "" {
		return errors.New("prepared machine warm command source is required")
	}
	mount.ComputerInstanceID = strings.TrimSpace(target.ID)
	key := computerInstanceIDFromComputerMount(mount)
	computerInstanceID := strings.TrimSpace(target.ID)
	workerEpoch := target.WorkerEpoch
	if computerInstanceID == "" || workerEpoch <= 0 {
		return errors.New("instance reconcile target id and worker_epoch are required")
	}
	if p.Size <= 0 || p.Backend == nil || p.CAS == nil {
		reason := errors.New("prepared machines are not configured")
		p.logInfo("prepared machine warm skipped", "computer_instance_id", key, "reason", reason.Error())
		stateCtx, cancelState := preparedMachineControlContext(ctx)
		defer cancelState()
		return p.markInstanceTargetFailedWithProof(stateCtx, client, target, reason, workerapi.InstanceCleanupNotMaterialized)
	}
	refillCtx, cancelRefill := p.withMachinesContext(ctx)
	defer cancelRefill()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		reason := errors.New("prepared machines closed")
		p.logInfo("prepared machine warm skipped", "computer_instance_id", key, "reason", reason.Error())
		stateCtx, cancelState := preparedMachineControlContext(ctx)
		defer cancelState()
		return p.markInstanceTargetFailedWithProof(stateCtx, client, target, reason, workerapi.InstanceCleanupNotMaterialized)
	}
	if p.reservedCountLocked() >= p.Size {
		p.mu.Unlock()
		p.logInfo("prepared machine warm deferred", "computer_instance_id", key, "reason", errPreparedMachineCapacityBusy.Error())
		return errPreparedMachineCapacityBusy
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

func (p *PreparedMachines) Close(ctx context.Context) error {
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
	var closingEntries []preparedMachineEntry
	for key, keyEntries := range p.entries {
		closingEntries = append(closingEntries, keyEntries...)
		delete(p.entries, key)
	}
	p.mu.Unlock()
	var err error
	for _, entry := range closingEntries {
		if closeErr := entry.machine.Close(ctx); closeErr != nil {
			transitionErr := p.transitionInstanceTargetFailed(ctx, entry.target, closeErr)
			p.retainCloseRetry(entry)
			err = errors.Join(err, closeErr, transitionErr)
			continue
		}
		if releaseErr := p.releaseInstanceCapacity(entry.computerInstanceID, entry.workerEpoch); releaseErr != nil {
			err = errors.Join(err, releaseErr)
		}
		if closeErr := p.transitionInstanceTargetFailed(ctx, entry.target, errors.New("instance controller stopped")); closeErr != nil {
			p.retainCloseRetry(entry)
			err = errors.Join(err, closeErr)
		}
	}
	if waitErr := p.waitForActivity(ctx); waitErr != nil {
		err = errors.Join(err, waitErr)
	}
	return err
}

func (p *PreparedMachines) transitionInstanceTargetFailed(ctx context.Context, target workerapi.InstanceReconcileTarget, failure error) error {
	if p.ComputerInstances == nil {
		return errors.New("prepared machine instance client is required")
	}
	_, err := p.ComputerInstances.MarkComputerInstanceFailed(ctx, instanceTargetStatusRequest(target, failure))
	return err
}

func (p *PreparedMachines) retainCloseRetry(entry preparedMachineEntry) {
	if entry.machine == nil {
		return
	}
	p.mu.Lock()
	key := entry.machineKey
	if strings.TrimSpace(key) == "" {
		key = entry.computerInstanceID
	}
	p.entries[key] = append(p.entries[key], entry)
	p.mu.Unlock()
}

func (p *PreparedMachines) beginActivityLocked() bool {
	if p.closed {
		return false
	}
	p.activity++
	return true
}

func (p *PreparedMachines) endActivity() {
	p.mu.Lock()
	if p.activity > 0 {
		p.activity--
	}
	close(p.activityWake)
	p.activityWake = make(chan struct{})
	p.mu.Unlock()
}

func (p *PreparedMachines) waitForActivity(ctx context.Context) error {
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
			return fmt.Errorf("prepared machines close timed out with %d background tasks: %w", remaining, ctx.Err())
		case <-wake:
		}
	}
}

func (p *PreparedMachines) prepareAndStore(
	ctx context.Context,
	key string,
	mount workerapi.ComputerInstanceAssignment,
	target workerapi.InstanceReconcileTarget,
	admitted func(),
) (retErr error) {
	computerInstanceID := strings.TrimSpace(target.ID)
	workerEpoch := target.WorkerEpoch
	materializeAttempted := false
	failInstance := func(err error) error {
		if err == nil {
			return nil
		}
		stateCtx, cancelState := preparedMachineControlContext(ctx)
		defer cancelState()
		proofMethod := ""
		if !materializeAttempted {
			if closeErr := p.releaseComputerDevice(computerInstanceID, workerEpoch); closeErr != nil {
				failure := errors.Join(err, closeErr)
				return errors.Join(failure, p.reportInstanceTargetFailedWithProof(stateCtx, p.ComputerInstances, target, failure, ""))
			}
			proofMethod = workerapi.InstanceCleanupNotMaterialized
		}
		if markErr := p.reportInstanceTargetFailedWithProof(stateCtx, p.ComputerInstances, target, err, proofMethod); markErr != nil {
			p.logInfo("prepared machines instance fail transition failed", "computer_instance_id", computerInstanceID, "error", markErr.Error())
			return markErr
		}
		var fatal interface{ FatalWorker() bool }
		if errors.As(err, &fatal) && fatal.FatalWorker() {
			return err
		}
		return nil
	}
	topology := vm.Topology{Computer: &vm.ComputerDisk{
		ComputerID: target.Source.ComputerID,
		VersionID:  target.Source.Computer.VersionID, SizeBytes: target.Source.Computer.LogicalBytes,
	}}
	if err := p.reserveInstanceCapacity(target, topology); err != nil {
		if errors.Is(err, errPreparedMachineCapacityBusy) {
			return err
		}
		return failInstance(err)
	}
	defer func() {
		if !materializeAttempted {
			retErr = errors.Join(retErr, p.releaseInstanceCapacity(computerInstanceID, workerEpoch))
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
		if errors.Is(err, errPreparedMachineCapacityBusy) {
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
	var machine vm.Machine
	var materializeErr error
	phases := &phaseCollector{}
	phaseLogMessage := "prepared machine phase"
	if target.Source.Restore != nil {
		phaseLogMessage = "prepared restored instance phase"
		machine, materializeErr = p.restorePreparedMachine(ctx, target, topology, readOnlyDrives, phases.Record)
	} else {
		machine, materializeErr = p.Backend.Materialize(ctx, vm.MaterializeRequest{
			ID: computerInstanceID, OwnerKind: vm.OwnerRuntime, RootfsDigest: mount.RootfsDigest,
			Binding:           instanceTargetWorkloadBinding(target),
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
	if err != nil && machine != nil {
		err = errors.Join(err, p.closeMachine(ctx, machine))
	}
	p.logInfo("prepared machine materialized", "computer_instance_id", computerInstanceID, "duration_ms", time.Since(started).Milliseconds(), "error", errorString(err))
	if err != nil {
		return failInstance(err)
	}
	keepMachine := false
	defer func() {
		if !keepMachine {
			if closeErr := p.closeMachine(ctx, machine); closeErr == nil {
				_ = p.releaseInstanceCapacity(computerInstanceID, workerEpoch)
			}
		}
	}()
	live, ok := machine.(liveCaptureMachine)
	if !ok {
		return failInstance(errors.New("machine cannot capture a live Computer"))
	}
	if target.Source.Restore == nil {
		if err := p.prepareGuestRuntime(ctx, machine, key, target.Source.WriterGeneration, mount, "", mountedImageConfig); err != nil {
			p.logInfo("prepared machines guest prepare failed", "computer_instance_id", computerInstanceID, "error", err.Error())
			return failInstance(err)
		}
	}
	entry := preparedMachineEntry{
		machine:            live,
		machineKey:         key,
		computerInstanceID: computerInstanceID,
		workerEpoch:        workerEpoch,
		target:             target,
		exit:               newPreparedMachineSignal(),
		ready:              newPreparedMachineSignal(),
	}
	p.mu.Lock()
	closed := p.closed
	capacityBusy := !closed && p.readyCountLocked() >= p.Size
	if closed || capacityBusy {
		p.mu.Unlock()
		if capacityBusy {
			closeCtx, cancelClose := preparedMachineControlContext(ctx)
			closeErr := machine.Close(closeCtx)
			cancelClose()
			if closeErr != nil {
				return failInstance(closeErr)
			}
			if err := p.releaseInstanceCapacity(computerInstanceID, workerEpoch); err != nil {
				return failInstance(err)
			}
			keepMachine = true
			p.logInfo("prepared machine warm deferred", "computer_instance_id", computerInstanceID, "reason", errPreparedMachineCapacityBusy.Error())
			return errPreparedMachineCapacityBusy
		}
		stateCtx, cancelState := preparedMachineControlContext(ctx)
		defer cancelState()
		if err := p.markInstanceTargetFailed(stateCtx, p.ComputerInstances, target, errors.New("prepared machines capacity changed")); err != nil {
			p.logInfo("prepared machines instance close transition failed", "computer_instance_id", computerInstanceID, "error", err.Error())
			return err
		}
		return nil
	}
	p.entries[key] = append(p.entries[key], entry)
	p.monitorReadyEntryLocked(key, entry)
	p.mu.Unlock()
	keepMachine = true
	if err, exited := entry.exit.finished(); exited {
		entry.ready.finish(preparedMachineExitCause(err))
		if failErr := p.removeReadyEntryAndFail(key, entry, preparedMachineExitCause(err), true); failErr != nil {
			return errors.Join(preparedMachineExitCause(err), failErr)
		}
		return nil
	}
	readyRequest := instanceTargetStatusRequest(target, nil)
	readyRequest.VMVCPUCount = target.Source.VMVCPUCount
	readyRequest.CPUConfigDigest = target.Source.CPUConfigDigest
	readyCtx, cancelReady := preparedMachineControlContext(ctx)
	_, readyErr := p.ComputerInstances.MarkComputerInstanceReady(readyCtx, readyRequest)
	cancelReady()
	if err := readyErr; err != nil {
		entry.ready.finish(err)
		p.logInfo("prepared machines instance ready transition failed", "computer_instance_id", computerInstanceID, "error", err.Error())
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
		if candidate.computerInstanceID == entry.computerInstanceID && candidate.workerEpoch == entry.workerEpoch {
			stillReady = true
			break
		}
	}
	p.mu.Unlock()
	if !stillReady {
		return nil
	}
	p.logInfo("prepared machines refilled", "computer_instance_id", computerInstanceID, "available", available)
	return nil
}

func instanceTargetWorkloadBinding(target workerapi.InstanceReconcileTarget) vm.WorkloadBinding {
	return vm.WorkloadBinding{
		WorkerEpoch:        target.WorkerEpoch,
		OwnerID:            target.ID,
		Generation:         1,
		ComputerInstanceID: target.ID,
		VMPlatformID:       target.Source.VMPlatformID,
	}
}

func (p *PreparedMachines) prepareProgram(
	ctx context.Context,
	tempDir string,
	target workerapi.InstanceReconcileTarget,
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
	runtimeSnapshot, err := snapshot.ReadRuntime(
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
	p.logInfo("prepared machine artifact verified",
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
	programSnapshot, err := snapshot.ReadProgram(
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
func (p *PreparedMachines) verifyProgram(
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

func (p *PreparedMachines) verifyRuntime(
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

func (p *PreparedMachines) monitorReadyEntryLocked(key string, entry preparedMachineEntry) {
	if p == nil || p.closed || entry.machine == nil || entry.exit == nil {
		return
	}
	if !p.beginActivityLocked() {
		return
	}
	go func() {
		defer p.endActivity()
		err := entry.machine.Wait(p.ctx)
		entry.exit.finish(err)
		entry.ready.finish(preparedMachineExitCause(err))
		if p.ctx != nil && p.ctx.Err() != nil && errors.Is(err, context.Canceled) {
			return
		}
		p.removeReadyEntryAndFail(key, entry, preparedMachineExitCause(err), false)
	}()
}

func preparedMachineExitCause(err error) error {
	if err != nil {
		return err
	}
	return errors.New("prepared machine exited")
}

func (p *PreparedMachines) readyCountLocked() int {
	total := 0
	for _, entries := range p.entries {
		total += len(entries)
	}
	return total
}

func (p *PreparedMachines) fillingCountLocked() int {
	total := 0
	for _, count := range p.filling {
		total += count
	}
	return total
}

func (p *PreparedMachines) decrementFillingLocked(key string) {
	count := p.filling[key] - 1
	if count <= 0 {
		delete(p.filling, key)
		return
	}
	p.filling[key] = count
}

func (p *PreparedMachines) reservedCountLocked() int {
	return p.readyCountLocked() + p.fillingCountLocked() + len(p.claims)
}

// claimLocked records a new claim on ref with a fresh generation.
func (p *PreparedMachines) claimLocked(ref preparedMachineRef, kind machineClaimKind, entry preparedMachineEntry) *machineClaim {
	if p.claims == nil {
		p.claims = map[preparedMachineRef]*machineClaim{}
	}
	p.claimGen++
	claim := &machineClaim{gen: p.claimGen, kind: kind, entry: entry}
	p.claims[ref] = claim
	return claim
}

func (p *PreparedMachines) instanceCheckedOut(computerInstanceID string, workerEpoch int64) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.claims[preparedMachineRef{id: computerInstanceID, epoch: workerEpoch}] != nil
}

// machineCheckout is a Server's handle on its claim of a prepared machine taken
// from PreparedMachines. While the claim is held, instance reconciliation
// (stopInstanceTarget) leaves the instance to its holder. The holder closes the
// machine through its mount and then ends the claim with Release when the close
// succeeded, or with Relinquish when it failed. Checkpoint capture may take the
// claim over, and forced reclaim may orphan it; from then on the handle is
// stale and every operation on it is a no-op. The writer generation and restore
// provenance are those the instance was prepared with.
type machineCheckout struct {
	machines            *PreparedMachines
	ref                 preparedMachineRef
	gen                 uint64
	machine             liveCaptureMachine
	mount               *instanceMount
	writerGeneration    int64
	restoreCheckpointID string
}

// Machine returns the checked-out instance's machine, which the prepared machines admitted
// only once it could capture its live Computer.
func (c *machineCheckout) Machine() liveCaptureMachine {
	if c == nil {
		return nil
	}
	return c.machine
}

// beginTeardown commits the holder to closing the machine, ending the claim and
// reporting the Instance itself; capture can no longer take the claim over. It
// reports false when the claim is no longer this handle's: capture took it
// over, forced reclaim orphaned it, physical cleanup is releasing it, or it has
// ended. Repeated calls by the holder report true. A nil checkout holds no
// prepared machine, so its Server always owns its teardown.
func (c *machineCheckout) beginTeardown() bool {
	if c == nil {
		return true
	}
	p := c.machines
	p.mu.Lock()
	defer p.mu.Unlock()
	claim := p.claims[c.ref]
	if claim == nil || claim.gen != c.gen || claim.release != nil {
		return false
	}
	claim.teardown = true
	return true
}

// Release returns the instance's capacity reservations, preparation directories
// and Computer device after its machine has been closed. The claim ends even when
// a release step fails: the remaining resources stay recorded against the
// instance, and ending the claim is what lets instance reconciliation clean them
// up, since stopInstanceTarget skips instances that are still checked out.
// Releasing a claim that has ended, been taken over or is already being
// released by physical cleanup returns nil. A nil checkout holds nothing, so
// releasing it returns nil; this serves a mount without prepared machines.
func (c *machineCheckout) Release() error {
	if c == nil {
		return nil
	}
	p := c.machines
	p.mu.Lock()
	claim := p.claims[c.ref]
	if claim == nil || claim.gen != c.gen || claim.release != nil {
		p.mu.Unlock()
		return nil
	}
	release := claim.beginReleaseLocked()
	p.mu.Unlock()
	err := p.releaseInstanceCapacity(c.ref.id, c.ref.epoch)
	p.finishRelease(c.ref, claim, release, err, true)
	return err
}

// Relinquish gives up the claim without releasing anything, for an instance
// whose machine could not be closed. Capacity and device ownership stay
// reserved until instance reconciliation proves physical cleanup. If the
// mount's save owner has not joined, the claim stays as an orphan that keeps
// the mount, so reconciliation joins that save owner before finalizing;
// otherwise the claim ends. Relinquishing a claim that has ended, been taken
// over or is being released does nothing, and so does relinquishing a nil
// checkout.
func (c *machineCheckout) Relinquish() {
	if c == nil {
		return
	}
	p := c.machines
	p.mu.Lock()
	defer p.mu.Unlock()
	claim := p.claims[c.ref]
	if claim == nil || claim.gen != c.gen || claim.release != nil {
		return
	}
	if claim.mount != nil && !claim.mount.saves.joined() {
		p.orphanLocked(claim)
		return
	}
	delete(p.claims, c.ref)
}

// orphanLocked leaves claim without a holder: every handle on it becomes stale
// and only physical cleanup can end it.
func (p *PreparedMachines) orphanLocked(claim *machineClaim) {
	p.claimGen++
	claim.gen = p.claimGen
	claim.kind = orphanClaim
}

// finishRelease publishes the release result to joined releasers and ends the
// claim, unless end is false and the claim is kept for a retry.
func (p *PreparedMachines) finishRelease(ref preparedMachineRef, claim *machineClaim, release *claimRelease, err error, end bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	release.err = err
	close(release.done)
	if p.claims[ref] != claim {
		return
	}
	if end {
		delete(p.claims, ref)
		return
	}
	claim.release = nil
}

func (p *PreparedMachines) removeReadyEntryAndFail(key string, entry preparedMachineEntry, cause error, closeMachine bool) error {
	if !p.forgetReadyEntry(key, entry) {
		return nil
	}
	if closeMachine && entry.machine != nil {
		if closeErr := p.closeMachine(context.Background(), entry.machine); closeErr == nil {
			if releaseErr := p.releaseInstanceCapacity(entry.computerInstanceID, entry.workerEpoch); releaseErr != nil {
				cause = errors.Join(cause, releaseErr)
			}
		} else {
			cause = errors.Join(cause, closeErr)
		}
	}
	stateCtx, cancelState := preparedMachineControlContext(context.Background())
	defer cancelState()
	if err := p.markInstanceTargetFailed(stateCtx, p.ComputerInstances, entry.target, cause); err != nil {
		p.logInfo("prepared machines instance fail transition failed", "computer_instance_id", entry.computerInstanceID, "error", err.Error())
		return err
	}
	p.logInfo("prepared machines entry failed", "computer_instance_id", entry.computerInstanceID, "error", errorString(cause))
	return nil
}

func (p *PreparedMachines) cleanupClaimedEntryAsync(entry preparedMachineEntry, cause error) {
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

func (p *PreparedMachines) cleanupClaimedEntry(entry preparedMachineEntry, cause error) {
	if entry.machine != nil {
		if closeErr := p.closeMachine(context.Background(), entry.machine); closeErr == nil {
			if releaseErr := p.releaseInstanceCapacity(entry.computerInstanceID, entry.workerEpoch); releaseErr != nil {
				cause = errors.Join(cause, releaseErr)
			}
		} else {
			cause = errors.Join(cause, closeErr)
		}
	}
	stateCtx, cancelState := preparedMachineControlContext(context.Background())
	defer cancelState()
	if err := p.markInstanceTargetFailed(stateCtx, p.ComputerInstances, entry.target, cause); err != nil {
		p.logInfo("prepared machines instance fail transition failed", "computer_instance_id", entry.computerInstanceID, "error", err.Error())
		return
	}
	p.logInfo("prepared machines claimed entry failed", "computer_instance_id", entry.computerInstanceID, "error", errorString(cause))
}

func (p *PreparedMachines) forgetReadyEntry(key string, entry preparedMachineEntry) bool {
	removed := false
	p.mu.Lock()
	entries := p.entries[key]
	for i := range entries {
		if entries[i].computerInstanceID == entry.computerInstanceID && entries[i].workerEpoch == entry.workerEpoch {
			p.removeReadyEntryAtLocked(key, entries, i)
			removed = true
			break
		}
	}
	p.mu.Unlock()
	return removed
}

func (p *PreparedMachines) claimReadyEntry(computerInstanceID string, workerEpoch int64) (preparedMachineEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.claimReadyEntryLocked(computerInstanceID, workerEpoch)
}

func (p *PreparedMachines) claimReadyEntryLocked(computerInstanceID string, workerEpoch int64) (preparedMachineEntry, bool) {
	for key, entries := range p.entries {
		for i, entry := range entries {
			if entry.computerInstanceID != computerInstanceID || entry.workerEpoch != workerEpoch {
				continue
			}
			p.removeReadyEntryAtLocked(key, entries, i)
			return entry, true
		}
	}
	return preparedMachineEntry{}, false
}

func (p *PreparedMachines) removeReadyEntryAtLocked(key string, entries []preparedMachineEntry, index int) {
	entries = append(entries[:index], entries[index+1:]...)
	if len(entries) == 0 {
		delete(p.entries, key)
		return
	}
	p.entries[key] = entries
}

func preparedMachineComputerMountFromSource(source workerapi.InstanceSource) workerapi.ComputerInstanceAssignment {
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

func (p *PreparedMachines) prepareGuestRuntime(ctx context.Context, machine vm.Machine, key string, writerGeneration int64, mount workerapi.ComputerInstanceAssignment, computerImagePath string, mountedImageConfig *computerv0.RuntimeImageConfig) error {
	stream, err := machine.OpenStream(ctx)
	if err != nil {
		return fmt.Errorf("open prepared machine stream: %w", err)
	}
	defer stream.Close()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:       wire.StreamTypeComputerRuntimePrepare,
		ComputerID: mount.ComputerID,
	}, 0); err != nil {
		return fmt.Errorf("write prepared machine header: %w", err)
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
		return fmt.Errorf("write prepared machine request: %w", err)
	}
	started := time.Now()
	if mountedImageConfig == nil {
		if err := writeFileFrameWithMetadataContext(ctx, machine, stream, wire.StreamHeader{
			Type:       wire.StreamTypeRunImage,
			ComputerID: mount.ComputerID,
		}, computerImagePath, strings.TrimSpace(mount.ComputerImage.Digest), mount.ComputerImage.SizeBytes); err != nil {
			// The guest can reject the request while the host is still streaming a
			// large image. Preserve the guest's structured failure instead of
			// reporting only the resulting broken pipe.
			responseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var response computerv0.PrepareComputerRuntimeResponse
			if responseErr := readProtoFrameFromReaderContext(responseCtx, machine, stream, &response); responseErr == nil {
				if phaseError := computerMountPhaseError(response.GetPhases()); phaseError != "" {
					return fmt.Errorf("prepared machine rejected computer image: %s: %w", phaseError, err)
				}
				return fmt.Errorf("prepared machine returned state %q while writing computer image: %w", response.GetStatus(), err)
			}
			return fmt.Errorf("write prepared machine computer image: %w", err)
		}
		p.logInfo("prepared machines computer image sent", "computer_instance_id", key, "duration_ms", time.Since(started).Milliseconds(), "size_bytes", mount.ComputerImage.SizeBytes)
	}

	var response computerv0.PrepareComputerRuntimeResponse
	started = time.Now()
	if err := readProtoFrameFromReaderContext(ctx, machine, stream, &response); err != nil {
		return fmt.Errorf("read prepared machine response: %w", err)
	}
	p.logInfo("prepared machines response read", "computer_instance_id", key, "duration_ms", time.Since(started).Milliseconds(), "state", strings.TrimSpace(response.Status))
	for _, guestPhase := range response.GetPhases() {
		if guestPhase == nil {
			continue
		}
		p.logInfo("prepared machines guest phase",
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
			return fmt.Errorf("prepared machine returned state %q: %s", response.Status, phaseError)
		}
		return fmt.Errorf("prepared machine returned state %q", response.Status)
	}
	if strings.TrimSpace(response.ComputerInstanceId) != key {
		return errors.New("prepared machine instance id mismatch")
	}
	return nil
}

func (p *PreparedMachines) withMachinesContext(parent context.Context) (context.Context, context.CancelFunc) {
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

func preparedMachineControlContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	} else {
		parent = context.WithoutCancel(parent)
	}
	return context.WithTimeout(parent, defaultPreparedMachineControlTimeout)
}

func (p *PreparedMachines) reserveInstanceCapacity(
	target workerapi.InstanceReconcileTarget,
	topologies ...vm.Topology,
) error {
	if p == nil || p.Reservations == nil {
		return errors.New("prepared machine capacity ledger is required")
	}
	projectionBytes := int64(0)
	if len(topologies) > 1 {
		return errors.New("instance capacity accepts at most one topology")
	}
	if len(topologies) == 1 && topologies[0].Computer != nil {
		projectionBytes = topologies[0].Computer.SizeBytes
	}
	retained, staging, err := p.checkpointRestoreCapacity(target)
	if err != nil {
		return err
	}
	if retained > math.MaxInt64-projectionBytes {
		return reservation.ErrOverflow
	}
	projectionBytes += retained
	request, err := instanceReservationVectorWithProjection(
		int64(target.Source.ReservedCPUMillis),
		int64(target.Source.ReservedMemoryMiB),
		target.Source.ReservedDiskMiB,
		projectionBytes,
	)
	if err != nil {
		return err
	}
	created, err := p.Reservations.Reserve(instanceReservationKey(target.ID, target.WorkerEpoch), request)
	if errors.Is(err, reservation.ErrCapacityExceeded) || err == nil && !created {
		return errPreparedMachineCapacityBusy
	}
	if err != nil {
		return err
	}
	if staging > 0 {
		created, err = p.Reservations.Reserve(restoreStagingKey(target.ID, target.WorkerEpoch), reservation.Vector{GuestEphemeralDiskBytes: staging})
		if err != nil || !created {
			releaseErr := p.Reservations.Release(instanceReservationKey(target.ID, target.WorkerEpoch))
			if errors.Is(err, reservation.ErrCapacityExceeded) || err == nil {
				err = errPreparedMachineCapacityBusy
			}
			return errors.Join(err, releaseErr)
		}
	}
	return err
}

func (p *PreparedMachines) releaseInstanceCapacity(computerInstanceID string, workerEpoch int64) error {
	if err := p.releaseComputerDevice(computerInstanceID, workerEpoch); err != nil {
		return err
	}
	if p == nil || p.Reservations == nil {
		return nil
	}
	if ids.Validate(computerInstanceID) == nil && workerEpoch > 0 {
		if err := os.RemoveAll(p.computerPreparationDirectory(computerInstanceID, workerEpoch)); err != nil {
			return err
		}
		if err := os.RemoveAll(p.restorePreparationDirectory(computerInstanceID, workerEpoch)); err != nil {
			return err
		}
	}
	if err := p.Reservations.Release(computerStagingKey(computerInstanceID, workerEpoch)); err != nil {
		return err
	}
	if err := p.Reservations.Release(restoreStagingKey(computerInstanceID, workerEpoch)); err != nil {
		return err
	}
	if err := p.Reservations.Release(instanceReservationKey(computerInstanceID, workerEpoch)); err != nil {
		return err
	}
	p.mu.Lock()
	delete(p.computerDevices, preparedMachineRef{id: computerInstanceID, epoch: workerEpoch})
	p.mu.Unlock()
	return nil
}

// releaseInstanceAfterPhysicalCleanup finalizes an instance whose machine has
// been stopped physically and ends whatever claim holds it.
//
// Resource finalization (releasing reservations, staging and restore
// directories, device records and the claim) happens only after the claim's
// mount save owner has joined. Physical teardown (VM stop, device close) may
// precede the join on forced paths, because it is what terminates a producer
// blocked on the VM; the producer then fails on the closed resources and
// exits. The join is bounded: if the save owner still runs, the claim and its
// resources are kept, nothing is final, and the caller retries. Callers never
// pass a live Server claim: forced cleanup orphans it before stopping the
// machine, so the Server neither reports the stop nor takes release ownership.
//
// The caller becomes the claim's single release owner; if another release of
// the claim is already running, the caller joins it and returns its result. A
// failed release keeps only a capture claim that retains checkpoint cleanup,
// for a retry; any other claim ends, leaving its resources recorded for
// unclaimed reconciliation.
func (p *PreparedMachines) releaseInstanceAfterPhysicalCleanup(ctx context.Context, computerInstanceID string, workerEpoch int64) error {
	ref := preparedMachineRef{id: strings.TrimSpace(computerInstanceID), epoch: workerEpoch}
	for {
		p.mu.Lock()
		claim := p.claims[ref]
		if claim == nil {
			p.mu.Unlock()
			return p.releaseInstanceCapacity(ref.id, ref.epoch)
		}
		if active := claim.release; active != nil {
			p.mu.Unlock()
			<-active.done
			return active.err
		}
		if mount := claim.mount; mount != nil && !mount.saves.joined() {
			p.mu.Unlock()
			if err := p.joinSaveOwner(ctx, ref.id, mount); err != nil {
				return err
			}
			continue
		}
		release := claim.beginReleaseLocked()
		capture := claim.checkpointer
		p.mu.Unlock()
		var err error
		if capture != nil {
			err = capture.cleanupAfterSourceStopped()
		}
		if err == nil {
			err = p.releaseInstanceCapacity(ref.id, ref.epoch)
		}
		p.finishRelease(ref, claim, release, err, err == nil || capture == nil)
		return err
	}
}

// joinSaveOwner quiesces and joins a mount's save owner within the source
// release bound, after its machine has been stopped physically.
func (p *PreparedMachines) joinSaveOwner(ctx context.Context, computerInstanceID string, mount *instanceMount) error {
	joinCtx, cancel := p.sourceReleaseContext(ctx)
	settleErr := mount.saves.Quiesce(joinCtx)
	cancel()
	if !mount.saves.joined() {
		return fmt.Errorf("computer save owner still runs after physical cleanup: %w", settleErr)
	}
	if settleErr != nil {
		p.logInfo("Computer save settlement failed after physical cleanup", "computer_instance_id", computerInstanceID, "error", settleErr.Error())
	}
	return nil
}

func (p *PreparedMachines) closeMachine(parent context.Context, machine vm.Machine) error {
	if machine == nil {
		return nil
	}
	ctx, cancel := preparedMachineControlContext(parent)
	defer cancel()
	return machine.Close(ctx)
}

func instanceTargetStatusRequest(target workerapi.InstanceReconcileTarget, failure error) workerapi.ComputerInstanceStateRequest {
	request := workerapi.ComputerInstanceStateRequest{
		ID: target.ID, WorkerEpoch: target.WorkerEpoch, DesiredVersion: target.DesiredVersion,
		ExpectedObservedVersion: target.ObservedVersion, ReasonCode: "desired_state_reconciled",
	}
	if failure != nil {
		request.ReasonCode = workerapi.InstanceFailureReconcile
		message := failure.Error()
		var sourceFailure *disk.SourceFailure
		if errors.As(failure, &sourceFailure) {
			request.ReasonCode = workerapi.InstanceFailureComputerSource
		}
		var fatal interface{ FatalWorker() bool }
		if errors.As(failure, &fatal) && fatal.FatalWorker() {
			request.ReasonCode = workerapi.InstanceFailureWorkerInvalid
			message = "worker runtime infrastructure failed"
		}
		request.Error, _ = json.Marshal(map[string]string{"message": message})
	}
	return request
}

func (p *PreparedMachines) markInstanceTargetFailed(ctx context.Context, client PreparedComputerInstanceClient, target workerapi.InstanceReconcileTarget, failure error) error {
	return p.markInstanceTargetFailedWithProof(ctx, client, target, failure, "")
}

func (p *PreparedMachines) markInstanceTargetFailedWithProof(ctx context.Context, client PreparedComputerInstanceClient, target workerapi.InstanceReconcileTarget, failure error, proofMethod string) error {
	if err := p.reportInstanceTargetFailedWithProof(ctx, client, target, failure, proofMethod); err != nil {
		return errors.Join(failure, err)
	}
	return failure
}

func (p *PreparedMachines) reportInstanceTargetFailedWithProof(ctx context.Context, client PreparedComputerInstanceClient, target workerapi.InstanceReconcileTarget, failure error, proofMethod string) error {
	request := instanceTargetStatusRequest(target, failure)
	if proofMethod != "" {
		request.CleanupProof = &workerapi.InstanceCleanupProof{Method: proofMethod, CompletedAt: time.Now().UTC()}
	}
	_, err := client.MarkComputerInstanceFailed(ctx, request)
	if err != nil {
		return fmt.Errorf("report instance preparation failure: %w", err)
	}
	return nil
}

func writeFileFrameWithMetadataContext(ctx context.Context, machine vm.Machine, w io.Writer, header wire.StreamHeader, path string, digest string, size int64) error {
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
		closeCtx, cancel := preparedMachineControlContext(ctx)
		defer cancel()
		_ = machine.Close(closeCtx)
		return ctx.Err()
	}
}

func (p *PreparedMachines) logInfo(message string, attrs ...any) {
	if p == nil || p.Log == nil {
		return
	}
	p.Log.Info(message, attrs...)
}

type phaseCollector struct {
	mu     sync.Mutex
	phases []vm.Phase
}

func (c *phaseCollector) Record(phase vm.Phase) {
	if c == nil || strings.TrimSpace(phase.Name) == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.phases = append(c.phases, phase)
}

func (c *phaseCollector) Snapshot() []workerapi.CheckpointPhase {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]workerapi.CheckpointPhase, 0, len(c.phases))
	for _, phase := range c.phases {
		result = append(result, workerCheckpointPhase(phase))
	}
	return result
}

func readProtoFrameFromReaderContext(
	ctx context.Context,
	machine vm.Machine,
	reader io.Reader,
	message proto.Message,
) error {
	result := make(chan error, 1)
	go func() {
		result <- frameio.ReadProtoFrame(reader, message)
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = machine.Close(context.Background())
		return ctx.Err()
	}
}
