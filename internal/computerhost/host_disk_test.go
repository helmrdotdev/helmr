package computerhost

import (
	"encoding/json"
	"math"
	"os"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestHostDiskFundsTwoRestoredRecaptures(t *testing.T) {
	cipher, err := NewCheckpointEncryptor(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	const memory = int64(2048 << 20)
	const scratch = int64(32768 << 20)
	const staging = int64(65536 << 20)
	envelope, err := HostDiskPerSlot(memory>>20, scratch>>20, staging, cipher)
	if err != nil {
		t.Fatal(err)
	}
	limits, err := checkpointStagingSize(vm.SnapshotLimits{ComputerBytes: disk.SeedCapacity, MemoryBytes: memory, ScratchBytes: scratch, StateBytes: vm.SnapshotStateLimit, ConfigBytes: vm.SnapshotConfigLimit}, cipher)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := cipher.EncryptedSize(vm.SnapshotStateLimit)
	ledger, err := reservation.New(reservation.Vector{CPUMillis: 8000, MemoryBytes: 2 * memory, HostDiskBytes: 2 * envelope, VMSlots: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		for kind, size := range map[string]int64{"instance": scratch + disk.SeedCapacity + memory + state, "computer": staging, "artifacts": artifact.MaxProgramPhysicalBytes + artifact.MaxRuntimePhysicalBytes, "capture": limits.total} {
			if _, err := ledger.Reserve(reservation.Key{Kind: kind, Epoch: 1, ID: id}, reservation.Vector{HostDiskBytes: size}); err != nil {
				t.Fatalf("%s/%s: %v", id, kind, err)
			}
		}
	}
	if ledger.Snapshot().Used.HostDiskBytes != 2*envelope {
		t.Fatal("envelope components differ")
	}
	for _, shape := range [][3]int64{{0, 1, 1}, {1, 0, 1}, {1, 1, 0}, {math.MaxInt64, 1, 1}, {1, 1, math.MaxInt64}} {
		if _, err := HostDiskPerSlot(shape[0], shape[1], shape[2], cipher); err == nil {
			t.Fatalf("invalid shape accepted: %v", shape)
		}
	}
}

func TestRestoreRejectsOversizedArtifactsBeforeReservation(t *testing.T) {
	cipher, _ := NewCheckpointEncryptor(make([]byte, 32))
	p := &PreparedMachines{CheckpointEncryptor: cipher}
	limits, err := checkpointStagingSize(vm.SnapshotLimits{ComputerBytes: disk.SeedCapacity, MemoryBytes: 2 << 30, ScratchBytes: 32 << 30, StateBytes: vm.SnapshotStateLimit, ConfigBytes: vm.SnapshotConfigLimit}, cipher)
	if err != nil {
		t.Fatal(err)
	}
	media := []string{cas.CheckpointVMConfigMediaType, cas.CheckpointVMStateMediaType, cas.CheckpointScratchDiskMediaType, cas.CheckpointMemoryMediaType}
	artifacts := make([]workerapi.CheckpointArtifact, 4)
	for i, n := range []int64{limits.config, limits.state, limits.scratch, limits.memory} {
		bound, _ := cipher.EncryptedSize(n)
		artifacts[i] = workerapi.CheckpointArtifact{Digest: sha256sum.DigestBytes([]byte(media[i])), MediaType: media[i], SizeBytes: bound}
	}
	check := func() error {
		checkpoint := workerapi.CheckpointManifest{RuntimeState: workerapi.CheckpointRuntimeState{ConfigArtifact: artifacts[0], VMStateArtifact: artifacts[1], ScratchDiskArtifact: artifacts[2], MemoryArtifacts: artifacts[3:]}}
		raw, _ := json.Marshal(checkpoint)
		_, _, err := p.checkpointRestoreCapacity(workerapi.InstanceReconcileTarget{Source: workerapi.InstanceSource{ReservedMemoryMiB: 2048, ReservedDiskMiB: 32768, Restore: &workerapi.InstanceRestore{Manifest: raw}}})
		return err
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
	for i := range artifacts {
		artifacts[i].SizeBytes++
		if check() == nil {
			t.Fatalf("oversized role %d accepted", i)
		}
		artifacts[i].SizeBytes--
	}
}

// The AWS module tests consume these same format fixtures, so codec or artifact
// limit changes cannot silently leave infrastructure admission behind.
func TestHostDiskFormatBounds(t *testing.T) {
	raw, err := os.ReadFile("testdata/host_disk_bounds.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		MemoryMiB  int64 `json:"memory_mib"`
		ScratchMiB int64 `json:"scratch_mib"`
		StagingMiB int64 `json:"staging_mib"`
		Bytes      int64 `json:"bytes"`
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	cipher, _ := NewCheckpointEncryptor(make([]byte, 32))
	for i, fixture := range fixtures {
		actual, err := HostDiskPerSlot(fixture.MemoryMiB, fixture.ScratchMiB, fixture.StagingMiB<<20, cipher)
		if err != nil {
			t.Fatal(err)
		}
		if actual != fixture.Bytes {
			t.Errorf("fixture %d: bytes=%d, recorded=%d", i, actual, fixture.Bytes)
		}
	}
}
