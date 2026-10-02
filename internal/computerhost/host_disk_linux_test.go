//go:build linux

package computerhost

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type lifecycleCheckpointMachine struct{ *checkpointMachine }

func (*lifecycleCheckpointMachine) SnapshotLimits() (vm.SnapshotLimits, error) {
	return vm.SnapshotLimits{ComputerBytes: disk.SeedCapacity, MemoryBytes: 2048 << 20, ScratchBytes: 32768 << 20, StateBytes: vm.SnapshotStateLimit, ConfigBytes: vm.SnapshotConfigLimit}, nil
}

// Exercise the actual admission owners for restored instances, Computer versions
// and simultaneous recaptures. No VMM or NBD device is needed for these owners.
func TestHostDiskFundsTwoRestoredLifecycleCallers(t *testing.T) {
	cipher := testCheckpointEncryptor(t)
	const staging = int64(65536 << 20)
	envelope, err := HostDiskPerSlot(2048, 32768, staging, cipher)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := reservation.New(reservation.Vector{CPUMillis: 4000, MemoryBytes: 4096 << 20, HostDiskBytes: 2 * envelope, VMSlots: 2})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{8}, 32)
	writer := blockformat.Writer{Source: objects, Sink: objects, Scope: "fixture", ActiveKey: preparationKey, Keys: map[string][]byte{preparationKey: key}, PackLimit: blockformat.MinPackLimit}
	locator, err := writer.Empty(t.Context(), disk.SeedCapacity, 64)
	if err != nil {
		t.Fatal(err)
	}
	root, err := disk.NewVersionRoot(locator, disk.SeedCapacity)
	if err != nil {
		t.Fatal(err)
	}
	transport := &computerPreparationTransport{Store: objects, root: root, key: key, targets: make(map[string]workerapi.InstanceReconcileTarget)}
	prepared := &PreparedMachines{TempDir: t.TempDir(), CAS: objects, ComputerRanges: objects, ComputerObjects: transport, ComputerPreparation: transport, ComputerStagingBytes: staging, CheckpointEncryptor: cipher, Reservations: ledger}
	shape, _ := (&lifecycleCheckpointMachine{}).SnapshotLimits()
	limits, err := checkpointStagingSize(shape, cipher)
	if err != nil {
		t.Fatal(err)
	}
	media := []string{cas.CheckpointVMConfigMediaType, cas.CheckpointVMStateMediaType, cas.CheckpointScratchDiskMediaType, cas.CheckpointMemoryMediaType}
	inputs := make([]workerapi.CheckpointArtifact, 4)
	for i, size := range []int64{limits.config, limits.state, limits.scratch, limits.memory} {
		bound, err := cipher.EncryptedSize(size)
		if err != nil {
			t.Fatal(err)
		}
		inputs[i] = workerapi.CheckpointArtifact{Digest: sha256sum.DigestBytes([]byte(media[i])), MediaType: media[i], SizeBytes: bound}
	}
	manifest, err := json.Marshal(workerapi.CheckpointManifest{RuntimeState: workerapi.CheckpointRuntimeState{ConfigArtifact: inputs[0], VMStateArtifact: inputs[1], ScratchDiskArtifact: inputs[2], MemoryArtifacts: inputs[3:]}})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	defer func() {
		if release != nil {
			close(release)
		}
	}()
	results := make(chan error, 2)
	for range 2 {
		target := checkpointCaptureTarget(1)
		target.ID = uuid.NewV7().String()
		target.Capture.CheckpointID = uuid.NewV7().String()
		target.Source.ReservedCPUMillis = 2000
		target.Source.ReservedMemoryMiB = 2048
		target.Source.ReservedDiskMiB = 32768
		target.Source.Computer = &workerapi.InstanceComputerSource{VersionID: uuid.NewV7().String(), LogicalBytes: disk.SeedCapacity, Root: &root}
		target.Source.Restore = &workerapi.InstanceRestore{Manifest: manifest}
		transport.targets[target.ID] = target
		if err := prepared.reserveInstanceCapacity(target, vm.Topology{Computer: &vm.ComputerDisk{SizeBytes: disk.SeedCapacity}}); err != nil {
			t.Fatal(err)
		}
		local, err := prepared.prepareComputerVersion(t.Context(), target)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := local.Close(); err != nil {
				t.Error(err)
			}
			if err := prepared.releaseInstanceCapacity(target.ID, target.WorkerEpoch); err != nil {
				t.Error(err)
			}
		})
		// Restore materialization has removed its transient files; raw RAM and VM
		// state remain charged to the instance when another capture begins.
		if err := ledger.Release(restoreStagingKey(target.ID, target.WorkerEpoch)); err != nil {
			t.Fatal(err)
		}
		// Program/runtime owners are outside these three lifecycle callers. Hold
		// their full documented bound rather than reducing the test's pressure.
		if _, err := ledger.Reserve(reservation.Key{Kind: "artifacts", ID: target.ID, Epoch: target.WorkerEpoch}, reservation.Vector{HostDiskBytes: artifact.MaxProgramPhysicalBytes + artifact.MaxRuntimePhysicalBytes}); err != nil {
			t.Fatal(err)
		}
		snapshot := checkpointArtifact(t)
		snapshot.Computer.Capture = &versionCaptureFixture{root: root}
		gate := release
		machine := &lifecycleCheckpointMachine{&checkpointMachine{stream: checkpointFreezeStream(t, target), artifact: snapshot, snapshotHook: func() { entered <- struct{}{}; <-gate }}}
		checkpointer := &computerCheckpointer{publication: testCheckpointPublication, machine: machine, objects: &checkpointCAS{}, reservations: ledger, encryptor: cipher, tempDir: t.TempDir()}
		go func() {
			_, err := checkpointer.CreateCheckpoint(t.Context(), computerCheckpointRequest{Target: target, Register: func(context.Context, workerapi.CheckpointManifest) error { return nil }})
			results <- err
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case err := <-results:
			t.Fatalf("capture failed before both slots entered: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("both captures did not enter")
		}
	}
	if got := ledger.Snapshot().Used.HostDiskBytes; got != 2*envelope {
		t.Fatalf("actual lifecycle reservations = %d, want %d", got, 2*envelope)
	}
	close(release)
	release = nil
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}
