package computerhost

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/vm"
	"testing"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestPhysicalCloseReportsCleanupOnlyAfterSuccessfulClose(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "closed", true: "close failed"}[fail], func(t *testing.T) {
			raw := &serverTestSession{}
			if fail {
				raw.closeErr = errors.New("physical exclusion not established")
			}
			session := newInstanceMount(raw)
			client := &serverTestClient{}
			err := (Server{}).stopControlledComputerMount(t.Context(), session, nil, workerapi.ComputerInstanceAssignment{ComputerInstanceID: "instance", RuntimeEpoch: 1, DesiredVersion: 2, ObservedVersion: 1}, client)
			if fail {
				if !errors.Is(err, raw.closeErr) || client.stops != 0 || len(client.failures) != 1 {
					t.Fatalf("err=%v stop=%d failure=%d", err, client.stops, len(client.failures))
				}
			} else if err != nil || client.stops != 1 || len(client.failures) != 0 {
				t.Fatalf("err=%v stop=%d failure=%d", err, client.stops, len(client.failures))
			}
			if raw.closeCount() != 1 {
				t.Fatalf("close count=%d", raw.closeCount())
			}
		})
	}
}

type failingCloseComputerDevice struct {
	vm.ComputerDevice
	err error
}

func (d *failingCloseComputerDevice) Close(context.Context) error { return d.err }

func TestPhysicalCloseRetainsResourcesWithoutProofWhenDeviceCleanupFails(t *testing.T) {
	_, mount := testComputerMountArtifacts(t)
	mount.ComputerID = "computer"
	mount.DesiredVersion = 2
	mount.ObservedVersion = 1
	raw := &serverTestSession{}
	machines := computerPreparedMachines(t, mount, raw)
	checkout, _, ok := machines.checkout(t.Context(), mount)
	if !ok {
		t.Fatal("checkout failed")
	}
	ref := preparedMachineRef{id: mount.ComputerInstanceID, epoch: mount.RuntimeEpoch}
	device := &failingCloseComputerDevice{err: errors.New("device cleanup failed")}
	machines.computerDevices = map[preparedMachineRef]vm.ComputerDevice{ref: device}
	client := &serverTestClient{}
	err := (Server{Machines: machines}).stopControlledComputerMount(t.Context(), raw, checkout, mount, client)
	if !errors.Is(err, device.err) || client.stops != 0 {
		t.Fatalf("err=%v stops=%d", err, client.stops)
	}
	if machines.runtimeCheckedOut(ref.id, ref.epoch) || len(machines.Reservations.Snapshot().Reservations) != 1 || machines.computerDevices[ref] == nil {
		t.Fatal("exited checkout must relinquish ownership while retaining uncleaned resources")
	}
	machines.Backend = &cleanupRuntimeBackend{}
	device.err = nil
	target := runtimeReservationTarget(ref.id, ref.epoch)
	control := &typedRuntimeClient{}
	if err := machines.stopRuntimeTarget(t.Context(), control, target); err != nil {
		t.Fatal(err)
	}
	if len(machines.Reservations.Snapshot().Reservations) != 0 || len(control.closed) != 1 || control.closed[0].CleanupProof == nil {
		t.Fatal("cleanup did not release resources and publish proof")
	}
}
