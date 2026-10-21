package computerhost

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/objectcache"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type artifactCleanupBackend struct {
	vm.Backend
	err error
}

func (b *artifactCleanupBackend) Cleanup(context.Context, vm.Owner) error { return b.err }

type artifactCleanupClient struct {
	AllocatedPreparationClient
	AllocatedBootSeedClient
	stops int
}

func (c *artifactCleanupClient) ObservePreparationStopped(context.Context, workerapi.AllocationIdentity) error {
	c.stops++
	return nil
}
func (c *artifactCleanupClient) ObserveBootSeedStopped(context.Context, workerapi.AllocationIdentity) error {
	c.stops++
	return nil
}

func TestAllocationCleanupReleasesArtifactPinsBeforePhysicalCleanup(t *testing.T) {
	for _, kind := range []string{"preparation", "seed"} {
		t.Run(kind, func(t *testing.T) {
			root, links := t.TempDir(), t.TempDir()
			cache, err := objectcache.Open(filepath.Join(root, "cache"), 16384)
			if err != nil {
				t.Fatal(err)
			}
			if err := cache.EnableDrives(12345, os.Getgid(), links); err != nil {
				t.Fatal(err)
			}
			backend := &artifactCleanupBackend{err: errors.New("physical absence unproved")}
			capacity, err := reservation.New(reservation.Vector{CPUMillis: 1, MemoryBytes: 1, HostDiskBytes: 4096})
			if err != nil {
				t.Fatal(err)
			}
			p := &PreparedMachines{ObjectCache: cache, Backend: backend, TempDir: root, Reservations: capacity}
			images := &programSnapshots{}
			// Keep cleanup bounded even when a regression leaves a pin behind.
			t.Cleanup(func() { _ = images.close(); _ = cache.Close() })
			for _, role := range []string{"program", "runtime"} {
				data := []byte(role)
				d := cas.Descriptor{Digest: sha256sum.DigestBytes(data), SizeBytes: int64(len(data)), MediaType: artifact.ProgramArtifactMediaType}
				if role == "runtime" {
					d.MediaType = artifact.RuntimeArtifactMediaType
				}
				if seeded, err := cache.Seed(t.Context(), role, d, bytes.NewReader(data)); err != nil || !seeded {
					t.Fatal("seed", seeded, err)
				}
				if role == "program" {
					images.artifact, err = p.readProgramArtifact(t.Context(), root, artifact.ProgramDescriptor(d))
					if err == nil {
						err = images.artifact.LinkInto(links, role, 12345, os.Getgid())
					}
				} else {
					images.runtime, err = p.readRuntimeArtifact(t.Context(), root, artifact.RuntimeDescriptor{Digest: d.Digest, SizeBytes: d.SizeBytes, MediaType: d.MediaType, Architecture: definition.ArchitectureX8664, RuntimeContract: definition.RuntimeContract, FormatVersion: artifact.RuntimeDescriptorFormatVersion})
					if err == nil {
						err = images.runtime.LinkInto(links, role, 12345, os.Getgid())
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(filepath.Join(links, role))
				if err != nil || info.Mode().Perm() != 0440 {
					t.Fatal("expected shared drive", err)
				}
			}
			identity := workerapi.AllocationIdentity{Kind: kind, AllocationID: "allocation", Epoch: 1}
			if _, err := capacity.Reserve(instanceReservationKey(identity.AllocationID, identity.Epoch), reservation.Vector{HostDiskBytes: 4096}); err != nil {
				t.Fatal(err)
			}
			directory := p.computerBootProgramDirectory(identity.AllocationID, identity.Epoch)
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			client := &artifactCleanupClient{}
			var run func(context.Context) error
			var finished func() bool
			var retained func() bool
			if kind == "preparation" {
				disk, err := os.Create(filepath.Join(directory, "disk"))
				if err != nil {
					t.Fatal(err)
				}
				defer disk.Close()
				owner := &PreparationAllocationOwner{machines: p, client: client, identity: identity, closing: true, reserved: true, stage: &preparationStage{directory: directory, disk: disk, images: images}}
				run, finished = owner.Run, owner.Finished
				retained = func() bool { _, err := disk.Stat(); return owner.reserved && owner.stage.disk == disk && err == nil }
			} else {
				owner := &BootSeedAllocationOwner{machines: p, client: client, identity: identity, closing: true, reserved: true, images: images, active: &preparationQualification{identity: identity}}
				run, finished = owner.Run, owner.Finished
				retained = func() bool { return owner.reserved && owner.active != nil }
			}
			if err := run(t.Context()); !errors.Is(err, backend.err) {
				t.Fatal("physical cleanup error lost", err)
			}
			if finished() || !retained() || client.stops != 0 || len(capacity.Snapshot().Reservations) != 1 {
				t.Fatal("failed cleanup released physical custody")
			}
			if _, err := os.Stat(directory); err != nil {
				t.Fatal("failed cleanup removed staging", err)
			}
			done := make(chan error, 1)
			go func() { done <- cache.Close() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("terminal cleanup retained an artifact pin")
			}
			for _, role := range []string{"program", "runtime"} {
				data, err := os.ReadFile(filepath.Join(links, role))
				if err != nil || string(data) != role {
					t.Fatal("live VM link lost", role, err)
				}
			}
			backend.err = nil
			// Embedded nil startup methods make any relaunch fail the test.
			if err := run(t.Context()); err != nil || !finished() {
				t.Fatal("cleanup retry", err)
			}
			if client.stops != 1 || len(capacity.Snapshot().Reservations) != 0 {
				t.Fatal("successful cleanup retained custody")
			}
			if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("successful cleanup retained staging", err)
			}
		})
	}
}
