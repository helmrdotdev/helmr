package computerhost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type allocationOwnerTestClient struct {
	nextSave       func(context.Context, workerapi.AgentSaveDiscovery) (workerapi.AgentSavePending, error)
	claimCommand   func(context.Context, workerapi.ComputerCommandClaimRequest) (workerapi.ComputerCommandClaimResponse, error)
	beforeStop     func()
	deliveryError  error
	readinessError error

	AllocatedComputerClient
	delivery                               workerapi.ComputerAllocationDelivery
	source                                 workerapi.ComputerAllocationSource
	deliveries, renewals, readiness, stops atomic.Int64
	loseStop                               atomic.Bool
	failRenew                              atomic.Bool
	closeProcesses                         atomic.Bool
}

func (c *allocationOwnerTestClient) DeliverComputerAllocation(context.Context, workerapi.AllocationIdentity) (workerapi.ComputerAllocationDelivery, error) {
	c.deliveries.Add(1)
	return c.delivery, c.deliveryError
}
func (c *allocationOwnerTestClient) ComputerAllocationSource(context.Context, workerapi.AllocationIdentity) (workerapi.ComputerAllocationSource, error) {
	return c.source, nil
}
func (c *allocationOwnerTestClient) RenewAgentComputerLease(context.Context, workerapi.AgentComputerLeaseRequest) (workerapi.AgentComputerLeaseResponse, error) {
	c.renewals.Add(1)
	if c.failRenew.Load() {
		return workerapi.AgentComputerLeaseResponse{}, &httpclient.Error{StatusCode: http.StatusServiceUnavailable}
	}
	return workerapi.AgentComputerLeaseResponse{ExpiresAt: time.Now().Add(time.Minute)}, nil
}
func (c *allocationOwnerTestClient) ObserveFreshComputerReady(context.Context, workerapi.ComputerAllocationReady) error {
	if c.readinessError != nil {
		c.readiness.Add(1)
		return c.readinessError
	}
	if c.readiness.Add(1) == 1 {
		return errors.New("readiness reply lost")
	}
	return nil
}
func (c *allocationOwnerTestClient) ObserveAgentComputerStopped(context.Context, workerapi.AgentComputerStoppedRequest) error {
	if c.beforeStop != nil {
		c.beforeStop()
	}
	c.stops.Add(1)
	if c.loseStop.Swap(false) {
		return errors.New("stop reply lost")
	}
	return nil
}
func (c *allocationOwnerTestClient) ListComputerProcesses(context.Context, workerapi.AllocationIdentity, *workerapi.ProcessIdentity) (workerapi.ComputerProcessesResponse, error) {
	if c.closeProcesses.Load() {
		return workerapi.ComputerProcessesResponse{}, &httpclient.Error{StatusCode: http.StatusConflict, Code: workerapi.AllocationClosed}
	}
	return workerapi.ComputerProcessesResponse{}, nil
}

func (c *allocationOwnerTestClient) NextAgentSave(ctx context.Context, request workerapi.AgentSaveDiscovery) (workerapi.AgentSavePending, error) {
	if c.nextSave != nil {
		return c.nextSave(ctx, request)
	}
	return workerapi.AgentSavePending{}, nil
}

type allocationOwnerTestMachine struct {
	*agentControlMachine
	unusedCheckpoint
	captureError error
	failClose    atomic.Bool
	closes       atomic.Int64
}

func (m *allocationOwnerTestMachine) WithRunningGuestControl(ctx context.Context, _ vm.GuestControlStage, run func(context.Context) error) error {
	return run(ctx)
}
func (m *allocationOwnerTestMachine) Wait(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
func (m *allocationOwnerTestMachine) Close(context.Context) error {
	m.closes.Add(1)
	if m.failClose.Load() {
		return errors.New("physical cleanup unproved")
	}
	return nil
}
func (m *allocationOwnerTestMachine) CaptureComputer(context.Context) (*vm.ComputerSnapshot, error) {
	if m.captureError != nil {
		return nil, m.captureError
	}
	return nil, errors.New("unexpected capture")
}

type allocationOwnerTestBackend struct {
	unsupportedMachineStarts
	t                *testing.T
	machine          *allocationOwnerTestMachine
	starts, cleanups atomic.Int64
	failCleanup      atomic.Bool
}

func (b *allocationOwnerTestBackend) Materialize(_ context.Context, r vm.MaterializeRequest) (vm.CheckpointableMachine, error) {
	b.starts.Add(1)
	if r.Topology.Computer == nil || r.Topology.Computer.Device == nil || len(r.ReadOnlyDrives) != 0 || r.Binding.ComputerInstanceID != r.ID || r.Binding.Generation != 1 {
		b.t.Error("invalid allocated boot topology or binding")
	}
	return b.machine, nil
}
func (b *allocationOwnerTestBackend) Cleanup(context.Context, vm.Owner) error {
	b.cleanups.Add(1)
	if b.failCleanup.Load() {
		return errors.New("backend cleanup unproved")
	}
	return nil
}

func allocationOwnerFixture(t *testing.T) (*ComputerAllocationOwner, *allocationOwnerTestClient, *allocationOwnerTestBackend) {
	t.Helper()
	identity := workerapi.AllocationIdentity{Kind: "computer", EnvironmentID: uuid.NewV7().String(), OwnerID: uuid.NewV7().String(), InstanceID: uuid.NewV7().String(), Epoch: 2}
	root := testVersionRoot(4096)
	base, err := root.Digest()
	if err != nil {
		t.Fatal(err)
	}
	client := &allocationOwnerTestClient{delivery: workerapi.ComputerAllocationDelivery{Identity: identity, Shape: workerapi.AllocationShape{CPUMillis: 1000, VCPUCount: 1, MemoryBytes: 512 << 20, ScratchBytes: 1 << 30, VMPlatformID: "platform", CPUConfigDigest: "shape"}, ExpiresAt: time.Now().Add(time.Minute), ChannelCredential: "owned", BaseVersion: base}, source: workerapi.ComputerAllocationSource{Disk: workerapi.ComputerSourceMaterial{VersionID: base, Root: root}}}
	machine := &allocationOwnerTestMachine{agentControlMachine: &agentControlMachine{handle: func(stream net.Conn) {
		header, _, err := wire.ReadStreamFrameHeader(stream)
		if err != nil {
			t.Error(err)
			return
		}
		switch header.Type {
		case wire.StreamTypeComputerRuntimePrepare:
			var r computerv0.PrepareComputerRuntimeRequest
			if err := frameio.ReadProtoFrame(stream, &r); err != nil {
				t.Error(err)
				return
			}
			if r.ComputerId != identity.OwnerID || r.WriterGeneration != identity.Epoch || r.MountedImageConfig == nil {
				t.Error("unbound runtime preparation")
			}
			if err := frameio.WriteProtoFrame(stream, &computerv0.PrepareComputerRuntimeResponse{Status: "prepared", ComputerInstanceId: identity.InstanceID}); err != nil {
				t.Error(err)
			}
		case wire.StreamTypeComputerMaterialize:
			var r computerv0.MaterializeComputerRequest
			if err := frameio.ReadProtoFrame(stream, &r); err != nil {
				t.Error(err)
				return
			}
			if r.Envelope.ComputerInstanceId != identity.InstanceID || r.Envelope.ChannelCredential != "owned" || r.Target.BaseComputerDiskVersionId != base {
				t.Error("unbound materialization")
			}
			if err := frameio.WriteProtoFrame(stream, &computerv0.MaterializeComputerResponse{Status: "running", Target: r.Target, GuestChannelCredentialHash: sha256sum.HexBytes([]byte("owned"))}); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected guest stream: %s", header.Type)
		}
	}}}
	backend := &allocationOwnerTestBackend{t: t, machine: machine}
	ledger, err := reservation.New(reservation.Vector{CPUMillis: 1000, MemoryBytes: 1 << 30, HostDiskBytes: 32 << 30, VMSlots: 1})
	if err != nil {
		t.Fatal(err)
	}
	ranges, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := NewCheckpointEncryptor(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	machines := &PreparedMachines{CheckpointCipher: cipher, ComputerObjects: preparationOwnerStore{ranges}, ComputerRanges: ranges, ComputerHelper: "/test/nbd-helper", ComputerDevices: []string{"/dev/nbd-test"}, Backend: backend, Reservations: ledger, ComputerStagingBytes: 1 << 20, ComputerSaveEvery: time.Minute, TempDir: t.TempDir()}
	owner, err := NewComputerAllocationOwner(machines, client, identity, uuid.NewV7().String(), 7, func(context.Context) error { return nil }, func() {}, func(context.Context, *agentv1.GuestSessionMessage) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	owner.attachDevice = func(context.Context, workerapi.AllocationIdentity, workerapi.ComputerAllocationSource) (vm.ComputerDevice, error) {
		device := &failingCloseComputerDevice{}
		machines.computerDevices = map[preparedMachineRef]vm.ComputerDevice{{id: identity.InstanceID, epoch: identity.Epoch}: device}
		return device, nil
	}
	return owner, client, backend
}

func TestComputerAllocationOwnsRenewalAndRetriesCleanupWithoutReboot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o, client, backend := allocationOwnerFixture(t)
		backend.machine.failClose.Store(true)
		backend.failCleanup.Store(true)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- o.Run(ctx) }()
		time.Sleep(35 * time.Second)
		synctest.Wait()
		if o.Finished() || client.renewals.Load() < 3 || client.readiness.Load() != 2 {
			t.Fatal("owner did not retain live allocation and retry readiness")
		}
		cancel()
		if err := <-done; err == nil {
			t.Fatal("unproved cleanup hidden")
		}
		if client.stops.Load() != 0 || len(o.machines.Reservations.Snapshot().Reservations) != 1 {
			t.Fatal("cleanup failure released custody")
		}
		// Startup health may fail after boot. Cleanup must not re-enter admission.
		o.admitStart = func(context.Context) error {
			t.Fatal("cleanup invoked startup admission")
			return errors.New("unhealthy")
		}
		// The machine keeps its cached Close failure; backend cleanup must reconcile it.
		backend.failCleanup.Store(false)
		arena := o.machines.computerPreparationDirectory(o.identity.InstanceID, o.identity.Epoch)
		if err := os.MkdirAll(arena, 0700); err != nil {
			t.Fatal(err)
		}
		cut := filepath.Join(arena, "retained-cut")
		if err := os.WriteFile(cut, []byte("unresolved immutable cut"), 0600); err != nil {
			t.Fatal(err)
		}
		client.loseStop.Store(true)
		if err := o.Run(t.Context()); err == nil || o.Finished() {
			t.Fatal("lost stop receipt reported finished")
		}
		if _, err := os.Stat(cut); err != nil || len(o.machines.Reservations.Snapshot().Reservations) != 1 {
			t.Fatalf("unacknowledged stop released funded custody: %v", err)
		}
		if err := o.Run(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(arena); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("confirmed cleanup retained arena: %v", err)
		}
		if !o.Finished() || backend.starts.Load() != 1 || client.deliveries.Load() != 1 || client.stops.Load() != 2 || len(o.machines.Reservations.Snapshot().Reservations) != 0 {
			t.Fatal("cleanup retry rebooted or failed to settle exact allocation")
		}
	})
}

func TestComputerAllocationStopsAtLeaseExpiryAfterTransientOutage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o, client, backend := allocationOwnerFixture(t)
		client.failRenew.Store(true)
		done := make(chan error, 1)
		go func() { done <- o.Run(t.Context()) }()
		time.Sleep(61 * time.Second)
		synctest.Wait()
		if err := <-done; err == nil {
			t.Fatal("lease expiry returned success")
		}
		if !o.Finished() || backend.starts.Load() != 1 || backend.machine.closes.Load() != 1 || client.stops.Load() != 1 {
			t.Fatal("expired authority retained live allocation")
		}
	})
}

func TestComputerAllocationRenewsShortDeliveryAndRecoversBeforeExpiry(t *testing.T) {
	for _, outage := range []bool{false, true} {
		t.Run(fmt.Sprint(outage), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				o, client, backend := allocationOwnerFixture(t)
				client.delivery.ExpiresAt = time.Now().Add(5 * time.Second)
				client.failRenew.Store(outage)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- o.Run(ctx) }()
				time.Sleep(4 * time.Second)
				client.failRenew.Store(false)
				time.Sleep(3 * time.Second)
				synctest.Wait()
				if client.renewals.Load() == 0 || backend.machine.closes.Load() != 0 || o.Finished() {
					t.Fatal("short allocation expired without renewing after recovery")
				}
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestComputerAllocationRejectsMissingDeviceConfigurationBeforeDelivery(t *testing.T) {
	for _, missing := range []string{"helper", "devices", "source", "save-interval"} {
		t.Run(missing, func(t *testing.T) {
			o, client, _ := allocationOwnerFixture(t)
			switch missing {
			case "save-interval":
				o.machines.ComputerSaveEvery = 0
			case "helper":
				o.machines.ComputerHelper = "relative-helper"
			case "devices":
				o.machines.ComputerDevices = nil
			case "source":
				o.machines.ComputerRanges = nil
			}
			if _, err := NewComputerAllocationOwner(o.machines, client, o.identity, o.hostID, o.hostEpoch, o.admitStart, o.checkpointKeyUnavailable, o.observe); err == nil || client.deliveries.Load() != 0 || len(o.machines.Reservations.Snapshot().Reservations) != 0 {
				t.Fatal("invalid device configuration accepted allocation custody")
			}
		})
	}
}

func (c *allocationOwnerTestClient) ClaimComputerCommand(ctx context.Context, request workerapi.ComputerCommandClaimRequest) (workerapi.ComputerCommandClaimResponse, error) {
	if c.claimCommand != nil {
		return c.claimCommand(ctx, request)
	}
	return workerapi.ComputerCommandClaimResponse{}, nil
}

func TestComputerAllocationJoinsCommandRequestsBeforePhysicalCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		owner, client, backend := allocationOwnerFixture(t)
		var entered, joined atomic.Bool
		client.claimCommand = func(ctx context.Context, request workerapi.ComputerCommandClaimRequest) (workerapi.ComputerCommandClaimResponse, error) {
			if request.EnvironmentID != owner.identity.EnvironmentID || request.ComputerInstanceID != owner.identity.InstanceID || request.WriterGeneration != owner.identity.Epoch {
				t.Error("Command claim escaped its allocation")
			}
			entered.Store(true)
			<-ctx.Done()
			if backend.machine.closes.Load() != 0 {
				t.Error("physical cleanup preceded Command cancellation")
			}
			joined.Store(true)
			return workerapi.ComputerCommandClaimResponse{}, ctx.Err()
		}
		client.beforeStop = func() {
			if !joined.Load() {
				t.Error("stop observation preceded Command join")
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- owner.Run(ctx) }()
		time.Sleep(time.Second)
		synctest.Wait()
		if !entered.Load() {
			t.Fatal("Command service never started on the ready allocation")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if !owner.Finished() || !joined.Load() {
			t.Fatal("allocation did not join Command service and finish cleanup")
		}
	})
}

func TestComputerAllocationAdmissionPrecedesPhysicalEffects(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o, client, backend := allocationOwnerFixture(t)
		denied := errors.New("host health rejects startup")
		o.admitStart = func(context.Context) error { return denied }
		o.attachDevice = func(context.Context, workerapi.AllocationIdentity, workerapi.ComputerAllocationSource) (vm.ComputerDevice, error) {
			t.Fatal("device attached before admission")
			return nil, nil
		}
		ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
		defer cancel()
		if err := o.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("startup error = %v", err)
		}
		if backend.starts.Load() != 0 || client.stops.Load() != 0 || o.closing || len(o.machines.Reservations.Snapshot().Reservations) != 0 {
			t.Fatal("denied startup acquired physical custody")
		}
		if _, err := NewComputerAllocationOwner(o.machines, client, o.identity, o.hostID, o.hostEpoch, nil, o.checkpointKeyUnavailable, o.observe); err == nil {
			t.Fatal("missing admission accepted")
		}
	})
}

func TestComputerAllocationClosedDeliveryCleansUpWithoutStartupAdmission(t *testing.T) {
	o, client, backend := allocationOwnerFixture(t)
	client.deliveryError = &httpclient.Error{StatusCode: http.StatusConflict, Code: workerapi.AllocationClosed}
	o.admitStart = func(context.Context) error { t.Fatal("closed allocation attempted startup"); return nil }
	if err := o.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !o.Finished() || backend.starts.Load() != 0 || client.stops.Load() != 1 {
		t.Fatal("closed allocation did not settle physical absence")
	}
}

func TestComputerAllocationClosedReadinessStopsInsteadOfRenewingForever(t *testing.T) {
	o, client, backend := allocationOwnerFixture(t)
	client.readinessError = &httpclient.Error{StatusCode: http.StatusConflict, Code: workerapi.AllocationClosed}
	if err := o.Run(t.Context()); err == nil {
		t.Fatal("terminal readiness rejection hidden")
	}
	if !o.Finished() || backend.starts.Load() != 1 || client.readiness.Load() != 1 || client.stops.Load() != 1 {
		t.Fatal("terminal readiness did not settle physical custody")
	}
}

func TestComputerAllocationHealthWaitRenewsSameGrant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o, client, backend := allocationOwnerFixture(t)
		readyAt := time.Now().Add(75 * time.Second)
		o.admitStart = func(context.Context) error {
			if time.Now().Before(readyAt) {
				return errors.New("temporarily unhealthy")
			}
			return nil
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- o.Run(ctx) }()
		time.Sleep(70 * time.Second)
		synctest.Wait()
		if backend.starts.Load() != 0 || client.renewals.Load() < 3 || client.deliveries.Load() != 1 {
			t.Fatal("health wait lost grant or started early")
		}
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if backend.starts.Load() != 1 || client.deliveries.Load() != 1 || client.readiness.Load() != 2 {
			t.Fatal("same grant did not start after health recovered")
		}
		cancel()
		<-done
	})
}
func TestComputerAllocationCapacityWaitRenewsWithQuarantine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o, client, backend := allocationOwnerFixture(t)
		key := reservation.Key{Kind: "quarantine", ID: "residue", Epoch: 1}
		if _, err := o.machines.Reservations.Reserve(key, reservation.Vector{CPUMillis: 1, MemoryBytes: 1, VMSlots: 1}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- o.Run(ctx) }()
		time.Sleep(75 * time.Second)
		synctest.Wait()
		if backend.starts.Load() != 0 || client.renewals.Load() < 3 || client.deliveries.Load() != 1 {
			t.Fatal("capacity wait lost grant or bypassed quarantine")
		}
		if err := o.machines.Reservations.Release(key); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if backend.starts.Load() != 1 || client.deliveries.Load() != 1 {
			t.Fatal("capacity recovery did not start same grant")
		}
		cancel()
		<-done
	})
}

func TestComputerAllocationRetainsCapacityUntilDeviceCleanupSucceeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		owner, client, backend := allocationOwnerFixture(t)
		ref := preparedMachineRef{id: owner.identity.InstanceID, epoch: owner.identity.Epoch}
		deviceFailure := errors.New("device cleanup failed")
		device := &countingCloseComputerDevice{err: deviceFailure}
		owner.attachDevice = func(context.Context, workerapi.AllocationIdentity, workerapi.ComputerAllocationSource) (vm.ComputerDevice, error) {
			owner.machines.computerDevices = map[preparedMachineRef]vm.ComputerDevice{ref: device}
			return device, nil
		}
		client.beforeStop = func() {
			if len(owner.machines.Reservations.Snapshot().Reservations) != 1 || !computerDeviceTracked(owner.machines, ref) || device.closeCount() < 2 {
				t.Error("stop did not retain capacity after device closure")
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- owner.Run(ctx) }()
		time.Sleep(time.Second)
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, deviceFailure) {
			t.Fatalf("cleanup error = %v", err)
		}
		if owner.Finished() || client.stops.Load() != 0 || len(owner.machines.Reservations.Snapshot().Reservations) != 1 || !computerDeviceTracked(owner.machines, ref) {
			t.Fatal("failed device cleanup released allocation custody")
		}
		device.mu.Lock()
		device.err = nil
		device.mu.Unlock()
		if err := owner.Run(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !owner.Finished() || backend.starts.Load() != 1 || client.deliveries.Load() != 1 || client.stops.Load() != 1 || device.closeCount() < 2 {
			t.Fatal("device retry did not settle the same allocation")
		}
	})
}

func TestComputerAllocationStopsOnProcessDiscoveryClosure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		owner, client, backend := allocationOwnerFixture(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- owner.Run(ctx) }()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if client.readiness.Load() != 2 || owner.Finished() {
			t.Fatal("allocation did not reach healthy service")
		}
		client.closeProcesses.Store(true)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case err := <-done:
			var response *httpclient.Error
			if !errors.As(err, &response) || response.Code != workerapi.AllocationClosed {
				t.Fatalf("closure outcome: %v", err)
			}
		default:
			t.Fatal("closure was ignored by running allocation")
		}
		if !owner.Finished() || backend.starts.Load() != 1 || backend.machine.closes.Load() != 1 || client.stops.Load() != 1 || len(owner.machines.Reservations.Snapshot().Reservations) != 0 {
			t.Fatal("closure did not stop and acknowledge the exact physical allocation")
		}
	})
}

func (c *allocationOwnerTestClient) BeginAgentComputerCapture(context.Context, workerapi.AgentComputerCaptureRequest) (workerapi.AgentComputerCaptureResponse, error) {
	return workerapi.AgentComputerCaptureResponse{}, &httpclient.Error{StatusCode: http.StatusConflict, Code: workerapi.AgentComputerNotReady}
}

func TestComputerAllocationKeyFaultReleasesUnstartedOwners(t *testing.T) {
	for _, afterDelivery := range []bool{false, true} {
		t.Run(fmt.Sprint(afterDelivery), func(t *testing.T) {
			o, client, backend := allocationOwnerFixture(t)
			if !afterDelivery {
				client.deliveryError = &httpclient.Error{StatusCode: 409, Code: workerapi.AgentComputerNotReady}
			}
			o.admitStart = func(context.Context) error {
				if !afterDelivery || client.deliveries.Load() > 0 {
					return ErrCheckpointKeyUnavailable
				}
				return nil
			}
			if err := o.Run(t.Context()); !errors.Is(err, ErrCheckpointKeyUnavailable) {
				t.Fatalf("fault: %v", err)
			}
			if !o.Finished() || client.stops.Load() != 1 || backend.starts.Load() != 0 || len(o.machines.Reservations.Snapshot().Reservations) != 0 {
				t.Fatal("unstarted assignment remained held or started")
			}
		})
	}
}

func TestComputerAllocationStopsAfterBackgroundCaptureFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		owner, client, backend := allocationOwnerFixture(t)
		owner.machines.ComputerSaveEvery = time.Second
		failure := errors.New("uncertain optional capture")
		backend.machine.captureError = failure
		requests := 0
		client.nextSave = func(_ context.Context, r workerapi.AgentSaveDiscovery) (workerapi.AgentSavePending, error) {
			if !r.BackgroundDue {
				return workerapi.AgentSavePending{}, nil
			}
			requests++
			return workerapi.AgentSavePending{Save: &workerapi.AgentSave{EnvironmentID: owner.identity.EnvironmentID, SaveID: uuid.NewV7().String(), LeaseEpoch: owner.identity.Epoch}, Sequence: 1}, nil
		}
		client.beforeStop = func() {
			if backend.machine.closes.Load() == 0 || backend.cleanups.Load() == 0 {
				t.Error("fenced before physical cleanup")
			}
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if err := owner.Run(ctx); !errors.Is(err, failure) {
			t.Fatalf("capture failure: %v", err)
		}
		if requests != 1 || backend.starts.Load() != 1 || backend.machine.closes.Load() != 1 || client.stops.Load() != 1 || !owner.Finished() {
			t.Fatalf("optional failure did not close exact source: requests=%d starts=%d closes=%d stops=%d finished=%v", requests, backend.starts.Load(), backend.machine.closes.Load(), client.stops.Load(), owner.Finished())
		}
	})
}
