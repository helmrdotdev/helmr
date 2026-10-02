//go:build linux

package computerhost

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type computerPreparationTransport struct {
	cas.Store
	mu           sync.Mutex
	targets      map[string]workerapi.InstanceReconcileTarget
	root         disk.VersionRoot
	fail         string
	key          []byte
	publications int
	seedStatuses []string
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
func (s *computerPreparationTransport) PrepareComputerSeed(context.Context, workerapi.PrepareComputerSeedRequest) (workerapi.ComputerSeedPreparation, error) {
	if len(s.seedStatuses) > 0 {
		status := s.seedStatuses[0]
		s.seedStatuses = s.seedStatuses[1:]
		return workerapi.ComputerSeedPreparation{Status: status}, nil
	}
	return workerapi.ComputerSeedPreparation{Status: "convert", Key: &workerapi.ComputerKeyMaterial{Scope: "fixture", ID: preparationKey, Key: bytes.Clone(s.key)}}, nil
}

func TestSharedSeedPreparationAvoidsRepeatedConversion(t *testing.T) {
	for _, waiting := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "waiting"}[waiting], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				transport := &computerPreparationTransport{seedStatuses: []string{"ready"}}
				if waiting {
					transport.seedStatuses = []string{"waiting", "ready"}
				}
				machines := &PreparedMachines{ComputerPreparation: transport, ComputerObjects: transport}
				// No seed bytes or conversion directory exist. Adoption must finish
				// without touching either, including after a pending owner completes.
				if err := machines.publishComputerSeed(t.Context(), workerapi.InstanceReconcileTarget{}, "unused"); err != nil {
					t.Fatal(err)
				}
				if transport.publications != 0 || len(transport.seedStatuses) != 0 {
					t.Fatal("adoption repeated conversion or stopped before ready")
				}
			})
		})
	}
	synctest.Test(t, func(t *testing.T) {
		transport := &computerPreparationTransport{seedStatuses: []string{"waiting"}}
		machines := &PreparedMachines{ComputerPreparation: transport, ComputerObjects: transport}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		time.AfterFunc(time.Millisecond, cancel)
		if err := machines.publishComputerSeed(ctx, workerapi.InstanceReconcileTarget{}, "unused"); !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting ignored cancellation: %v", err)
		}
	})
}
func (s *computerPreparationTransport) PublishInitialComputerVersion(_ context.Context, r workerapi.InitialComputerVersionRequest) (workerapi.InitialComputerVersionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail == "commit" {
		return workerapi.InitialComputerVersionResponse{}, errors.New("commit failed")
	}
	s.root = r.Root
	s.publications++
	target := s.targets[r.ComputerInstanceID]
	return workerapi.InitialComputerVersionResponse{ComputerID: target.Source.ComputerID, VersionID: target.Source.Computer.VersionID}, nil
}
func (s *computerPreparationTransport) ComputerSource(_ context.Context, r workerapi.ComputerSourceRequest) (workerapi.ComputerSourceMaterial, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return workerapi.ComputerSourceMaterial{Root: s.root, VersionID: s.targets[r.ComputerInstanceID].Source.Computer.VersionID, WriteKeyID: preparationKey, Keys: []workerapi.ComputerKeyMaterial{{ID: preparationKey, Scope: "fixture", Key: bytes.Clone(s.key)}}}, nil
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
	if err = file.Truncate(disk.SeedCapacity); err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteAt([]byte("customer-state"), 8192); err != nil {
		t.Fatal(err)
	}
	file.Close()
	packed := filepath.Join(dir, "seed.pack")
	seed, err := disk.EncodeSeed(t.Context(), raw, packed)
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
	target := workerapi.InstanceReconcileTarget{ID: uuid.NewV7().String(), WorkerEpoch: 1, DesiredVersion: 1, Source: workerapi.InstanceSource{ComputerID: uuid.NewV7().String(), ReservedDiskMiB: disk.SeedCapacity / mebibyte, Computer: &workerapi.InstanceComputerSource{VersionID: uuid.NewV7().String(), LogicalBytes: disk.SeedCapacity, Seed: &workerapi.ComputerSeed{Profile: definition.ComputerSeedProfile, Object: workerapi.CASObject{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}}}}}
	const budget = 64 << 20
	for _, failure := range []string{"capacity", "register", "upload", "commit", "", "takeover"} {
		t.Run("failure="+failure, func(t *testing.T) {
			admitted := int64(budget)
			if failure == "capacity" {
				admitted--
			}
			ledger, err := reservation.New(reservation.Vector{CPUMillis: 1, MemoryBytes: 1, GuestEphemeralDiskBytes: admitted})
			if err != nil {
				t.Fatal(err)
			}
			client := &computerPreparationTransport{Store: objects, targets: map[string]workerapi.InstanceReconcileTarget{target.ID: target}, fail: failure, key: bytes.Repeat([]byte{7}, 32)}
			if failure == "takeover" {
				client.seedStatuses = []string{"waiting"}
			}
			machines := &PreparedMachines{TempDir: t.TempDir(), CAS: objects, ComputerRanges: objects, ComputerObjects: client, ComputerPreparation: client, ComputerStagingBytes: budget, Reservations: ledger}
			prepared, err := machines.prepareComputerVersion(t.Context(), target)
			if failure != "" && failure != "takeover" {
				if err == nil || prepared != nil {
					t.Fatal("failed preparation exposed version")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				data := make([]byte, 14)
				if _, err := prepared.ReadAt(t.Context(), data, 8192); err != nil || string(data) != "customer-state" {
					t.Fatalf("initial data: %q %v", data, err)
				}
				if err := prepared.Close(); err != nil {
					t.Fatal(err)
				}
				if err := machines.releaseInstanceCapacity(target.ID, target.WorkerEpoch); err != nil {
					t.Fatal(err)
				}
				continuation := target
				source := *target.Source.Computer
				source.Seed = nil
				source.Root = &client.root
				continuation.Source.Computer = &source
				prepared, err = machines.prepareComputerVersion(t.Context(), continuation)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := prepared.ReadAt(t.Context(), data, 8192); err != nil || string(data) != "customer-state" {
					t.Fatalf("restored data: %q %v", data, err)
				}
				if client.publications != 1 {
					t.Fatal("continuation republished seed")
				}
				if err := prepared.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "capacity" && !errors.Is(err, errPreparedMachineCapacityBusy) {
				t.Fatal("capacity failure was not deferred")
			}
			if err := machines.releaseInstanceCapacity(target.ID, target.WorkerEpoch); err != nil {
				t.Fatal(err)
			}
			if len(ledger.Snapshot().Reservations) != 0 {
				t.Fatal("reservation leaked")
			}
		})
	}
}

func TestReconcileDesiredInstancesRunsBatchConcurrentlyAndWaitsForShutdown(t *testing.T) {
	store, mount := testComputerMountArtifacts(t)
	connector := &blockingMaterializingBackend{
		started: make(chan string, 2), canceled: make(chan string, 2), failID: "instance-0",
	}
	var logs bytes.Buffer
	machines := NewPreparedMachines(connector, store, 2, slog.New(slog.NewTextHandler(&logs, nil)))
	machines.TempDir = t.TempDir()
	machines.RuntimeArchitecture = definition.RuntimeArchitecture("x86_64")
	machines.Reservations = newPreparedMachineReservations(t, 2)
	items := make([]workerapi.InstanceReconcileTarget, 2)
	for i := range items {
		items[i] = instancePreparationTarget(mount, uuid.NewV7().String(), 7)
	}
	configureComputerPreparationTest(t, machines, items)
	connector.failID = items[0].ID
	client := &batchInstanceClient{response: workerapi.InstanceReconcileResponse{Items: items}}
	machines.ComputerInstances = client
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- machines.ReconcileDesiredInstances(ctx, client) }()
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
		if err := machines.reclaimFailedInstanceTarget(t.Context(), client, target); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(machines.TempDir); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `msg="prepared machine phase"`) ||
		!strings.Contains(logs.String(), "phase=test_materialize") {
		t.Fatalf("fresh materialize phase was not logged: %s", logs.String())
	}
}

func TestWarmInstancePreparationDeadlineCancelsBlockedMaterialization(t *testing.T) {
	store, mount := testComputerMountArtifacts(t)
	connector := &blockingMaterializingBackend{started: make(chan string, 1), canceled: make(chan string, 1)}
	machines := NewPreparedMachines(connector, store, 1, nil)
	machines.TempDir = t.TempDir()
	machines.RuntimeArchitecture = definition.RuntimeArchitecture("x86_64")
	machines.Reservations = newPreparedMachineReservations(t, 1)
	client := &typedInstanceClient{}
	machines.ComputerInstances = client
	target := instancePreparationTarget(mount, uuid.NewV7().String(), 7)
	items := []workerapi.InstanceReconcileTarget{target}
	configureComputerPreparationTest(t, machines, items)
	target = items[0]
	target.PreparationExpiresAt = time.Now().Add(2 * time.Second)
	done := make(chan error, 1)
	go func() { done <- machines.warmInstanceTarget(t.Context(), client, target, func() {}) }()
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
	if err := machines.reclaimFailedInstanceTarget(t.Context(), client, target); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(machines.TempDir); err != nil {
		t.Fatal(err)
	}
}

func configureComputerPreparationTest(t *testing.T, machines *PreparedMachines, targets []workerapi.InstanceReconcileTarget) {
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
	machines.TempDir = arena
	t.Log("NBD evidence arena:", arena)
	machines.ComputerHelper = os.Getenv("HELMR_NBD_TEST_HELPER")
	machines.ComputerDevices = []string{"/dev/nbd14", "/dev/nbd15"}
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
	client := &computerPreparationTransport{Store: objects, root: root, key: key, targets: make(map[string]workerapi.InstanceReconcileTarget)}
	for _, target := range targets {
		client.targets[target.ID] = target
	}
	machines.ComputerPreparation = client
	machines.ComputerObjects = client
	machines.ComputerRanges = objects
	machines.CAS = objects
	machines.ComputerStagingBytes = 64 << 20
	n := int64(len(targets))
	machines.Reservations, err = reservation.New(reservation.Vector{CPUMillis: 1000 * n, MemoryBytes: n * 512 << 20, GuestEphemeralDiskBytes: n * (2*disk.SeedCapacity + machines.ComputerStagingBytes), VMSlots: n})
	if err != nil {
		t.Fatal(err)
	}
}
