//go:build linux

package executor

import (
	"bytes"
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// File-backed publication is a test transport, not a proof of object-store immutability.
type computerPreparationTransport struct {
	cas.Store
	target  workerapi.RuntimeReconcileTarget
	events  []string
	request workerapi.ComputerInitializationRequest
	fail    string
}

func (s *computerPreparationTransport) Publish(ctx context.Context, expected cas.Descriptor, file *os.File) (cas.Object, error) {
	s.events = append(s.events, "upload")
	if s.fail == "upload" {
		return cas.Object{}, errors.New("upload unavailable")
	}
	object, err := s.Store.Put(ctx, expected.MediaType, file)
	if err == nil && (object.Digest != expected.Digest || object.SizeBytes != expected.SizeBytes) {
		return object, errors.New("candidate changed")
	}
	return object, err
}
func (s *computerPreparationTransport) RegisterComputerInitialization(_ context.Context, r workerapi.ComputerInitializationRequest) (workerapi.ComputerInitializationResponse, error) {
	s.events = append(s.events, "register")
	s.request = r
	if s.fail == "register" {
		return workerapi.ComputerInitializationResponse{}, errors.New("registration unavailable")
	}
	return s.receipt("registered"), nil
}
func (s *computerPreparationTransport) PublishComputerInitialization(_ context.Context, r workerapi.ComputerInitializationRequest) (workerapi.ComputerInitializationResponse, error) {
	s.events = append(s.events, "commit")
	if !reflect.DeepEqual(r, s.request) {
		return workerapi.ComputerInitializationResponse{}, errors.New("publication request changed")
	}
	if s.fail == "commit" {
		return workerapi.ComputerInitializationResponse{}, errors.New("publication unavailable")
	}
	return s.receipt("consumed"), nil
}
func (s *computerPreparationTransport) receipt(status string) workerapi.ComputerInitializationResponse {
	r := workerapi.ComputerInitializationResponse{ID: "01950000-0000-7000-8000-000000000004", ComputerID: s.target.Source.WorkspaceID, VersionID: s.target.Source.Computer.VersionID, Status: status}
	if status == "consumed" {
		r.ArtifactID = "01950000-0000-7000-8000-000000000005"
	}
	return r
}

func TestComputerPreparationPublicationAndRestore(t *testing.T) {
	root := t.TempDir()
	objects, err := cas.NewFile(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := checkpoint.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "seed.raw")
	file, err := os.OpenFile(source, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(computer.SeedCapacity); err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteAt([]byte("customer-state"), 8192); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	packed := filepath.Join(root, "seed.filepack")
	seed, err := computer.EncodeSeed(t.Context(), source, packed)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.Open(packed)
	if err != nil {
		t.Fatal(err)
	}
	object, err := objects.Put(t.Context(), seed.Object.MediaType, body)
	body.Close()
	if err != nil {
		t.Fatal(err)
	}
	target := workerapi.RuntimeReconcileTarget{ID: "01950000-0000-7000-8000-000000000001", WorkerEpoch: 1, DesiredVersion: 1, Source: workerapi.RuntimeSource{WorkspaceID: "01950000-0000-7000-8000-000000000002", ReservedDiskMiB: computer.SeedCapacity / mebibyte, Computer: &workerapi.RuntimeComputerSource{VersionID: "01950000-0000-7000-8000-000000000003", LogicalBytes: computer.SeedCapacity, Seed: &workerapi.ComputerSeed{Profile: computer.SeedProfile, Object: workerapi.CASObject{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}}}}}
	limit, err := (computer.DiskStore{CAS: objects, Cipher: cipher}).CaptureSizeLimit(computer.SeedCapacity)
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"capacity", "register", "upload", "commit", ""} {
		name := failure
		if name == "" {
			name = "success"
		}
		t.Run(name, func(t *testing.T) {
			diskCapacity := limit
			if failure == "capacity" {
				diskCapacity--
			}
			ledger, err := capacity.New(capacity.Vector{CPUMillis: 1, MemoryBytes: 1, GuestEphemeralDiskBytes: diskCapacity})
			if err != nil {
				t.Fatal(err)
			}
			transport := &computerPreparationTransport{Store: objects, target: target, fail: failure}
			pool := &PreparedRuntimePool{TempDir: t.TempDir(), CAS: objects, CheckpointEncryptor: cipher, Capacity: ledger, ComputerObjects: transport, ComputerInitializations: transport}
			disk, cleanup, err := pool.prepareComputerDisk(t.Context(), target)
			if failure != "" {
				if err == nil || disk != nil {
					t.Fatal("failed preparation exposed a disk")
				}
				if failure == "capacity" && (!errors.Is(err, errPreparedRuntimeCapacityBusy) || len(transport.events) != 0) {
					t.Fatalf("capacity did not defer before publication: %v", err)
				}
				if _, err := os.Stat(pool.computerPreparationDirectory(target.ID, target.WorkerEpoch)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed preparation leaked files: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(transport.events, []string{"register", "upload", "commit"}) {
					t.Fatalf("wrong publication order: %v", transport.events)
				}
				got := make([]byte, 14)
				if _, err := disk.ReadAt(got, 8192); err != nil || string(got) != "customer-state" {
					t.Fatalf("wrong prepared data: %q %v", got, err)
				}
				if err := cleanup(); err != nil {
					t.Fatal(err)
				}
				continuation := target
				c := *target.Source.Computer
				continuation.Source.Computer = &c
				c.Seed = nil
				c.Disk = &transport.request.Disk
				transport.events = nil
				restored, closeRestored, err := pool.prepareComputerDisk(t.Context(), continuation)
				if err != nil {
					t.Fatal(err)
				}
				if len(transport.events) != 0 {
					t.Fatal("continuation republished initialization")
				}
				if _, err := restored.ReadAt(got, 8192); err != nil || string(got) != "customer-state" {
					t.Fatalf("wrong restored data: %q %v", got, err)
				}
				if err := closeRestored(); err != nil {
					t.Fatal(err)
				}
			}

			if failure == "" {
				target.Source.ReservedCPUMillis = 1000
				target.Source.ReservedMemoryMiB = 512
				target.Source.DeploymentDefinitionID = "01950000-0000-7000-8000-000000000006"
				ledger, err := capacity.New(capacity.Vector{CPUMillis: 1000, MemoryBytes: 512 << 20, GuestEphemeralDiskBytes: 2*computer.SeedCapacity + limit, VMSlots: 1})
				if err != nil {
					t.Fatal(err)
				}
				pool.Capacity = ledger
				client := &typedRuntimeClient{}
				pool.RuntimeInstances = client
				transport.events = nil
				connector := &inspectingComputerConnector{inspect: func(r vm.MaterializeRequest) {
					if !reflect.DeepEqual(transport.events, []string{"register", "upload", "commit"}) {
						t.Fatalf("boot before publication: %v", transport.events)
					}
					if r.Topology.Computer == nil || r.Topology.Substrate != nil {
						t.Fatal("wrong materialization topology")
					}
					got := make([]byte, 14)
					if _, err := r.Topology.Computer.File.ReadAt(got, 8192); err != nil || string(got) != "customer-state" {
						t.Fatalf("wrong connector disk: %q %v", got, err)
					}
					snapshot := ledger.Snapshot()
					if len(snapshot.Reservations) != 1 || snapshot.Used.GuestEphemeralDiskBytes != 2*computer.SeedCapacity {
						t.Fatalf("wrong post-publication capacity: %+v", snapshot)
					}
				}}
				pool.Connector = connector
				mount := preparedRuntimeWorkspaceMountFromSource(target.Source)
				if err := pool.prepareAndStore(t.Context(), target.ID, mount, target, func() {}); err != nil {
					t.Fatal(err)
				}
				if connector.calls != 1 || len(client.failed) != 1 {
					t.Fatalf("calls=%d failures=%d", connector.calls, len(client.failed))
				}
				if len(ledger.Snapshot().Reservations) != 1 {
					t.Fatal("released uncertain materialization capacity")
				}
				if _, err := os.Stat(pool.computerPreparationDirectory(target.ID, target.WorkerEpoch)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("source files remain after transfer failure: %v", err)
				}
				if err := pool.ReclaimFailedRuntimeTarget(t.Context(), client, target); err != nil {
					t.Fatal(err)
				}
				if len(ledger.Snapshot().Reservations) != 0 {
					t.Fatal("proven cleanup retained capacity")
				}
			}
			if len(ledger.Snapshot().Reservations) != 0 {
				t.Fatalf("staging reservation leaked: %+v", ledger.Snapshot())
			}
		})
	}
}

// The host call path must publish before handing a writable file to a connector.
type inspectingComputerConnector struct {
	inspect func(vm.MaterializeRequest)
	calls   int
}

func (c *inspectingComputerConnector) Cleanup(context.Context, vm.Owner) error { return nil }
func (c *inspectingComputerConnector) Materialize(_ context.Context, r vm.MaterializeRequest) (vm.Session, error) {
	c.calls++
	c.inspect(r)
	return nil, errors.New("injected materialization failure")
}

func TestReconcileDesiredRuntimesRunsBatchConcurrentlyAndWaitsForShutdown(t *testing.T) {
	store, mount := testWorkspaceMountArtifacts(t)
	connector := &blockingMaterializingConnector{
		started: make(chan string, 2), canceled: make(chan string, 2), failID: "runtime-0",
	}
	var logs bytes.Buffer
	pool := NewPreparedRuntimePool(connector, store, 2, slog.New(slog.NewTextHandler(&logs, nil)))
	pool.TempDir = t.TempDir()
	pool.RuntimeArchitecture = deployment.RuntimeArchitecture("x86_64")
	pool.Capacity = newPreparedRuntimeCapacity(t, 2)
	items := make([]workerapi.RuntimeReconcileTarget, 2)
	for i := range items {
		items[i] = runtimePreparationTarget(mount, uuid.NewV7().String(), 7)
	}
	configureComputerPreparationTest(t, pool, items)
	connector.failID = items[0].ID
	client := &batchRuntimeClient{response: workerapi.RuntimeReconcileResponse{Items: items}}
	pool.RuntimeInstances = client
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pool.ReconcileDesiredRuntimes(ctx, client) }()
	for range items {
		select {
		case <-connector.started:
		case <-time.After(time.Second):
			t.Fatal("batch target did not reach materialization")
		}
	}
	select {
	case id := <-connector.canceled:
		t.Fatalf("sibling %q was canceled after an ordinary target failure", id)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reconciler returned before its attempts drained")
	}
	if !strings.Contains(logs.String(), `msg="prepared runtime phase"`) ||
		!strings.Contains(logs.String(), "phase=test_materialize") {
		t.Fatalf("fresh materialize phase was not logged: %s", logs.String())
	}
}

func TestWarmRuntimePreparationDeadlineCancelsBlockedMaterialization(t *testing.T) {
	store, mount := testWorkspaceMountArtifacts(t)
	connector := &blockingMaterializingConnector{started: make(chan string, 1), canceled: make(chan string, 1)}
	pool := NewPreparedRuntimePool(connector, store, 1, nil)
	pool.TempDir = t.TempDir()
	pool.RuntimeArchitecture = deployment.RuntimeArchitecture("x86_64")
	pool.Capacity = newPreparedRuntimeCapacity(t, 1)
	client := &typedRuntimeClient{}
	pool.RuntimeInstances = client
	target := runtimePreparationTarget(mount, uuid.NewV7().String(), 7)
	items := []workerapi.RuntimeReconcileTarget{target}
	configureComputerPreparationTest(t, pool, items)
	target = items[0]
	target.PreparationExpiresAt = time.Now().Add(2 * time.Second)
	done := make(chan error, 1)
	go func() { done <- pool.warmRuntimeTarget(t.Context(), client, target, func() {}) }()
	select {
	case <-connector.started:
	case <-time.After(3 * time.Second):
		t.Fatal("materialization did not start")
	}
	select {
	case <-connector.canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("deadline did not cancel preparation")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("preparation did not finish")
	}
	if len(client.failed) != 1 {
		t.Fatalf("failure reports=%d", len(client.failed))
	}
}

// Reconciliation tests exercise the real Linux encrypted disk path up to a fake VM connector.
func configureComputerPreparationTest(t *testing.T, pool *PreparedRuntimePool, targets []workerapi.RuntimeReconcileTarget) {
	t.Helper()
	dir := t.TempDir()
	objects, err := cas.NewFile(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := checkpoint.New(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(dir, "raw")
	f, err := os.Create(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(computer.SeedCapacity); err != nil {
		t.Fatal(err)
	}
	f.Close()
	candidate, err := (computer.DiskStore{CAS: objects, Cipher: cipher}).Capture(t.Context(), targets[0].Source.WorkspaceID, raw, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	if err := candidate.Upload(t.Context(), &computerPreparationTransport{Store: objects}); err != nil {
		t.Fatal(err)
	}
	artifact := candidate.Artifact()
	for i := range targets {
		targets[i].Source.Computer.Disk = &workerapi.CASObject{Digest: artifact.Object.Digest, SizeBytes: artifact.Object.SizeBytes, MediaType: artifact.Object.MediaType}
	}
	pool.CAS = objects
	pool.CheckpointEncryptor = cipher
	n := int64(len(targets))
	pool.Capacity, err = capacity.New(capacity.Vector{CPUMillis: 1000 * n, MemoryBytes: n * 512 << 20, GuestEphemeralDiskBytes: n * 2 * computer.SeedCapacity, VMSlots: n})
	if err != nil {
		t.Fatal(err)
	}
}
