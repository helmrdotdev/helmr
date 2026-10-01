package computerhost

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// A handle whose claim ended through physical cleanup must not act on a later
// claim of the same prepared machine key.
func TestStaleCheckoutReleaseCannotReleaseNewerClaim(t *testing.T) {
	_, mount := testComputerMountArtifacts(t)
	mount.ComputerID = "computer"
	machines := computerPreparedMachines(t, mount, &closeTrackingMachine{})
	machines.Backend = &cleanupBackend{}
	ref := preparedMachineRef{id: mount.ComputerInstanceID, epoch: mount.WorkerEpoch}
	stale, _, ok := machines.checkout(t.Context(), mount)
	if !ok {
		t.Fatal("first checkout failed")
	}
	target := instanceReservationTarget(ref.id, ref.epoch)
	target.Source.ComputerID = mount.ComputerID
	target.Source.WriterGeneration = mount.WriterGeneration
	target.Source.Computer = &workerapi.InstanceComputerSource{VersionID: mount.Target.BaseComputerDiskVersionID}
	if err := machines.reclaimFailedInstanceTarget(t.Context(), &typedInstanceClient{}, target); err != nil {
		t.Fatal(err)
	}
	if machines.instanceCheckedOut(ref.id, ref.epoch) || len(machines.Reservations.Snapshot().Reservations) != 0 {
		t.Fatal("physical cleanup did not end the first claim")
	}

	// The same key is reserved and claimed again.
	if err := machines.reserveInstanceCapacity(target); err != nil {
		t.Fatal(err)
	}
	key := computerInstanceIDFromComputerMount(mount)
	ready := newPreparedMachineSignal()
	ready.finish(nil)
	machines.mu.Lock()
	machines.entries[key] = []preparedMachineEntry{{
		machine: &closeTrackingMachine{}, machineKey: key, computerInstanceID: ref.id,
		workerEpoch: ref.epoch, target: target, exit: newPreparedMachineSignal(), ready: ready,
	}}
	machines.mu.Unlock()
	current, _, ok := machines.checkout(t.Context(), mount)
	if !ok {
		t.Fatal("second checkout failed")
	}

	if err := stale.Release(); err != nil {
		t.Fatalf("stale release = %v", err)
	}
	stale.Relinquish()
	if !machines.instanceCheckedOut(ref.id, ref.epoch) {
		t.Fatal("stale handle ended the newer claim")
	}
	if got := len(machines.Reservations.Snapshot().Reservations); got != 1 {
		t.Fatalf("reservations after stale release = %d, want the newer claim's 1", got)
	}
	if err := current.Release(); err != nil {
		t.Fatal(err)
	}
	if machines.instanceCheckedOut(ref.id, ref.epoch) || len(machines.Reservations.Snapshot().Reservations) != 0 {
		t.Fatal("current handle did not release its own claim")
	}
}

// capturedServeMachine is a served prepared machine that can also produce a
// checkpoint and live save cuts. Closing it ends Wait, as a stopped VM does.
type capturedServeMachine struct {
	*serverTestMachine
	artifact    vm.SnapshotArtifact
	save        computerSaveCapture
	failSaves   atomic.Bool
	beforeClose func()
	onSnapshot  func()
	exited      chan struct{}
	exitOnce    sync.Once
}

// exit ends the machine as a VM that stopped on its own.
func (m *capturedServeMachine) exit() { m.exitOnce.Do(func() { close(m.exited) }) }

func (m *capturedServeMachine) Close(ctx context.Context) error {
	if m.beforeClose != nil {
		m.beforeClose()
	}
	err := m.serverTestMachine.Close(ctx)
	m.exit()
	return err
}

func (m *capturedServeMachine) Wait(ctx context.Context) error {
	select {
	case <-m.exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *capturedServeMachine) SnapshotLimits() (vm.SnapshotLimits, error) {
	return vm.SnapshotLimits{ComputerBytes: 4096, MemoryBytes: 4096, ScratchBytes: 4096, StateBytes: 10000000, ConfigBytes: 65536}, nil
}

func (m *capturedServeMachine) CreateSnapshot(context.Context, vm.SnapshotRequest) (vm.SnapshotArtifact, error) {
	if m.onSnapshot != nil {
		m.onSnapshot()
	}
	return m.artifact, nil
}

func (m *capturedServeMachine) CaptureComputer(context.Context) (*vm.ComputerSnapshot, error) {
	if m.save == nil || m.failSaves.Load() {
		return nil, errTestLiveCapture
	}
	return &vm.ComputerSnapshot{ComputerID: m.artifact.Computer.ComputerID, Capture: m.save}, nil
}

func (m *capturedServeMachine) isClosed() bool {
	select {
	case <-m.exited:
		return true
	default:
		return false
	}
}

type capturedServe struct {
	target   workerapi.InstanceReconcileTarget
	machine  *capturedServeMachine
	machines *PreparedMachines
	mounts   *Mounts
	client   *serverTestClient
	captures *checkpointReconcileClient
	served   chan error
}

type capturedServeOptions struct {
	members   int
	saves     func(workerapi.ComputerInstanceAssignment) ComputerSaveClient
	saveEvery time.Duration
	heartbeat time.Duration
	// failureTimeout bounds the Server's own cleanup; zero keeps its default.
	failureTimeout time.Duration
	// control wraps the Server's control plane client.
	control func(*serverTestClient) workerapi.ComputerServerControlPlaneClient
}

// startCapturedServe serves a checked-out prepared machine and returns once
// the mount is registered and its save loop is running.
func startCapturedServe(ctx context.Context, t *testing.T, machine *capturedServeMachine, options capturedServeOptions) *capturedServe {
	t.Helper()
	members := options.members
	if options.saves == nil {
		options.saves = func(workerapi.ComputerInstanceAssignment) ComputerSaveClient { return &saveHostFixture{} }
	}
	if options.saveEvery == 0 {
		options.saveEvery = time.Hour
	}
	if options.heartbeat == 0 {
		options.heartbeat = time.Hour
	}
	store, mount := testComputerMountArtifacts(t)
	target := checkpointCaptureTarget(members)
	target.ID, target.WorkerEpoch = mount.ComputerInstanceID, mount.WorkerEpoch
	mount.OrgID = uuid.NewV7().String()
	mount.ComputerID = target.Source.ComputerID
	mount.WriterGeneration = target.Source.WriterGeneration
	mount.GuestChannelCredential = "channel-credential"
	mount.GuestChannelCredentialHash = sha256sum.HexBytes([]byte(mount.GuestChannelCredential))
	preparedClient, preparedServer := net.Pipe()
	t.Cleanup(func() { _ = preparedServer.Close() })
	go acknowledgePreparedComputerMount(t, preparedServer, mount, mount.ComputerInstanceID)
	machine.serverTestMachine = &serverTestMachine{streams: []io.ReadWriteCloser{preparedClient}, operation: checkpointFreezeStream(t, target)}
	machine.artifact = checkpointArtifact(t)
	machine.exited = make(chan struct{})

	prepared := instanceReservationTarget(target.ID, target.WorkerEpoch)
	prepared.Source.ComputerID = mount.ComputerID
	prepared.Source.WriterGeneration = mount.WriterGeneration
	prepared.Source.Computer = &workerapi.InstanceComputerSource{VersionID: mount.Target.BaseComputerDiskVersionID}
	machines := NewPreparedMachines(nil, nil, 1, nil)
	ledger, err := reservation.New(reservation.Vector{CPUMillis: 2000, MemoryBytes: 2 << 30, GuestEphemeralDiskBytes: 4 << 30, VMSlots: 1})
	if err != nil {
		t.Fatal(err)
	}
	machines.Reservations = ledger
	if err := machines.reserveInstanceCapacity(prepared); err != nil {
		t.Fatal(err)
	}
	key := computerInstanceIDFromComputerMount(mount)
	ready := newPreparedMachineSignal()
	ready.finish(nil)
	machines.entries[key] = []preparedMachineEntry{{
		machine: machine, machineKey: key, computerInstanceID: target.ID,
		workerEpoch: target.WorkerEpoch, target: prepared,
		exit: newPreparedMachineSignal(), ready: ready,
	}}
	captures := &checkpointReconcileClient{target: target}
	machines.ComputerCaptures = &CaptureRuns{}
	machines.Checkpoints = captures
	machines.CheckpointEncryptor = testCheckpointEncryptor(t)
	machines.ComputerObjects = &captureStore{}
	machines.TempDir = t.TempDir()

	mounted := make(chan struct{})
	client := &serverTestClient{onReady: func() { close(mounted) }}
	var control workerapi.ComputerServerControlPlaneClient = client
	if options.control != nil {
		control = options.control(client)
	}
	mounts := NewMounts()
	server := Server{
		RestoreControl: unusedComputerRestoreControl{}, ComputerSaves: options.saves(mount), ComputerSaveEvery: options.saveEvery,
		ComputerObjects: &checkpointCAS{}, Mounts: mounts, CAS: store, TempDir: t.TempDir(),
		Heartbeat: options.heartbeat, PollEvery: time.Hour, Machines: machines,
		FailureTimeout: options.failureTimeout,
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx, mount, control) }()
	select {
	case <-mounted:
	case err := <-served:
		t.Fatalf("serve ended before mounting: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return &capturedServe{target: target, machine: machine, machines: machines, mounts: mounts, client: client, captures: captures, served: served}
}

func (s *capturedServe) awaitServed(ctx context.Context, t *testing.T) error {
	t.Helper()
	select {
	case err := <-s.served:
		return err
	case <-ctx.Done():
		t.Fatal("serve did not end after its source was released for checkpoint")
		return nil
	}
}

// Capture of a served Instance takes over the Server's claim and ends the
// mount as a checkpoint release: the machine closes once, only the capture
// reports closure, and the paused member is released as detached.
func TestCaptureExclusionEndsMountAsCheckpointRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	machine := &capturedServeMachine{}
	s := startCapturedServe(ctx, t, machine, capturedServeOptions{members: 1})
	wait := captureRegistryWait(t, s.machines.ComputerCaptures, s.target, s.target.Capture.Runs[0])
	settled := make(chan error, 1)
	go func() {
		pause := <-wait.Pauses()
		settled <- pause.Settle(nil)
	}()

	if err := s.machines.captureInstanceTarget(ctx, s.captures, s.target); err != nil {
		t.Fatal(err)
	}
	if err := s.awaitServed(ctx, t); err != nil {
		t.Fatalf("serve = %v, want a quiet checkpoint release", err)
	}
	select {
	case err := <-settled:
		if err != nil {
			t.Fatalf("member settle = %v, want detached", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if closes := machine.closeCount(); closes != 1 {
		t.Fatalf("physical closes = %d, want 1", closes)
	}
	if s.captures.ready != 1 || s.captures.closed != 1 {
		t.Fatalf("checkpoint ready=%d instance closed by capture=%d", s.captures.ready, s.captures.closed)
	}
	if len(s.client.closed) != 0 || len(s.client.failures) != 0 || len(s.captures.instanceFailures) != 0 {
		t.Fatalf("server closed=%+v server failures=%+v capture failures=%+v", s.client.closed, s.client.failures, s.captures.instanceFailures)
	}
	if s.machines.instanceCheckedOut(s.target.ID, s.target.WorkerEpoch) || len(s.machines.Reservations.Snapshot().Reservations) != 0 {
		t.Fatal("capture did not release the taken-over claim")
	}
}

// delayedAckSaves commits the first save's adoption acknowledgement but holds
// its response until the caller gives up; replays answer immediately.
type delayedAckSaves struct {
	runtime, computer string
	mu                sync.Mutex
	acks              int
	inFlight          bool
	acked             chan struct{}
}

func (s *delayedAckSaves) BeginComputerSave(_ context.Context, r workerapi.ComputerSaveBeginRequest) (workerapi.ComputerSaveBeginResponse, error) {
	return workerapi.ComputerSaveBeginResponse{ComputerInstanceID: s.runtime, SaveID: r.SaveID, Sequence: r.Sequence, WriterGeneration: r.WriterGeneration, PredecessorID: uuid.NewV7().String(), DesiredVersion: 1}, nil
}
func (s *delayedAckSaves) RegisterComputerSaveObject(context.Context, workerapi.ComputerSaveObjectRequest) error {
	return nil
}
func (s *delayedAckSaves) CertifyComputerSaveObject(context.Context, workerapi.ComputerSaveObjectRequest) error {
	return nil
}
func (s *delayedAckSaves) ReuseComputerSaveObject(context.Context, workerapi.ComputerSaveObjectRequest) error {
	return nil
}
func (s *delayedAckSaves) PublishComputerSave(_ context.Context, r workerapi.ComputerSavePublicationRequest) (workerapi.ComputerSavePublicationResponse, error) {
	return workerapi.ComputerSavePublicationResponse{ComputerID: s.computer, VersionID: r.Save.SaveID}, nil
}
func (s *delayedAckSaves) AdoptComputerSave(ctx context.Context, _ workerapi.ComputerSavePublicationRequest) error {
	s.mu.Lock()
	s.acks++
	first := s.acks == 1
	if first {
		s.inFlight = true
		close(s.acked)
	}
	s.mu.Unlock()
	if !first {
		return nil
	}
	<-ctx.Done()
	s.mu.Lock()
	s.inFlight = false
	s.mu.Unlock()
	return ctx.Err()
}
func (s *delayedAckSaves) AbandonComputerSave(context.Context, workerapi.ComputerSaveBeginRequest) error {
	return errors.New("committed save must not be abandoned")
}
func (s *delayedAckSaves) ackInFlight() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlight
}

// closedVersionSave is a live save cut whose local maintenance fails once
// its machine's disk version has been closed.
type closedVersionSave struct {
	machine         *capturedServeMachine
	collectedClosed atomic.Bool
}

func (c *closedVersionSave) Root() disk.VersionRoot {
	return disk.VersionRoot{FormatVersion: 1, LogicalBytes: 1 << 20}
}
func (c *closedVersionSave) Publish(context.Context, disk.ContinuationPublication) error {
	return nil
}
func (c *closedVersionSave) Release()                         {}
func (c *closedVersionSave) Adopt(context.Context, int) error { return nil }
func (c *closedVersionSave) Collect(context.Context, int) (int64, error) {
	if c.machine.isClosed() {
		c.collectedClosed.Store(true)
		return 0, os.ErrClosed
	}
	return 0, nil
}

// A save whose adoption was committed but whose response is still in flight
// must be joined before the checkpoint source is physically closed, so its
// local completion never runs against a closed version.
func TestCaptureReleaseJoinsSaveOwnerBeforePhysicalClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	machine := &capturedServeMachine{}
	save := &closedVersionSave{machine: machine}
	machine.save = save
	saves := &delayedAckSaves{acked: make(chan struct{})}
	var closedDuringSave atomic.Bool
	machine.beforeClose = func() {
		if saves.ackInFlight() {
			closedDuringSave.Store(true)
		}
	}
	s := startCapturedServe(ctx, t, machine, capturedServeOptions{saveEvery: time.Millisecond, saves: func(mount workerapi.ComputerInstanceAssignment) ComputerSaveClient {
		saves.runtime, saves.computer = mount.ComputerInstanceID, mount.ComputerID
		return saves
	}})
	select {
	case <-saves.acked:
	case <-ctx.Done():
		t.Fatal("save did not reach its adoption acknowledgement")
	}

	if err := s.machines.captureInstanceTarget(ctx, s.captures, s.target); err != nil {
		t.Fatal(err)
	}
	served := s.awaitServed(ctx, t)
	if closedDuringSave.Load() {
		t.Fatal("checkpoint source closed while a committed save was still settling")
	}
	if served != nil {
		t.Fatalf("serve = %v", served)
	}
	for _, failure := range s.client.failures {
		if strings.Contains(string(failure.Error), "computer_preservation_failed") {
			t.Fatalf("successful save reported as preservation failure: %s", failure.Error)
		}
	}
	if save.collectedClosed.Load() {
		t.Fatal("save maintenance ran against the closed version")
	}
	if closes := machine.closeCount(); closes != 1 {
		t.Fatalf("physical closes = %d, want 1", closes)
	}
}

// switchedRenewal answers renewals normally until the test switches it to
// report the Instance closed or to fail.
type switchedRenewal struct {
	*serverTestClient
	mode atomic.Int32
}

const (
	renewalClosed int32 = iota + 1
	renewalFails
)

func (c *switchedRenewal) RenewComputerInstance(ctx context.Context, r workerapi.ComputerInstanceRenewRequest) (workerapi.ComputerInstanceRenewResponse, error) {
	switch c.mode.Load() {
	case renewalClosed:
		return workerapi.ComputerInstanceRenewResponse{ComputerInstanceID: r.ComputerInstanceID, WriterGeneration: r.WriterGeneration, DesiredState: "closed", DesiredVersion: 2, ObservedVersion: 1}, nil
	case renewalFails:
		return workerapi.ComputerInstanceRenewResponse{}, errors.New("renewal lost")
	}
	return c.serverTestClient.RenewComputerInstance(ctx, r)
}

func claimState(p *PreparedMachines, ref preparedMachineRef) (uint64, machineClaimKind) {
	p.mu.Lock()
	defer p.mu.Unlock()
	claim := p.claims[ref]
	if claim == nil {
		return 0, 0
	}
	return claim.gen, claim.kind
}

// Once capture has taken the claim over, nothing the Server observes makes it
// close the source or report the Instance; capture's exclusion is the one
// closure report.
func TestServerDefersToCaptureAfterTakeover(t *testing.T) {
	for _, event := range []string{"vm exits", "renewal closes", "renewal fails", "save fails", "program start fails"} {
		t.Run(event, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			machine := &capturedServeMachine{}
			// Only the save failure case runs a live save loop.
			saveEvery := time.Hour
			if event == "save fails" {
				machine.save = &closedVersionSave{machine: machine}
				saveEvery = time.Millisecond
			}
			var control *switchedRenewal
			s := startCapturedServe(ctx, t, machine, capturedServeOptions{
				heartbeat: 5 * time.Millisecond,
				saveEvery: saveEvery,
				saves: func(mount workerapi.ComputerInstanceAssignment) ComputerSaveClient {
					return &saveHostFixture{runtime: mount.ComputerInstanceID, computer: mount.ComputerID}
				},
				control: func(client *serverTestClient) workerapi.ComputerServerControlPlaneClient {
					control = &switchedRenewal{serverTestClient: client}
					return control
				},
			})
			ref := preparedMachineRef{id: s.target.ID, epoch: s.target.WorkerEpoch}
			serverGen, _ := claimState(s.machines, ref)
			var served error
			machine.onSnapshot = func() {
				if gen, kind := claimState(s.machines, ref); gen == serverGen || kind != captureClaim {
					t.Errorf("claim not taken over before the snapshot: gen=%d (server %d) kind=%d", gen, serverGen, kind)
				}
				switch event {
				case "vm exits":
					machine.exit()
				case "renewal closes":
					control.mode.Store(renewalClosed)
				case "renewal fails":
					control.mode.Store(renewalFails)
				case "save fails":
					machine.failSaves.Store(true)
				case "program start fails":
					if err := s.mounts.RequestFailure(ctx, s.target.ID); err == nil {
						t.Error("program start failure was handled by a Server that no longer owns the source")
					}
				}
				select {
				case served = <-s.served:
				case <-time.After(5 * time.Second):
					t.Error("server did not end after capture took over its claim")
				}
			}
			captureErr := s.machines.captureInstanceTarget(ctx, s.captures, s.target)
			if event == "save fails" {
				// The failed cut cannot be settled, so the managed release reports
				// it and capture keeps the claim for reconciliation.
				if captureErr == nil || !captureRetained(s.machines, ref) {
					t.Fatalf("capture = %v, want an unsettled-save release failure", captureErr)
				}
			} else if captureErr != nil {
				t.Fatal(captureErr)
			}
			if event != "renewal fails" && event != "save fails" && served != nil {
				t.Fatalf("serve = %v, want a quiet exit", served)
			}
			if len(s.client.failures) != 0 || len(s.client.closed) != 0 {
				t.Fatalf("server failures=%+v closed=%+v", s.client.failures, s.client.closed)
			}
			if closes := machine.closeCount(); closes != 1 {
				t.Fatalf("physical closes = %d, want 1", closes)
			}
			if event == "save fails" {
				return
			}
			if s.captures.ready != 1 || s.captures.closed != 1 || len(s.captures.instanceFailures) != 0 {
				t.Fatalf("capture ready=%d closed=%d failures=%+v", s.captures.ready, s.captures.closed, s.captures.instanceFailures)
			}
			if s.machines.instanceCheckedOut(ref.id, ref.epoch) || len(s.machines.Reservations.Snapshot().Reservations) != 0 {
				t.Fatal("capture did not release the claim")
			}
		})
	}
}

// A capture-owned source whose release fails stays with capture: the Server
// neither reports nor gives up the claim, and the checkpointer is retained for
// the retry.
func TestCaptureOwnsFailedSourceRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	machine := &capturedServeMachine{}
	s := startCapturedServe(ctx, t, machine, capturedServeOptions{})
	stopErr := errors.New("VM stop unproven")
	machine.mu.Lock()
	machine.closeErr = stopErr
	machine.mu.Unlock()
	if err := s.machines.captureInstanceTarget(ctx, s.captures, s.target); !errors.Is(err, stopErr) {
		t.Fatalf("capture = %v, want source release failure", err)
	}
	if err := s.awaitServed(ctx, t); err != nil {
		t.Fatalf("serve = %v", err)
	}
	if len(s.client.failures) != 0 || len(s.client.closed) != 0 || s.captures.closed != 0 {
		t.Fatalf("server failures=%+v closed=%+v capture closed=%d", s.client.failures, s.client.closed, s.captures.closed)
	}
	ref := preparedMachineRef{id: s.target.ID, epoch: s.target.WorkerEpoch}
	if !captureRetained(s.machines, ref) || len(s.machines.Reservations.Snapshot().Reservations) == 0 {
		t.Fatal("failed exclusion lost its capture claim or resources")
	}
	if machine.closeCount() != 1 {
		t.Fatalf("physical closes = %d, want 1", machine.closeCount())
	}
}

// A Server that has begun closing its machine keeps the claim: capture
// refuses the source instead of taking over mid-close, whether the teardown
// began before capture started or while capture waited for its members.
func TestServerTeardownRefusesCaptureTakeover(t *testing.T) {
	for _, when := range []string{"before capture", "during member pause"} {
		t.Run(when, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			serveCtx, stopServe := context.WithCancel(ctx)
			defer stopServe()
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			machine := &capturedServeMachine{}
			var enterOnce sync.Once
			machine.beforeClose = func() {
				enterOnce.Do(func() { close(entered) })
				<-release
			}
			members := 0
			if when == "during member pause" {
				members = 1
			}
			s := startCapturedServe(serveCtx, t, machine, capturedServeOptions{members: members})
			teardown := func() {
				stopServe()
				select {
				case <-entered:
				case <-ctx.Done():
					t.Error("server teardown did not reach its close")
				}
			}
			settled := make(chan error, 1)
			if members == 0 {
				teardown()
			} else {
				wait := captureRegistryWait(t, s.machines.ComputerCaptures, s.target, s.target.Capture.Runs[0])
				go func() {
					pause := <-wait.Pauses()
					teardown()
					settled <- pause.Settle(nil)
				}()
			}
			captured := make(chan error, 1)
			go func() { captured <- s.machines.captureInstanceTarget(ctx, s.captures, s.target) }()
			select {
			case err := <-captured:
				if err == nil {
					t.Fatal("capture took over a claim its Server was closing")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("capture did not refuse a claim its Server was closing")
			}
			releaseOnce.Do(func() { close(release) })
			_ = s.awaitServed(ctx, t)
			if members == 0 && s.captures.failed != 0 {
				t.Fatal("capture refused before starting still failed the checkpoint")
			}
			if members != 0 {
				if err := <-settled; err == nil {
					t.Fatal("member detached from a capture that never owned the source")
				}
			}
			if s.captures.registered != 0 || s.captures.ready != 0 || s.captures.closed != 0 {
				t.Fatalf("capture acted on the source: %+v", s.captures)
			}
			if machine.closeCount() != 1 || s.machines.instanceCheckedOut(s.target.ID, s.target.WorkerEpoch) || len(s.machines.Reservations.Snapshot().Reservations) != 0 {
				t.Fatalf("server teardown incomplete: closes=%d", machine.closeCount())
			}
		})
	}
}

type barrierComputerDevice struct {
	vm.ComputerDevice
	entered chan struct{}
	release chan struct{}
	closes  atomic.Int32
}

func (d *barrierComputerDevice) Close(context.Context) error {
	d.closes.Add(1)
	d.entered <- struct{}{}
	<-d.release
	return nil
}

// Resource release has one owner per claim: physical cleanup joins a holder's
// active release, a holder's release defers to physical cleanup, and neither
// finished releaser touches a later claim of the same key.
func TestClaimReleaseHasOneOwner(t *testing.T) {
	for _, first := range []string{"holder", "reclaim"} {
		t.Run(first+" first", func(t *testing.T) {
			_, mount := testComputerMountArtifacts(t)
			mount.ComputerID = "computer"
			machines := computerPreparedMachines(t, mount, &closeTrackingMachine{})
			holder, _, ok := machines.checkout(t.Context(), mount)
			if !ok {
				t.Fatal("checkout failed")
			}
			ref := preparedMachineRef{id: mount.ComputerInstanceID, epoch: mount.WorkerEpoch}
			device := &barrierComputerDevice{entered: make(chan struct{}, 2), release: make(chan struct{})}
			machines.computerDevices = map[preparedMachineRef]vm.ComputerDevice{ref: device}
			held, reclaimed := make(chan error, 1), make(chan error, 1)
			releaseHolder := func() { held <- holder.Release() }
			reclaim := func() { reclaimed <- machines.releaseInstanceAfterPhysicalCleanup(t.Context(), ref.id, ref.epoch) }
			if first == "holder" {
				go releaseHolder()
				<-device.entered
				go reclaim()
				select {
				case <-device.entered:
					t.Fatal("physical cleanup released the holder's resources again")
				case err := <-reclaimed:
					t.Fatalf("physical cleanup returned before the active release: %v", err)
				case <-time.After(50 * time.Millisecond):
				}
			} else {
				go reclaim()
				<-device.entered
				if holder.beginTeardown() {
					t.Fatal("holder began teardown of a claim physical cleanup is releasing")
				}
				holder.Relinquish()
				if !machines.instanceCheckedOut(ref.id, ref.epoch) {
					t.Fatal("holder relinquished a claim physical cleanup is releasing")
				}
				go releaseHolder()
				select {
				case <-device.entered:
					t.Fatal("holder released resources physical cleanup owns")
				case err := <-held:
					if err != nil {
						t.Fatal(err)
					}
					held <- nil
				case <-time.After(time.Second):
					t.Fatal("holder waited for physical cleanup")
				}
			}
			close(device.release)
			if err := <-held; err != nil {
				t.Fatal(err)
			}
			if err := <-reclaimed; err != nil {
				t.Fatal(err)
			}
			if device.closes.Load() != 1 || machines.instanceCheckedOut(ref.id, ref.epoch) || len(machines.Reservations.Snapshot().Reservations) != 0 {
				t.Fatalf("device closes=%d", device.closes.Load())
			}

			target := instanceReservationTarget(ref.id, ref.epoch)
			target.Source.ComputerID = mount.ComputerID
			target.Source.WriterGeneration = mount.WriterGeneration
			target.Source.Computer = &workerapi.InstanceComputerSource{VersionID: mount.Target.BaseComputerDiskVersionID}
			if err := machines.reserveInstanceCapacity(target); err != nil {
				t.Fatal(err)
			}
			key := computerInstanceIDFromComputerMount(mount)
			ready := newPreparedMachineSignal()
			ready.finish(nil)
			machines.mu.Lock()
			machines.entries[key] = []preparedMachineEntry{{machine: &closeTrackingMachine{}, machineKey: key, computerInstanceID: ref.id, workerEpoch: ref.epoch, target: target, exit: newPreparedMachineSignal(), ready: ready}}
			machines.mu.Unlock()
			if _, _, ok := machines.checkout(t.Context(), mount); !ok {
				t.Fatal("key reuse checkout failed")
			}
			if err := holder.Release(); err != nil {
				t.Fatal(err)
			}
			if !machines.instanceCheckedOut(ref.id, ref.epoch) || len(machines.Reservations.Snapshot().Reservations) != 1 {
				t.Fatal("a finished releaser touched the later claim")
			}
		})
	}
}

// A failed release after physical cleanup ends a Server claim, leaving its
// resources to unclaimed reconciliation, but keeps a capture claim whose
// checkpoint cleanup still has to run.
func TestPhysicalCleanupReleaseFailure(t *testing.T) {
	t.Run("server claim ends", func(t *testing.T) {
		_, mount := testComputerMountArtifacts(t)
		mount.ComputerID = "computer"
		machines := computerPreparedMachines(t, mount, &closeTrackingMachine{})
		holder, _, ok := machines.checkout(t.Context(), mount)
		if !ok {
			t.Fatal("checkout failed")
		}
		ref := preparedMachineRef{id: mount.ComputerInstanceID, epoch: mount.WorkerEpoch}
		device := &countingCloseComputerDevice{err: errors.New("device cleanup failed")}
		machines.computerDevices = map[preparedMachineRef]vm.ComputerDevice{ref: device}
		if err := machines.releaseInstanceAfterPhysicalCleanup(t.Context(), ref.id, ref.epoch); !errors.Is(err, device.err) {
			t.Fatalf("release = %v", err)
		}
		if machines.instanceCheckedOut(ref.id, ref.epoch) || len(machines.Reservations.Snapshot().Reservations) != 1 {
			t.Fatal("failed release must end the claim and keep its resources recorded")
		}
		if err := holder.Release(); err != nil || device.closeCount() != 1 {
			t.Fatalf("stale holder release = %v, device closes = %d", err, device.closeCount())
		}
		device.mu.Lock()
		device.err = nil
		device.mu.Unlock()
		machines.Backend = &cleanupBackend{}
		control := &typedInstanceClient{}
		if err := machines.stopInstanceTarget(t.Context(), control, instanceReservationTarget(ref.id, ref.epoch)); err != nil {
			t.Fatal(err)
		}
		if len(control.closed) != 1 || len(machines.Reservations.Snapshot().Reservations) != 0 {
			t.Fatal("unclaimed reconciliation did not clean up")
		}
	})
	t.Run("capture claim retained", func(t *testing.T) {
		target := instanceReservationTarget("019c10d5-a6f7-7af1-8f5f-000000000519", 7)
		machines := NewPreparedMachines(nil, nil, 1, nil)
		machines.Reservations = newPreparedMachineReservations(t, 1)
		if err := machines.reserveInstanceCapacity(target); err != nil {
			t.Fatal(err)
		}
		ref := preparedMachineRef{id: target.ID, epoch: target.WorkerEpoch}
		cleanupErr := errors.New("staging cleanup failed")
		machines.mu.Lock()
		claim := machines.claimLocked(ref, captureClaim, preparedMachineEntry{})
		claim.checkpointer = &computerCheckpointer{pendingCleanup: func() error { return cleanupErr }}
		machines.mu.Unlock()
		if err := machines.releaseInstanceAfterPhysicalCleanup(t.Context(), ref.id, ref.epoch); !errors.Is(err, cleanupErr) {
			t.Fatalf("release = %v", err)
		}
		if !captureRetained(machines, ref) || len(machines.Reservations.Snapshot().Reservations) != 1 {
			t.Fatal("failed capture release lost its claim")
		}
		cleanupErr = nil
		if err := machines.releaseInstanceAfterPhysicalCleanup(t.Context(), ref.id, ref.epoch); err != nil {
			t.Fatal(err)
		}
		if machines.instanceCheckedOut(ref.id, ref.epoch) || len(machines.Reservations.Snapshot().Reservations) != 0 {
			t.Fatal("retried capture release did not finish")
		}
	})
}

// Capture that claims a ready machine and ends before taking over its source
// hands the machine back to the ready entries.
func TestUnstartedCaptureReturnsReadyMachine(t *testing.T) {
	for _, tc := range []struct {
		name    string
		members int
		match   bool
		session func(*testing.T, workerapi.InstanceReconcileTarget) liveCaptureMachine
	}{
		{name: "source mismatch", session: func(t *testing.T, target workerapi.InstanceReconcileTarget) liveCaptureMachine {
			return &checkpointMachine{stream: checkpointFreezeStream(t, target), artifact: checkpointArtifact(t)}
		}},
		{name: "not checkpointable", match: true, session: func(*testing.T, workerapi.InstanceReconcileTarget) liveCaptureMachine {
			return &closeTrackingMachine{}
		}},
		{name: "member not waiting", members: 1, match: true, session: func(t *testing.T, target workerapi.InstanceReconcileTarget) liveCaptureMachine {
			return &checkpointMachine{stream: checkpointFreezeStream(t, target), artifact: checkpointArtifact(t)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, mount := testComputerMountArtifacts(t)
			mount.ComputerID = "computer"
			target := checkpointCaptureTarget(tc.members)
			target.ID, target.WorkerEpoch = mount.ComputerInstanceID, mount.WorkerEpoch
			if tc.match {
				target.Source.ComputerID = mount.ComputerID
			}
			mount.WriterGeneration = target.Source.WriterGeneration
			machines := computerPreparedMachines(t, mount, tc.session(t, target))
			client := &checkpointReconcileClient{target: target}
			machines.ComputerCaptures = &CaptureRuns{}
			machines.Checkpoints = client
			machines.CheckpointEncryptor = testCheckpointEncryptor(t)
			machines.ComputerObjects = &captureStore{}
			machines.TempDir = t.TempDir()
			if err := machines.captureInstanceTarget(t.Context(), client, target); err == nil {
				t.Fatal("capture unexpectedly succeeded")
			}
			if machines.instanceCheckedOut(target.ID, target.WorkerEpoch) {
				t.Fatal("unstarted capture kept the machine claimed")
			}
			if _, _, ok := machines.checkout(t.Context(), mount); !ok {
				t.Fatal("machine did not return to the ready entries")
			}
		})
	}
}

// vmStopBackend physically stops a runtime: whatever waits on the VM is
// released by its cleanup.
type vmStopBackend struct {
	unsupportedMachineStarts
	mu      sync.Mutex
	cleaned []string
	stopped chan struct{}
	once    sync.Once
	onStop  func()
}

func (b *vmStopBackend) Cleanup(_ context.Context, owner vm.Owner) error {
	b.mu.Lock()
	b.cleaned = append(b.cleaned, owner.ID)
	b.mu.Unlock()
	b.once.Do(func() {
		close(b.stopped)
		if b.onStop != nil {
			b.onStop()
		}
	})
	return nil
}

func (b *vmStopBackend) cleanups() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.cleaned)
}

// vmBoundSave is a live save cut whose upload only ends when the VM stops.
type vmBoundSave struct {
	stopped   <-chan struct{}
	uploading chan struct{}
	once      sync.Once
}

func (c *vmBoundSave) Root() disk.VersionRoot {
	return disk.VersionRoot{FormatVersion: 1, LogicalBytes: 1 << 20}
}
func (c *vmBoundSave) Publish(context.Context, disk.ContinuationPublication) error {
	c.once.Do(func() { close(c.uploading) })
	<-c.stopped
	return errors.New("VM stopped during upload")
}
func (c *vmBoundSave) Release()                                    {}
func (c *vmBoundSave) Adopt(context.Context, int) error            { return nil }
func (c *vmBoundSave) Collect(context.Context, int) (int64, error) { return 0, nil }

// retainedTestDevice is a runtime's retained Computer device. Backend cleanup
// closes it physically, as Firecracker cleanup closes the retained device;
// Close is finalization of the device record, which must wait for the save
// owner's join.
type retainedTestDevice struct {
	vm.ComputerDevice
	t        *testing.T
	saves    *instanceComputerSaves
	closed   chan struct{}
	once     sync.Once
	released atomic.Int32
}

func (d *retainedTestDevice) stop() { d.once.Do(func() { close(d.closed) }) }

func (d *retainedTestDevice) Close(context.Context) error {
	if !d.saves.joined() {
		d.t.Error("device record finalized while the save owner still runs")
	}
	d.stop()
	d.released.Add(1)
	return nil
}

// deviceStopBackend models production runtime cleanup: it stops the VM and
// closes the runtime's retained device.
type deviceStopBackend struct {
	unsupportedMachineStarts
	device  *retainedTestDevice
	cleaned atomic.Int32
	// onStop runs while cleanup stops the VM.
	onStop func()
}

func (b *deviceStopBackend) Cleanup(context.Context, vm.Owner) error {
	b.cleaned.Add(1)
	b.device.stop()
	if b.onStop != nil {
		b.onStop()
	}
	return nil
}

// deviceBoundSave is a save cut whose upload is blocked on the VM's device.
// It fails only once the device is closed, and then only after hold opens.
type deviceBoundSave struct {
	device    *retainedTestDevice
	hold      <-chan struct{}
	uploading chan struct{}
	started   sync.Once
	observed  atomic.Bool
}

func (c *deviceBoundSave) Root() disk.VersionRoot {
	return disk.VersionRoot{FormatVersion: 1, LogicalBytes: 1 << 20}
}
func (c *deviceBoundSave) Publish(context.Context, disk.ContinuationPublication) error {
	if c.uploading != nil {
		c.started.Do(func() { close(c.uploading) })
	}
	<-c.device.closed
	c.observed.Store(true)
	<-c.hold
	// The producer takes a moment to unwind after the device closes.
	time.Sleep(20 * time.Millisecond)
	return os.ErrClosed
}
func (c *deviceBoundSave) Release()                                    {}
func (c *deviceBoundSave) Adopt(context.Context, int) error            { return nil }
func (c *deviceBoundSave) Collect(context.Context, int) (int64, error) { return 0, nil }

type retainedCapture struct {
	machines *PreparedMachines
	target   workerapi.InstanceReconcileTarget
	ref      preparedMachineRef
	raw      *closeTrackingMachine
	mount    *instanceMount
	device   *retainedTestDevice
	backend  *deviceStopBackend
	save     *deviceBoundSave
}

// newRetainedCapture is a capture claim retained after a failed exclusion,
// whose mount's save producer is uploading on the VM's device.
func newRetainedCapture(t *testing.T, hold <-chan struct{}) *retainedCapture {
	t.Helper()
	target := instanceReservationTarget("019c10d5-a6f7-7af1-8f5f-000000000521", 7)
	target.DesiredVersion, target.ObservedVersion = 3, 2
	raw := &closeTrackingMachine{}
	mount := newInstanceMount(raw)
	device := &retainedTestDevice{t: t, saves: &mount.saves, closed: make(chan struct{})}
	save := &deviceBoundSave{device: device, hold: hold}
	f := &saveHostFixture{runtime: uuid.NewV7().String(), computer: uuid.NewV7().String()}
	uploading := make(chan struct{})
	request := workerapi.ComputerSaveBeginRequest{EnvironmentID: uuid.NewV7().String(), ComputerInstanceID: f.runtime, WriterGeneration: 2, SaveID: uuid.NewV7().String(), Sequence: 1}
	pending, err := startComputerSave(t.Context(), f, f, request, f.runtime, f.computer, func(context.Context) (computerSaveCapture, error) {
		close(uploading)
		return save, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	<-uploading
	mount.saves.pending = pending
	backend := &deviceStopBackend{device: device}
	machines := NewPreparedMachines(backend, nil, 1, nil)
	machines.sourceReleaseTimeout = 50 * time.Millisecond
	machines.Reservations = newPreparedMachineReservations(t, 1)
	if err := machines.reserveInstanceCapacity(target); err != nil {
		t.Fatal(err)
	}
	ref := preparedMachineRef{id: target.ID, epoch: target.WorkerEpoch}
	machines.computerDevices = map[preparedMachineRef]vm.ComputerDevice{ref: device}
	machines.mu.Lock()
	claim := machines.claimLocked(ref, captureClaim, preparedMachineEntry{target: target, machine: raw})
	claim.mount = mount
	claim.checkpointer = &computerCheckpointer{mount: mount}
	machines.mu.Unlock()
	return &retainedCapture{machines: machines, target: target, ref: ref, raw: raw, mount: mount, device: device, backend: backend, save: save}
}

// escalationRuntimeClient serves a Close target once, then, after the close
// is reported, a Reclaim target until it is reported.
type escalationInstanceClient struct {
	mu             sync.Mutex
	closeTarget    workerapi.InstanceReconcileTarget
	reclaimTarget  workerapi.InstanceReconcileTarget
	closeServed    bool
	closed, failed []workerapi.ComputerInstanceStateRequest
	onClosed       func()
	reclaimed      chan struct{}
}

func (c *escalationInstanceClient) ListInstanceReconcileTargets(context.Context) (workerapi.InstanceReconcileResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case !c.closeServed:
		c.closeServed = true
		return workerapi.InstanceReconcileResponse{Items: []workerapi.InstanceReconcileTarget{c.closeTarget}}, nil
	case len(c.closed) > 0 && len(c.failed) == 0:
		return workerapi.InstanceReconcileResponse{Items: []workerapi.InstanceReconcileTarget{c.reclaimTarget}}, nil
	}
	return workerapi.InstanceReconcileResponse{Items: []workerapi.InstanceReconcileTarget{}}, nil
}
func (c *escalationInstanceClient) MarkComputerInstanceReady(context.Context, workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	return workerapi.ComputerInstance{}, errors.New("unexpected ready")
}
func (c *escalationInstanceClient) MarkComputerInstanceClosed(_ context.Context, r workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	c.onClosed()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = append(c.closed, r)
	return workerapi.ComputerInstance{ID: r.ID}, nil
}
func (c *escalationInstanceClient) MarkComputerInstanceFailed(_ context.Context, r workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failed = append(c.failed, r)
	if len(c.failed) == 1 {
		close(c.reclaimed)
	}
	return workerapi.ComputerInstance{ID: r.ID}, nil
}

// A retained capture claim whose save producer only ends when the VM stops
// does not wedge reconciliation: the Close target's bounded release escalates
// to physical cleanup, which closes the device the producer is blocked on;
// the producer observes it and exits, and only after its join are resources
// finalized once and closure reported with host-reconciled proof. A later
// Reclaim of the same Instance still runs.
func TestCaptureSourceReleaseEscalatesToPhysicalCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	hold := make(chan struct{})
	close(hold)
	r := newRetainedCapture(t, hold)
	closeTarget := r.target
	closeTarget.Action = workerapi.InstanceReconcileClose
	reclaimTarget := r.target
	reclaimTarget.DesiredVersion++
	reclaimTarget.Action = workerapi.InstanceReconcileReclaim
	var cleanupsAtClose int32
	var reservationsAtClose int
	client := &escalationInstanceClient{closeTarget: closeTarget, reclaimTarget: reclaimTarget, reclaimed: make(chan struct{}), onClosed: func() {
		cleanupsAtClose = r.backend.cleaned.Load()
		reservationsAtClose = len(r.machines.Reservations.Snapshot().Reservations)
	}}
	reconcileCtx, stopReconcile := context.WithCancel(ctx)
	reconciled := make(chan error, 1)
	go func() { reconciled <- r.machines.ReconcileDesiredInstances(reconcileCtx, client) }()
	select {
	case <-client.reclaimed:
	case <-ctx.Done():
		t.Fatal("reconciliation wedged on the capture source release")
	}
	stopReconcile()
	if err := <-reconciled; !errors.Is(err, context.Canceled) {
		t.Fatalf("reconcile = %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.closed) != 1 || client.closed[0].CleanupProof == nil || client.closed[0].CleanupProof.Method != workerapi.InstanceCleanupHostReconciled {
		t.Fatalf("closed = %+v, want one host-reconciled closure", client.closed)
	}
	if !r.save.observed.Load() {
		t.Fatal("save producer did not observe the closed device")
	}
	if cleanupsAtClose != 1 || reservationsAtClose != 0 || r.device.released.Load() != 1 || r.raw.closed != 0 {
		t.Fatalf("at close: cleanups=%d reservations=%d; device releases=%d machine closes=%d", cleanupsAtClose, reservationsAtClose, r.device.released.Load(), r.raw.closed)
	}
	if !r.mount.saves.joined() || r.machines.instanceCheckedOut(r.ref.id, r.ref.epoch) {
		t.Fatal("escalation left the save owner or the claim behind")
	}
}

// Reclaim, like escalation, finalizes only after the save owner has joined:
// while the producer still runs after physical cleanup, Close and Reclaim keep
// the claim and report nothing; once it exits, the next Reclaim finalizes once.
func TestReclaimJoinsSaveOwnerBeforeFinalization(t *testing.T) {
	hold := make(chan struct{})
	r := newRetainedCapture(t, hold)
	client := &typedInstanceClient{}
	closeTarget := r.target
	closeTarget.Action = workerapi.InstanceReconcileClose
	if err := r.machines.stopInstanceTarget(t.Context(), client, closeTarget); err == nil {
		t.Fatal("close finalized while the save owner still runs")
	}
	reclaimTarget := r.target
	reclaimTarget.DesiredVersion++
	reclaimTarget.Action = workerapi.InstanceReconcileReclaim
	if err := r.machines.reclaimFailedInstanceTarget(t.Context(), client, reclaimTarget); err == nil {
		t.Fatal("reclaim finalized while the save owner still runs")
	}
	if !r.save.observed.Load() {
		t.Fatal("physical cleanup did not close the device the producer uses")
	}
	if !r.machines.instanceCheckedOut(r.ref.id, r.ref.epoch) || len(r.machines.Reservations.Snapshot().Reservations) != 1 || r.device.released.Load() != 0 || len(client.closed) != 0 || len(client.failed) != 0 {
		t.Fatalf("finalized or reported while the save owner runs: closed=%d failed=%d", len(client.closed), len(client.failed))
	}
	close(hold)
	if err := r.machines.reclaimFailedInstanceTarget(t.Context(), client, reclaimTarget); err != nil {
		t.Fatal(err)
	}
	if len(client.failed) != 1 || client.failed[0].CleanupProof == nil || client.failed[0].CleanupProof.Method != workerapi.InstanceCleanupHostReconciled {
		t.Fatalf("failed = %+v, want one host-reconciled report", client.failed)
	}
	if r.machines.instanceCheckedOut(r.ref.id, r.ref.epoch) || len(r.machines.Reservations.Snapshot().Reservations) != 0 || r.device.released.Load() != 1 || len(client.closed) != 0 {
		t.Fatal("reclaim did not finalize exactly once")
	}
}

// Capture exclusion whose save producer only ends when the VM stops escalates
// the same way and reports the closure with host-reconciled proof.
func TestCaptureExclusionEscalatesWhenSaveOwnerWaitsOnVM(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	machine := &capturedServeMachine{}
	stopped := make(chan struct{})
	save := &vmBoundSave{stopped: stopped, uploading: make(chan struct{})}
	machine.save = save
	s := startCapturedServe(ctx, t, machine, capturedServeOptions{saveEvery: time.Millisecond, saves: func(mount workerapi.ComputerInstanceAssignment) ComputerSaveClient {
		return &saveHostFixture{runtime: mount.ComputerInstanceID, computer: mount.ComputerID}
	}})
	select {
	case <-save.uploading:
	case <-ctx.Done():
		t.Fatal("save did not start")
	}
	backend := &vmStopBackend{stopped: stopped, onStop: machine.exit}
	s.machines.Backend = backend
	s.machines.sourceReleaseTimeout = 50 * time.Millisecond
	if err := s.machines.captureInstanceTarget(ctx, s.captures, s.target); err != nil {
		t.Fatal(err)
	}
	if s.captures.ready != 1 || s.captures.closed != 1 || s.captures.closedProof != workerapi.InstanceCleanupHostReconciled {
		t.Fatalf("ready=%d closed=%d proof=%q", s.captures.ready, s.captures.closed, s.captures.closedProof)
	}
	if backend.cleanups() != 1 || machine.closeCount() != 0 {
		t.Fatalf("physical cleanups=%d machine closes=%d", backend.cleanups(), machine.closeCount())
	}
	if err := s.awaitServed(ctx, t); err != nil {
		t.Fatalf("serve = %v", err)
	}
	if len(s.client.failures) != 0 || len(s.client.closed) != 0 {
		t.Fatalf("server failures=%+v closed=%+v", s.client.failures, s.client.closed)
	}
	if s.machines.instanceCheckedOut(s.target.ID, s.target.WorkerEpoch) || len(s.machines.Reservations.Snapshot().Reservations) != 0 {
		t.Fatal("escalated exclusion did not release the claim")
	}
}

// Capture that ends before taking over a ready machine does not return one
// that exited meanwhile or whose prepared machines closed: it is cleaned up
// and reported failed.
func TestUnstartedCaptureCleansUpUnreturnableMachine(t *testing.T) {
	for _, reason := range []string{"exited", "closed"} {
		t.Run(reason, func(t *testing.T) {
			_, mount := testComputerMountArtifacts(t)
			mount.ComputerID = "computer"
			machine := &closeTrackingMachine{}
			machines := computerPreparedMachines(t, mount, machine)
			instances := &typedInstanceClient{}
			machines.ComputerInstances = instances
			target := checkpointCaptureTarget(0)
			target.ID, target.WorkerEpoch = mount.ComputerInstanceID, mount.WorkerEpoch
			client := &checkpointReconcileClient{target: target}
			machines.ComputerCaptures = &CaptureRuns{}
			machines.Checkpoints = client
			machines.CheckpointEncryptor = testCheckpointEncryptor(t)
			machines.ComputerObjects = &captureStore{}
			machines.TempDir = t.TempDir()
			key := computerInstanceIDFromComputerMount(mount)
			machines.mu.Lock()
			exit := machines.entries[key][0].exit
			if reason == "closed" {
				machines.closed = true
			}
			machines.mu.Unlock()
			if reason == "exited" {
				exit.finish(errors.New("VM exited"))
			}
			// The source mismatch ends capture before it takes over the machine.
			if err := machines.captureInstanceTarget(t.Context(), client, target); err == nil {
				t.Fatal("capture unexpectedly succeeded")
			}
			if err := machines.waitForActivity(t.Context()); err != nil {
				t.Fatal(err)
			}
			machines.mu.Lock()
			returned := len(machines.entries[key])
			machines.mu.Unlock()
			if returned != 0 || machines.instanceCheckedOut(target.ID, target.WorkerEpoch) {
				t.Fatal("unreturnable machine went back to the ready entries")
			}
			if machine.closed != 1 || len(machines.Reservations.Snapshot().Reservations) != 0 || len(instances.failed) != 1 {
				t.Fatalf("closes=%d reservations=%d failures=%d", machine.closed, len(machines.Reservations.Snapshot().Reservations), len(instances.failed))
			}
		})
	}
}

// startDeviceBoundServe serves a machine whose live save is blocked on the
// VM's retained device until physical cleanup closes it and hold opens.
func startDeviceBoundServe(ctx context.Context, t *testing.T, hold <-chan struct{}) (*capturedServe, *retainedTestDevice, *deviceBoundSave) {
	t.Helper()
	machine := &capturedServeMachine{}
	device := &retainedTestDevice{t: t, closed: make(chan struct{})}
	save := &deviceBoundSave{device: device, hold: hold, uploading: make(chan struct{})}
	machine.save = save
	s := startCapturedServe(ctx, t, machine, capturedServeOptions{saveEvery: time.Millisecond, failureTimeout: 50 * time.Millisecond, saves: func(mount workerapi.ComputerInstanceAssignment) ComputerSaveClient {
		return &saveHostFixture{runtime: mount.ComputerInstanceID, computer: mount.ComputerID}
	}})
	select {
	case <-save.uploading:
	case <-ctx.Done():
		t.Fatal("save did not start")
	}
	ref := preparedMachineRef{id: s.target.ID, epoch: s.target.WorkerEpoch}
	s.machines.mu.Lock()
	device.saves = &s.machines.claims[ref].mount.saves
	s.machines.computerDevices = map[preparedMachineRef]vm.ComputerDevice{ref: device}
	s.machines.mu.Unlock()
	s.machines.Backend = &deviceStopBackend{device: device}
	s.machines.sourceReleaseTimeout = 50 * time.Millisecond
	return s, device, save
}

// A Server whose close times out because its save owner has not joined
// relinquishes the claim as an orphan that keeps the mount: Reclaim, Close and
// a Capture routed to reclaim all finalize only after the producer exits,
// exactly once, and nothing is stranded.
func TestRelinquishedClaimFinalizesAfterSaveOwnerJoins(t *testing.T) {
	for _, next := range []string{"reclaim", "close", "capture"} {
		t.Run(next, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			serveCtx, stopServe := context.WithCancel(ctx)
			defer stopServe()
			hold := make(chan struct{})
			s, device, save := startDeviceBoundServe(serveCtx, t, hold)
			stopServe()
			if err := s.awaitServed(ctx, t); err == nil || !strings.Contains(err.Error(), "join Computer saves before close") {
				t.Fatalf("serve = %v, want a close that could not join saves", err)
			}
			ref := preparedMachineRef{id: s.target.ID, epoch: s.target.WorkerEpoch}
			if _, kind := claimState(s.machines, ref); kind != orphanClaim {
				t.Fatalf("relinquished claim kind = %d, want orphan", kind)
			}
			client := &typedInstanceClient{}
			target := s.target
			target.DesiredVersion++
			attempt := func() error {
				switch next {
				case "reclaim":
					target.Action = workerapi.InstanceReconcileReclaim
					return s.machines.reclaimFailedInstanceTarget(ctx, client, target)
				case "close":
					target.Action = workerapi.InstanceReconcileClose
					return s.machines.stopInstanceTarget(ctx, client, target)
				}
				return s.machines.captureInstanceTarget(ctx, client, s.target)
			}
			if err := attempt(); err == nil {
				t.Fatal("finalized while the save owner still runs")
			}
			if !save.observed.Load() {
				t.Fatal("physical cleanup did not close the device the producer uses")
			}
			if !s.machines.instanceCheckedOut(ref.id, ref.epoch) || len(s.machines.Reservations.Snapshot().Reservations) != 1 || device.released.Load() != 0 || len(client.closed)+len(client.failed) != 0 {
				t.Fatal("finalized or reported while the save owner runs")
			}
			close(hold)
			if err := attempt(); err != nil {
				t.Fatal(err)
			}
			reports := client.failed
			if next == "close" {
				reports = client.closed
			}
			if len(reports) != 1 || reports[0].CleanupProof == nil || reports[0].CleanupProof.Method != workerapi.InstanceCleanupHostReconciled || len(client.closed)+len(client.failed) != 1 {
				t.Fatalf("closed=%+v failed=%+v, want one host-reconciled report", client.closed, client.failed)
			}
			if s.machines.instanceCheckedOut(ref.id, ref.epoch) || len(s.machines.Reservations.Snapshot().Reservations) != 0 || device.released.Load() != 1 {
				t.Fatal("orphaned claim did not finalize exactly once")
			}
		})
	}
}

// Forced reclaim revokes a live Server's claim before stopping its machine:
// the Server observes the VM exit during physical cleanup without reporting a
// failure or releasing the runtime, and reclaim finalizes after the save join.
func TestReclaimRevokesServerClaimBeforePhysicalCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	hold := make(chan struct{})
	s, device, _ := startDeviceBoundServe(ctx, t, hold)
	ref := preparedMachineRef{id: s.target.ID, epoch: s.target.WorkerEpoch}
	var served error
	var serverEnded bool
	backend := s.machines.Backend.(*deviceStopBackend)
	backend.onStop = func() {
		if backend.cleaned.Load() != 1 {
			return
		}
		// The stopped VM becomes visible to the Server while cleanup runs.
		s.machine.exit()
		select {
		case served = <-s.served:
			serverEnded = true
		case <-time.After(5 * time.Second):
		}
	}
	client := &typedInstanceClient{}
	target := s.target
	target.DesiredVersion++
	target.Action = workerapi.InstanceReconcileReclaim
	if err := s.machines.reclaimFailedInstanceTarget(ctx, client, target); err == nil {
		t.Fatal("reclaim finalized while the save owner still runs")
	}
	if !serverEnded || served != nil {
		t.Fatalf("server ended=%v serve=%v, want a quiet exit during cleanup", serverEnded, served)
	}
	if len(s.client.failures) != 0 || len(s.client.closed) != 0 {
		t.Fatalf("server failures=%+v closed=%+v during reclaim", s.client.failures, s.client.closed)
	}
	if !s.machines.instanceCheckedOut(ref.id, ref.epoch) || device.released.Load() != 0 || len(client.failed) != 0 {
		t.Fatal("finalized or reported while the save owner runs")
	}
	close(hold)
	if err := s.machines.reclaimFailedInstanceTarget(ctx, client, target); err != nil {
		t.Fatal(err)
	}
	if len(client.failed) != 1 || s.machines.instanceCheckedOut(ref.id, ref.epoch) || len(s.machines.Reservations.Snapshot().Reservations) != 0 || device.released.Load() != 1 {
		t.Fatalf("reclaim did not finalize exactly once: failed=%d", len(client.failed))
	}
}

// A Server that has committed to its own teardown keeps its runtime: reclaim
// neither stops the machine nor releases or reports it until the Server's
// teardown has ended the claim.
func TestReclaimLeavesCommittedServerTeardown(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	serveCtx, stopServe := context.WithCancel(ctx)
	defer stopServe()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	machine := &capturedServeMachine{}
	var enterOnce sync.Once
	machine.beforeClose = func() {
		enterOnce.Do(func() { close(entered) })
		<-release
	}
	s := startCapturedServe(serveCtx, t, machine, capturedServeOptions{})
	backend := &cleanupBackend{}
	s.machines.Backend = backend
	stopServe()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("server teardown did not reach its close")
	}
	client := &typedInstanceClient{}
	target := s.target
	target.DesiredVersion++
	target.Action = workerapi.InstanceReconcileReclaim
	if err := s.machines.reclaimFailedInstanceTarget(ctx, client, target); err == nil {
		t.Fatal("reclaim raced the Server's committed teardown")
	}
	if len(backend.cleaned) != 0 || len(client.failed) != 0 || len(s.machines.Reservations.Snapshot().Reservations) != 1 {
		t.Fatalf("reclaim acted during Server teardown: cleaned=%v failed=%d", backend.cleaned, len(client.failed))
	}
	releaseOnce.Do(func() { close(release) })
	_ = s.awaitServed(ctx, t)
	if machine.closeCount() != 1 || s.machines.instanceCheckedOut(s.target.ID, s.target.WorkerEpoch) || len(s.machines.Reservations.Snapshot().Reservations) != 0 {
		t.Fatal("server teardown did not close and release its runtime")
	}
	if err := s.machines.reclaimFailedInstanceTarget(ctx, client, target); err != nil {
		t.Fatal(err)
	}
	if len(backend.cleaned) != 1 || len(client.failed) != 1 {
		t.Fatalf("later reclaim cleaned=%v failed=%d", backend.cleaned, len(client.failed))
	}
}

// claimCheckingBackend records whether physical cleanup ever ran while a live
// Server claim still held the runtime.
type claimCheckingBackend struct {
	unsupportedMachineStarts
	machines *PreparedMachines
	ref      preparedMachineRef
	live     atomic.Bool
	cleaned  atomic.Int32
}

func (b *claimCheckingBackend) Cleanup(context.Context, vm.Owner) error {
	b.cleaned.Add(1)
	b.machines.mu.Lock()
	defer b.machines.mu.Unlock()
	if claim := b.machines.claims[b.ref]; claim != nil && claim.kind == serverClaim {
		b.live.Store(true)
	}
	return nil
}

// Reclaim arbitrates ownership of a ready machine in one step: however a
// concurrent checkout and reclaim interleave, physical cleanup never runs
// while a live Server claim holds the runtime, and a checkout that won ends up
// with a stale handle. Arbitration is a single critical section, so no
// deterministic barrier can separate its parts; the race detector's scheduling
// reliably exposes a split arbitration within these iterations.
func TestReclaimArbitratesReadyMachineAgainstCheckout(t *testing.T) {
	for i := range 1000 {
		_, mount := testComputerMountArtifacts(t)
		mount.ComputerID = "computer"
		machines := computerPreparedMachines(t, mount, &closeTrackingMachine{})
		ref := preparedMachineRef{id: mount.ComputerInstanceID, epoch: mount.WorkerEpoch}
		backend := &claimCheckingBackend{machines: machines, ref: ref}
		machines.Backend = backend
		target := instanceReservationTarget(ref.id, ref.epoch)
		target.Action = workerapi.InstanceReconcileReclaim
		client := &typedInstanceClient{}
		start := make(chan struct{})
		var checkout *machineCheckout
		var checkedOut bool
		var reclaimErr error
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			checkout, _, checkedOut = machines.checkout(t.Context(), mount)
		})
		wg.Go(func() {
			<-start
			reclaimErr = machines.reclaimFailedInstanceTarget(t.Context(), client, target)
		})
		close(start)
		wg.Wait()
		if reclaimErr != nil {
			t.Fatalf("iteration %d: reclaim = %v", i, reclaimErr)
		}
		if backend.live.Load() {
			t.Fatalf("iteration %d: physical cleanup ran while a live Server claim held the runtime", i)
		}
		if checkedOut && checkout.beginTeardown() {
			t.Fatalf("iteration %d: checkout kept a live claim on a reclaimed runtime", i)
		}
		if backend.cleaned.Load() != 1 || len(client.failed) != 1 || len(machines.Reservations.Snapshot().Reservations) != 0 {
			t.Fatalf("iteration %d: cleaned=%d failed=%d", i, backend.cleaned.Load(), len(client.failed))
		}
	}
}
