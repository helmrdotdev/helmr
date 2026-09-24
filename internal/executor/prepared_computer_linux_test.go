//go:build linux

package executor

import (
	"bytes"
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"
)

type computerPreparationTransport struct {
	cas.Store
	mu           sync.Mutex
	targets      map[string]workerapi.RuntimeReconcileTarget
	root         computer.GenerationRoot
	fail         string
	key          []byte
	publications int
}

const preparationKey = "01950000-0000-7000-8000-000000000004"

func (s *computerPreparationTransport) Publish(ctx context.Context, d cas.Descriptor, f *os.File) (cas.Object, error) {
	if s.fail == "upload" {
		return cas.Object{}, errors.New("upload failed")
	}
	return s.Store.Put(ctx, d.MediaType, f)
}
func (s *computerPreparationTransport) RegisterInitialComputerObject(context.Context, workerapi.InitialComputerObjectRequest) error {
	if s.fail == "register" {
		return errors.New("register failed")
	}
	return nil
}
func (s *computerPreparationTransport) CertifyInitialComputerObject(context.Context, workerapi.InitialComputerObjectRequest) error {
	return nil
}
func (s *computerPreparationTransport) InitialComputerKey(context.Context, workerapi.InitialComputerKeyRequest) (workerapi.ComputerKeyMaterial, error) {
	return workerapi.ComputerKeyMaterial{Scope: "fixture", ID: preparationKey, Key: bytes.Clone(s.key)}, nil
}
func (s *computerPreparationTransport) PublishInitialComputerGeneration(_ context.Context, r workerapi.InitialComputerGenerationRequest) (workerapi.InitialComputerGenerationResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail == "commit" {
		return workerapi.InitialComputerGenerationResponse{}, errors.New("commit failed")
	}
	s.root = r.Root
	s.publications++
	target := s.targets[r.RuntimeInstanceID]
	return workerapi.InitialComputerGenerationResponse{ComputerID: target.Source.WorkspaceID, VersionID: target.Source.Computer.VersionID}, nil
}
func (s *computerPreparationTransport) ComputerSource(_ context.Context, r workerapi.ComputerSourceRequest) (workerapi.ComputerSourceMaterial, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return workerapi.ComputerSourceMaterial{Root: s.root, VersionID: s.targets[r.RuntimeInstanceID].Source.Computer.VersionID, WriteKeyID: preparationKey, Keys: []workerapi.ComputerKeyMaterial{{ID: preparationKey, Scope: "fixture", Key: bytes.Clone(s.key)}}}, nil
}

func TestComputerPreparationPublicationAndRestore(t *testing.T) {
	objects, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	raw := filepath.Join(dir, "seed.raw")
	file, err := os.Create(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(computer.SeedCapacity); err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteAt([]byte("customer-state"), 8192); err != nil {
		t.Fatal(err)
	}
	file.Close()
	packed := filepath.Join(dir, "seed.pack")
	seed, err := computer.EncodeSeed(t.Context(), raw, packed)
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
	target := workerapi.RuntimeReconcileTarget{ID: uuid.NewV7().String(), WorkerEpoch: 1, DesiredVersion: 1, Source: workerapi.RuntimeSource{WorkspaceID: uuid.NewV7().String(), ReservedDiskMiB: computer.SeedCapacity / mebibyte, Computer: &workerapi.RuntimeComputerSource{VersionID: uuid.NewV7().String(), LogicalBytes: computer.SeedCapacity, Seed: &workerapi.ComputerSeed{Profile: computer.SeedProfile, Object: workerapi.CASObject{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}}}}}
	const budget = 64 << 20
	for _, failure := range []string{"capacity", "register", "upload", "commit", ""} {
		t.Run("failure="+failure, func(t *testing.T) {
			admitted := int64(budget)
			if failure == "capacity" {
				admitted--
			}
			ledger, err := capacity.New(capacity.Vector{CPUMillis: 1, MemoryBytes: 1, GuestEphemeralDiskBytes: admitted})
			if err != nil {
				t.Fatal(err)
			}
			client := &computerPreparationTransport{Store: objects, targets: map[string]workerapi.RuntimeReconcileTarget{target.ID: target}, fail: failure, key: bytes.Repeat([]byte{7}, 32)}
			pool := &PreparedRuntimePool{TempDir: t.TempDir(), CAS: objects, ComputerRanges: objects, ComputerObjects: client, ComputerPreparation: client, ComputerStagingBytes: budget, Capacity: ledger}
			disk, err := pool.prepareComputerGeneration(t.Context(), target)
			if failure != "" {
				if err == nil || disk != nil {
					t.Fatal("failed preparation exposed generation")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				data := make([]byte, 14)
				if _, err := disk.ReadAt(t.Context(), data, 8192); err != nil || string(data) != "customer-state" {
					t.Fatalf("initial data: %q %v", data, err)
				}
				if err := disk.Close(); err != nil {
					t.Fatal(err)
				}
				if err := pool.releaseRuntimeCapacity(target.ID, target.WorkerEpoch); err != nil {
					t.Fatal(err)
				}
				continuation := target
				source := *target.Source.Computer
				source.Seed = nil
				continuation.Source.Computer = &source
				disk, err = pool.prepareComputerGeneration(t.Context(), continuation)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := disk.ReadAt(t.Context(), data, 8192); err != nil || string(data) != "customer-state" {
					t.Fatalf("restored data: %q %v", data, err)
				}
				if client.publications != 1 {
					t.Fatal("continuation republished seed")
				}
				if err := disk.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "capacity" && !errors.Is(err, errPreparedRuntimeCapacityBusy) {
				t.Fatal("capacity failure was not deferred")
			}
			if err := pool.releaseRuntimeCapacity(target.ID, target.WorkerEpoch); err != nil {
				t.Fatal(err)
			}
			if len(ledger.Snapshot().Reservations) != 0 {
				t.Fatal("reservation leaked")
			}
		})
	}
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
	for _, target := range items {
		if err := pool.ReclaimFailedRuntimeTarget(t.Context(), client, target); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(pool.TempDir); err != nil {
		t.Fatal(err)
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
	if err := pool.ReclaimFailedRuntimeTarget(t.Context(), client, target); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(pool.TempDir); err != nil {
		t.Fatal(err)
	}
}

func configureComputerPreparationTest(t *testing.T, pool *PreparedRuntimePool, targets []workerapi.RuntimeReconcileTarget) {
	t.Helper()
	if os.Getenv("HELMR_DISPOSABLE_NBD_PROOF") != "1" {
		t.Skip("requires disposable NBD host")
	}
	// Keep attachment evidence outside testing's automatic directory cleanup.
	// The qualification harness removes it only after device/process postflight.
	arena, err := os.MkdirTemp("", "worker-nbd-")
	if err != nil {
		t.Fatal(err)
	}
	pool.TempDir = arena
	t.Log("NBD evidence arena:", arena)
	pool.ComputerHelper = os.Getenv("HELMR_NBD_TEST_HELPER")
	pool.ComputerDevices = []string{"/dev/nbd14", "/dev/nbd15"}
	objects, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{8}, 32)
	writer := blockformat.Writer{Source: objects, Sink: objects, Scope: "fixture", ActiveKey: preparationKey, Keys: map[string][]byte{preparationKey: key}, PackLimit: blockformat.MinPackLimit}
	locator, err := writer.Empty(t.Context(), computer.SeedCapacity, 64)
	if err != nil {
		t.Fatal(err)
	}
	root, err := computer.NewGenerationRoot(locator, computer.SeedCapacity)
	if err != nil {
		t.Fatal(err)
	}
	client := &computerPreparationTransport{Store: objects, root: root, key: key, targets: make(map[string]workerapi.RuntimeReconcileTarget)}
	for _, target := range targets {
		client.targets[target.ID] = target
	}
	pool.ComputerPreparation = client
	pool.ComputerObjects = client
	pool.ComputerRanges = objects
	pool.CAS = objects
	pool.ComputerStagingBytes = 64 << 20
	n := int64(len(targets))
	pool.Capacity, err = capacity.New(capacity.Vector{CPUMillis: 1000 * n, MemoryBytes: n * 512 << 20, GuestEphemeralDiskBytes: n * (2*computer.SeedCapacity + pool.ComputerStagingBytes), VMSlots: n})
	if err != nil {
		t.Fatal(err)
	}
}
