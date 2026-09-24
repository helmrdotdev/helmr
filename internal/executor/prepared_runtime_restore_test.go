package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"testing"

	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/deployment"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type restoreTestConnector struct {
	restore func(vm.RestoreRequest) (vm.Session, error)
}

func (c restoreTestConnector) Cleanup(context.Context, vm.Owner) error { return nil }
func (c restoreTestConnector) Restore(_ context.Context, r vm.RestoreRequest) (vm.Session, error) {
	return c.restore(r)
}

type restoreTestStore struct {
	*checkpointCAS
	get   func(string) (io.ReadCloser, error)
	reads atomic.Int32
}

func (s *restoreTestStore) Get(_ context.Context, d string) (io.ReadCloser, error) {
	s.reads.Add(1)
	return s.get(d)
}

func restoreTestFixture(t *testing.T) (*PreparedRuntimePool, workerapi.RuntimeReconcileTarget, vm.RuntimeTopology, *restoreTestStore) {
	t.Helper()
	encryptor := testCheckpointEncryptor(t)
	store := &restoreTestStore{checkpointCAS: &checkpointCAS{}}
	store.get = func(d string) (io.ReadCloser, error) { return store.checkpointCAS.Get(context.Background(), d) }
	put := func(suffix, media string) workerapi.CheckpointArtifact {
		var encrypted bytes.Buffer
		if err := encryptor.Encrypt(context.Background(), bytes.NewBufferString(suffix), &encrypted, checkpointPurpose(suffix)); err != nil {
			t.Fatal(err)
		}
		obj, err := store.Put(context.Background(), media, &encrypted)
		if err != nil {
			t.Fatal(err)
		}
		return workerapi.CheckpointArtifact{Digest: obj.Digest, SizeBytes: obj.SizeBytes, MediaType: obj.MediaType}
	}
	cp := workerapi.CheckpointManifest{
		RecoveryPoint: workerapi.CheckpointRecoveryPoint{ID: "checkpoint", RunID: "run", AttemptNumber: 2, RunWaitID: "wait", CorrelationID: "correlation",
			Runtime: workerapi.CheckpointRuntime{Backend: "firecracker", ID: "runtime-shape", Arch: string(deployment.ArchitectureX8664), Contract: "abi", KernelDigest: "kernel", InitramfsDigest: "initramfs", RootfsDigest: "rootfs", ConfigDigest: "config", VMVCPUCount: 1, CPUConfigDigest: sha256sum.DigestBytes([]byte("cpu"))}},
		RuntimeState: workerapi.CheckpointRuntimeState{
			Computer:       &workerapi.CheckpointComputer{ComputerID: "01950000-0000-7000-8000-000000000001", LogicalBytes: computer.SeedCapacity, Root: testGenerationRoot(computer.SeedCapacity)},
			ConfigArtifact: put("manifest", cas.CheckpointRuntimeConfigMediaType), VMStateArtifact: put("vmstate", cas.CheckpointVMStateMediaType), MemoryArtifacts: []workerapi.CheckpointArtifact{put("memory", cas.CheckpointMemoryMediaType)}, ScratchDiskArtifact: put("scratch-disk", cas.CheckpointScratchDiskMediaType),
		},
	}
	manifest, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	target := workerapi.RuntimeReconcileTarget{ID: "01950000-0000-7000-8000-000000000003", WorkerEpoch: 1, Source: workerapi.RuntimeSource{
		WorkspaceID: cp.RuntimeState.Computer.ComputerID, VMVCPUCount: 1, CPUConfigDigest: cp.RecoveryPoint.Runtime.CPUConfigDigest,
		ReservedCPUMillis: 1000, ReservedMemoryMiB: 16, ReservedDiskMiB: computer.SeedCapacity / mebibyte, ReservedExecutionSlots: 1,
		Computer: &workerapi.RuntimeComputerSource{VersionID: "01950000-0000-7000-8000-000000000002", LogicalBytes: computer.SeedCapacity, Root: ptrGenerationRoot(computer.SeedCapacity)},
		Restore:  &workerapi.RuntimeRestore{CheckpointID: "checkpoint", RunID: "run", AttemptNumber: 2, RunWaitID: "wait", Manifest: manifest},
	}}
	for _, member := range []struct {
		role     string
		artifact workerapi.CheckpointArtifact
	}{{"runtime_config", cp.RuntimeState.ConfigArtifact}, {"vm_state", cp.RuntimeState.VMStateArtifact}, {"memory", cp.RuntimeState.MemoryArtifacts[0]}, {"scratch_disk", cp.RuntimeState.ScratchDiskArtifact}} {
		target.Source.Restore.Artifacts = append(target.Source.Restore.Artifacts, workerapi.RunLeaseCheckpointArtifact{Role: member.role, Object: workerapi.CASObject(member.artifact)})
	}
	ledger, err := capacity.New(capacity.Vector{CPUMillis: 1000, VMSlots: 1, MemoryBytes: 16 * mebibyte, GuestEphemeralDiskBytes: 1 << 40})
	if err != nil {
		t.Fatal(err)
	}
	pool := &PreparedRuntimePool{CAS: store, CheckpointEncryptor: encryptor, Capacity: ledger, TempDir: t.TempDir(), RuntimeArchitecture: deployment.ArchitectureX8664}
	topology := vm.RuntimeTopology{Computer: &vm.RuntimeComputer{ComputerID: target.Source.WorkspaceID, VersionID: target.Source.Computer.VersionID, SizeBytes: computer.SeedCapacity}}
	return pool, target, topology, store
}

func TestPreparedRestoreReservesBeforeDownloadAndRetainsRuntimeCharge(t *testing.T) {
	p, target, topology, store := restoreTestFixture(t)
	retained, staging, err := p.checkpointRestoreCapacity(target)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	session := &checkpointSession{stream: newCheckpointStream(t, nil, "", "", &workspacev0.VerifyProgramRestoreResponse{RunId: "run", AttemptNumber: 2, RunWaitId: "wait", CheckpointId: "checkpoint", CorrelationId: "correlation"})}
	p.Connector = restoreTestConnector{restore: func(r vm.RestoreRequest) (vm.Session, error) {
		calls++
		if r.Topology.Computer != topology.Computer || r.Resources.MemoryMiB != 16 || r.Resources.DiskMiB != target.Source.ReservedDiskMiB {
			t.Fatalf("restore authority changed: %+v", r)
		}
		for path, want := range map[string]string{r.VMState: "vmstate", r.Memory[0]: "memory", r.ScratchDisk: "scratch-disk"} {
			data, err := os.ReadFile(path)
			if err != nil || string(data) != want {
				t.Fatalf("restore content %s: %q %v", path, data, err)
			}
		}
		if string(r.Manifest) != "manifest" {
			t.Fatalf("manifest=%q", r.Manifest)
		}
		if p.Capacity.Snapshot().Reservations[restoreStagingKey(target.ID, target.WorkerEpoch)].GuestEphemeralDiskBytes != staging {
			t.Fatal("staging released before restore")
		}
		return session, nil
	}}
	if _, err := p.restorePreparedRuntime(context.Background(), target, topology, nil, nil); err == nil || store.reads.Load() != 0 {
		t.Fatalf("unreserved restore: %v reads=%d", err, store.reads.Load())
	}
	if err := p.reserveRuntimeCapacity(target, topology); err != nil {
		t.Fatal(err)
	}
	got, err := p.restorePreparedRuntime(context.Background(), target, topology, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != session || calls != 1 || store.reads.Load() != 4 {
		t.Fatalf("restore calls=%d reads=%d", calls, store.reads.Load())
	}
	assertRemoved(t, p.restorePreparationDirectory(target.ID, target.WorkerEpoch))
	reservations := p.Capacity.Snapshot().Reservations
	if _, ok := reservations[restoreStagingKey(target.ID, target.WorkerEpoch)]; ok {
		t.Fatal("staging retained after unlink")
	}
	want := 2*computer.SeedCapacity + retained
	if charge := reservations[runtimeCapacityKey(target.ID, target.WorkerEpoch)].GuestEphemeralDiskBytes; charge != want {
		t.Fatalf("retained charge=%d want=%d", charge, want)
	}
	if err := p.closeSession(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if len(p.Capacity.Snapshot().Reservations) != 1 {
		t.Fatal("session close released physical runtime charge")
	}
	if err := p.releaseRuntimeAfterPhysicalCleanup(target.ID, target.WorkerEpoch); err != nil {
		t.Fatal(err)
	}
	if len(p.Capacity.Snapshot().Reservations) != 0 {
		t.Fatal("charge survived physical cleanup")
	}
}

func TestPreparedRestoreRejectsTamperedObjectsAndCleansStaging(t *testing.T) {
	for _, kind := range []string{"digest", "truncated", "trailing"} {
		t.Run(kind, func(t *testing.T) {
			p, target, topology, store := restoreTestFixture(t)
			p.Connector = restoreTestConnector{restore: func(vm.RestoreRequest) (vm.Session, error) {
				t.Error("restored corrupt snapshot")
				return nil, errors.New("unexpected restore")
			}}
			object := store.puts[0]
			payload := append([]byte(nil), object.content...)
			switch kind {
			case "digest":
				var encrypted bytes.Buffer
				if err := p.CheckpointEncryptor.Encrypt(context.Background(), bytes.NewBufferString("manifest"), &encrypted, checkpointPurpose("manifest")); err != nil {
					t.Fatal(err)
				}
				payload = encrypted.Bytes() // Valid ciphertext with the same size but a different nonce/digest.
			case "truncated":
				payload = payload[:len(payload)-1]
			case "trailing":
				payload = append(payload, 0)
			}
			store.get = func(d string) (io.ReadCloser, error) {
				if d == object.object.Digest {
					return io.NopCloser(bytes.NewReader(payload)), nil
				}
				return store.checkpointCAS.Get(context.Background(), d)
			}
			if err := p.reserveRuntimeCapacity(target, topology); err != nil {
				t.Fatal(err)
			}
			if _, err := p.restorePreparedRuntime(context.Background(), target, topology, nil, nil); err == nil {
				t.Fatal("accepted altered ciphertext")
			}
			assertRemoved(t, p.restorePreparationDirectory(target.ID, target.WorkerEpoch))
			if _, ok := p.Capacity.Snapshot().Reservations[restoreStagingKey(target.ID, target.WorkerEpoch)]; ok {
				t.Fatal("staging not reclaimed")
			}
			if len(p.Capacity.Snapshot().Reservations) != 1 {
				t.Fatal("runtime charge released without physical cleanup")
			}
		})
	}
}

func TestPreparedRestoreCapacityShortfallDoesNotStartIO(t *testing.T) {
	p, target, topology, store := restoreTestFixture(t)
	retained, _, err := p.checkpointRestoreCapacity(target)
	if err != nil {
		t.Fatal(err)
	}
	p.Capacity, err = capacity.New(capacity.Vector{CPUMillis: 1000, VMSlots: 1, MemoryBytes: 16 * mebibyte, GuestEphemeralDiskBytes: 2*computer.SeedCapacity + retained})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.reserveRuntimeCapacity(target, topology); !errors.Is(err, errPreparedRuntimeCapacityBusy) {
		t.Fatalf("admission=%v", err)
	}
	if store.reads.Load() != 0 || len(p.Capacity.Snapshot().Reservations) != 0 {
		t.Fatal("failed admission started IO or leaked its partial reservation")
	}
}
