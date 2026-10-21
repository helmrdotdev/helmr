package computerhost

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type gatedCheckpointRestoreStore struct {
	cas.ImmutableStore
	started         chan string
	release         chan struct{}
	cancelObserved  chan struct{}
	finishCancelled chan struct{}
	failDigest      string
	blockedDigest   string
	active, maximum atomic.Int32
}

func (s *gatedCheckpointRestoreStore) Get(ctx context.Context, digest string) (io.ReadCloser, error) {
	active := s.active.Add(1)
	for old := s.maximum.Load(); active > old; old = s.maximum.Load() {
		if s.maximum.CompareAndSwap(old, active) {
			break
		}
	}
	s.started <- digest
	if digest == s.blockedDigest {
		<-ctx.Done()
		close(s.cancelObserved)
		// Cancellation is not completion. Keep this operation alive until the
		// test explicitly permits its final independent error to be observed.
		<-s.finishCancelled
		s.active.Add(-1)
		return nil, ErrCheckpointKeyUnavailable
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		s.active.Add(-1)
		return nil, ctx.Err()
	}
	if digest == s.failDigest {
		s.active.Add(-1)
		return nil, io.ErrUnexpectedEOF
	}
	body, err := s.ImmutableStore.Get(ctx, digest)
	if err != nil {
		s.active.Add(-1)
		return nil, err
	}
	return &countedCheckpointBody{ReadCloser: body, active: &s.active}, nil
}

type countedCheckpointBody struct {
	io.ReadCloser
	active *atomic.Int32
}

func (b *countedCheckpointBody) Close() error {
	err := b.ReadCloser.Close()
	b.active.Add(-1)
	return err
}

func TestComputerCheckpointRestoreJoinsBoundedStaging(t *testing.T) {
	for _, failure := range []bool{false, true} {
		name := "success"
		if failure {
			name = "cancel-and-join"
		}
		t.Run(name, func(t *testing.T) {
			source, client, _, _ := checkpointPipelineFixture(t, "", "")
			if err := source.captureIdle(t.Context(), workerapi.ComputerAllocationDelivery{ChannelCredential: "private"}, func() func() { return func() {} }); !errors.Is(err, errComputerHibernated) {
				t.Fatal(err)
			}
			defer source.checkpointCut.Release()
			manifest := client.manifests[0]
			identity := source.identity
			identity.AllocationID = uuid.NewV7().String()
			identity.Epoch++
			backend := &checkpointRestoreBackend{t: t, identity: identity, machine: &checkpointPipelineMachine{}}
			store := &gatedCheckpointRestoreStore{
				ImmutableStore: source.machines.ComputerObjects,
				started:        make(chan string, 4), release: make(chan struct{}),
				cancelObserved: make(chan struct{}), finishCancelled: make(chan struct{}),
			}
			if failure {
				store.failDigest, store.blockedDigest = manifest.VMConfig.Digest, manifest.VMState.Digest
			}
			paused := false
			target := &ComputerAllocationOwner{
				identity: identity, client: client,
				checkpointKeyUnavailable: func() { paused = true },
				machines:                 &PreparedMachines{Backend: backend, CheckpointCipher: source.machines.CheckpointCipher, ComputerObjects: store, TempDir: t.TempDir()},
			}
			delivery := workerapi.ComputerAllocationDelivery{Identity: identity, BaseVersion: client.save, RestoredFrom: client.save, Shape: workerapi.AllocationShape{VMPlatformID: manifest.Runtime.RuntimeID, VCPUCount: 1, MemoryBytes: 4096, ScratchBytes: 4096}}
			material := workerapi.ComputerAllocationSource{Disk: workerapi.ComputerSourceMaterial{Root: manifest.Disk}}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				done <- target.restore(ctx, delivery, material, nil)
			}()
			defer func() {
				cancel()
				for _, gate := range []chan struct{}{store.release, store.finishCancelled} {
					select {
					case <-gate:
					default:
						close(gate)
					}
				}
				<-finished
			}()
			// Neither first download can finish until both have entered Get.
			for range 2 {
				select {
				case <-store.started:
				case err := <-done:
					t.Fatalf("restore returned before parallel staging: %v", err)
				case <-ctx.Done():
					t.Fatal("checkpoint downloads did not overlap")
				}
			}
			close(store.release)
			if failure {
				select {
				case <-store.cancelObserved:
				case <-ctx.Done():
					t.Fatal("failed staging did not cancel its peer")
				}
				select {
				case err := <-done:
					t.Fatalf("restore returned before cancelled peer joined: %v", err)
				default:
				}
				if backend.calls != 0 {
					t.Fatal("restore created a VM before all staging joined")
				}
				close(store.finishCancelled)
			}
			select {
			case err := <-done:
				if failure {
					if !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, ErrCheckpointKeyUnavailable) || !paused || backend.calls != 0 || target.invalidCheckpointID != "" {
						t.Fatalf("restore lost joined failure identities: %v paused=%v calls=%d invalid=%q", err, paused, backend.calls, target.invalidCheckpointID)
					}
				} else if err != nil || paused || backend.calls != 1 || target.checkpoint == nil {
					t.Fatalf("restore after authenticated staging: %v paused=%v calls=%d", err, paused, backend.calls)
				}
			case <-ctx.Done():
				t.Fatal("restore did not finish")
			}
			if store.active.Load() != 0 || store.maximum.Load() != 2 {
				t.Fatalf("staging ownership active=%d maximum=%d", store.active.Load(), store.maximum.Load())
			}
		})
	}
}
