package computerhost

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/objectcache"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func optionalCacheMachines(t *testing.T) *PreparedMachines {
	t.Helper()
	p, policy, _ := optionalMachines(t)
	var err error
	p.Reservations, err = reservation.New(reservation.Vector{CPUMillis: 4000, MemoryBytes: 400, HostDiskBytes: 256 << 20, VMSlots: 4})
	if err != nil {
		t.Fatal(err)
	}
	p.ObjectCache, err = objectcache.Open(filepath.Join(t.TempDir(), "cache"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.ObjectCache.Close(); err != nil {
			t.Error(err)
		}
	})
	policy.Optional = workerapi.OptionalResources{DiskBytes: 128 << 20, MaxTasks: 1, IOBytesPerSecond: 1000}
	p.ApplyResourcePolicy(policy)
	p.ObjectCacheCapacity = 128 << 20
	return p
}

func resizeTestCache(t *testing.T, p *PreparedMachines, want int64) {
	t.Helper()
	p.optional.mu.Lock()
	err := p.resizeOptionalCacheLocked(want)
	p.optional.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

func TestOptionalCacheRetainsPinsAndUnblocksRealCapacityAfterRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := optionalCacheMachines(t)
		resizeTestCache(t, p, cacheFilesystemHeadroom+16384)
		data := []byte("retained cache source")
		d := cas.Descriptor{Digest: sha256sum.DigestBytes(data), SizeBytes: int64(len(data)), MediaType: "application/octet-stream"}
		if ok, err := p.ObjectCache.Seed(t.Context(), "test", d, bytes.NewReader(data)); !ok || err != nil {
			t.Fatalf("seed: %v %v", ok, err)
		}
		lease := p.ObjectCache.Lease(t.Context(), "test", d)
		if lease == nil {
			t.Fatal("missing pin")
		}
		resizeTestCache(t, p, 0)
		if got := p.ResourceObservation(); !got.ReclaimPending || got.OptionalDiskBytes != cacheFilesystemHeadroom+4096 {
			t.Fatalf("pin lost its physical charge: %+v", got)
		}
		request := reservation.Vector{CPUMillis: 1000, MemoryBytes: 100, HostDiskBytes: 256 << 20, VMSlots: 1}
		admitted := make(chan error, 1)
		var wake allocationWake
		go func() {
			admitted <- p.waitRealAllocationCapacity(t.Context(), func(context.Context) error { return nil }, instanceReservationKey("real", 1), request, &wake)
		}()
		synctest.Wait()
		select {
		case err := <-admitted:
			t.Fatalf("real work started before reader release: %v", err)
		default:
		}
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
		resizeTestCache(t, p, 0)
		if err := <-admitted; err != nil {
			t.Fatal(err)
		}
		if p.Reservations.Snapshot().Used != request {
			t.Fatal("physical release left overlapping charges")
		}
	})
}

func TestOptionalCacheTaskCancellationAndZeroBaseline(t *testing.T) {
	p := optionalCacheMachines(t)
	resizeTestCache(t, p, cacheFilesystemHeadroom+16384)
	ctx, finish, ok := p.beginCacheTask(t.Context())
	if !ok {
		t.Fatal("cache-only policy rejected task")
	}
	if _, _, second := p.beginCacheTask(t.Context()); second {
		t.Fatal("cache task ceiling exceeded")
	}
	if p.ResourceObservation().LocalOptional.Tasks != 1 {
		t.Fatal("cache task missing from observation")
	}
	resizeTestCache(t, p, 0)
	if ctx.Err() == nil {
		t.Fatal("cache shrink left transfer wait running")
	}
	finish()
	p.ApplyResourcePolicy(workerapi.ResourcePolicy{Revision: 2})
	closed, cancel := context.WithCancel(t.Context())
	cancel()
	_ = p.RunOptionalResources(closed)
	if got := p.ResourceObservation(); got.OptionalDiskBytes != 0 || got.LocalOptional.Tasks != 0 || got.ReclaimPending {
		t.Fatalf("optional zero retained work: %+v", got)
	}
}

func TestOptionalCachePolicyStopsNewFillsBeforeCleanupTick(t *testing.T) {
	p := optionalCacheMachines(t)
	resizeTestCache(t, p, cacheFilesystemHeadroom+16384)
	policy := p.optional.policy
	policy.Revision++
	policy.Optional.DiskBytes = 0
	p.ApplyResourcePolicy(policy)
	if _, finish, allowed := p.beginCacheTask(t.Context()); allowed {
		finish()
		t.Fatal("old cache limit admitted work under zero disk policy")
	}
	if p.ResourceObservation().OptionalDiskBytes == 0 {
		t.Fatal("policy application invented physical release")
	}
}

func TestOptionalCacheZeroTasksAndRateRetainPermittedResidentData(t *testing.T) {
	for _, limit := range []string{"revision", "tasks", "rate", "both"} {
		t.Run(limit, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := optionalCacheMachines(t)
				want := cacheFilesystemHeadroom + 16384
				resizeTestCache(t, p, want)
				data := []byte("resident bytes survive optional seed retirement")
				d := cas.Descriptor{Digest: sha256sum.DigestBytes(data), SizeBytes: int64(len(data)), MediaType: "application/octet-stream"}
				if ok, err := p.ObjectCache.Seed(t.Context(), "test", d, bytes.NewReader(data)); !ok || err != nil {
					t.Fatalf("seed cache: %v %v", ok, err)
				}
				policy := p.optional.policy
				policy.Optional.CPUMillis, policy.Optional.MemoryBytes, policy.Optional.VMSlots = 1000, 100, 1
				policy.Optional.MaxTasks = 2
				p.ApplyResourcePolicy(policy)
				seed := instanceReservationKey("joining", 1)
				charge := reservation.Vector{CPUMillis: 1000, MemoryBytes: 100, HostDiskBytes: 16 << 20, VMSlots: 1}
				if err := p.reserveOptionalSeed(seed, charge); err != nil {
					t.Fatal(err)
				}
				transfer := make(chan error, 1)
				go func() {
					body := &optionalTransferBody{ReadCloser: io.NopCloser(bytes.NewReader(data)), ctx: t.Context(), machines: p}
					got, err := io.ReadAll(body)
					if len(got) != 0 {
						t.Error("revoked transfer read resident source bytes")
					}
					transfer <- err
				}()
				synctest.Wait()
				policy.Revision++
				if limit == "tasks" || limit == "both" {
					policy.Optional.MaxTasks = 0
				}
				if limit == "rate" || limit == "both" {
					policy.Optional.IOBytesPerSecond = 0
				}
				p.ApplyResourcePolicy(policy)
				if err := <-transfer; !errors.Is(err, errOptionalReclaimed) {
					t.Fatalf("old transfer survived policy change: %v", err)
				}
				if !p.optionalSeedReclaimRequested(seed) {
					t.Fatal("tracked old seed did not request retirement")
				}
				// The owner requests reclaim again while joining its physical cleanup.
				p.requestOptionalSeedReclaim(seed)
				if _, finish, allowed := p.beginCacheTask(t.Context()); allowed {
					finish()
					t.Fatal("retiring seed admitted a new cache task")
				}
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- p.RunOptionalResources(ctx) }()
				synctest.Wait()
				for range 3 {
					time.Sleep(time.Second)
					synctest.Wait()
					snapshot := p.Reservations.Snapshot()
					if got := snapshot.Optional[optionalCacheKey()].HostDiskBytes; got != want {
						t.Fatalf("seed retirement changed permitted cache charge: %d", got)
					}
					if snapshot.Optional[seed] != charge || !p.ResourceObservation().ReclaimPending {
						t.Fatal("seed retirement invented physical release")
					}
					lease := p.ObjectCache.Lease(t.Context(), "test", d)
					if lease == nil {
						t.Fatal("permitted resident data was evicted")
					}
					got := make([]byte, len(data))
					_, err := lease.ReadAt(got, 0)
					closeErr := lease.Close()
					if err != nil || closeErr != nil || !bytes.Equal(got, data) {
						t.Fatalf("retained cache bytes: %q %v %v", got, err, closeErr)
					}
				}
				// Disk removal must still reclaim unpinned bytes during seed cleanup.
				policy.Revision++
				policy.Optional.DiskBytes = 0
				p.ApplyResourcePolicy(policy)
				time.Sleep(time.Second)
				synctest.Wait()
				if got := p.Reservations.Snapshot().Optional[optionalCacheKey()].HostDiskBytes; got != 0 {
					t.Fatalf("zero disk retained cache: %d", got)
				}
				cancel()
				<-done
			})
		})
	}
}

func TestRealGrantReclaimsSeedLifecycleDiskBeforeAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, _, shape := optionalMachines(t)
		seed := instanceReservationKey("seed", 1)
		if err := p.reserveOptionalSeed(seed, shape); err != nil {
			t.Fatal(err)
		}
		request := shape
		request.HostDiskBytes = 350
		var wake allocationWake
		done := make(chan error, 1)
		go func() {
			done <- p.waitRealAllocationCapacity(t.Context(), func(context.Context) error { return nil }, instanceReservationKey("real", 1), request, &wake)
		}()
		synctest.Wait()
		if !p.optionalSeedReclaimRequested(seed) || p.Reservations.Snapshot().Used != shape {
			t.Fatal("local lifecycle pressure did not retain and reclaim seed")
		}
		select {
		case err := <-done:
			t.Fatalf("real work started before physical stop: %v", err)
		default:
		}
		if err := p.Reservations.Release(seed); err != nil {
			t.Fatal(err)
		}
		p.forgetOptionalSeed(seed)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestOptionalVMAdmissionPreservesCacheUntilDiskNeedsReclaim(t *testing.T) {
	p := optionalCacheMachines(t)
	policy := p.optional.policy
	policy.LocalHeadroomSlots = 0
	policy.Optional.CPUMillis = 3000
	policy.Optional.MemoryBytes = 300
	policy.Optional.VMSlots = 3
	policy.Optional.MaxTasks = 3
	p.ApplyResourcePolicy(policy)
	resizeTestCache(t, p, 96<<20)
	first := reservation.Vector{CPUMillis: 1000, MemoryBytes: 100, HostDiskBytes: 16 << 20, VMSlots: 1}
	if err := p.reserveOptionalSeed(instanceReservationKey("first", 1), first); err != nil {
		t.Fatal(err)
	}
	if got := p.Reservations.Snapshot().Optional[optionalCacheKey()].HostDiskBytes; got != 96<<20 {
		t.Fatalf("cache discarded with room available: %d", got)
	}
	second := first
	second.HostDiskBytes = 32 << 20
	if err := p.reserveOptionalSeed(instanceReservationKey("second", 1), second); err != nil {
		t.Fatal(err)
	}
	if got := p.Reservations.Snapshot().Optional[optionalCacheKey()].HostDiskBytes; got != 80<<20 {
		t.Fatalf("cache did not shrink only by deficit: %d", got)
	}
}

func TestOptionalCacheCopiesLocalArtifactsWithoutNetworkBurstLimit(t *testing.T) {
	p := optionalCacheMachines(t)
	resizeTestCache(t, p, cacheFilesystemHeadroom+(1<<20))
	data := bytes.Repeat([]byte("verified local artifact"), 4096)
	d := cas.Descriptor{Digest: sha256sum.DigestBytes(data), SizeBytes: int64(len(data)), MediaType: "application/octet-stream"}
	file, err := os.CreateTemp(t.TempDir(), "artifact")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	// The object exceeds one second of this network budget. It is already on
	// disk, so its size must not silently prevent an otherwise funded cache fill.
	p.seedObject(t.Context(), "test", d, file)
	lease := p.ObjectCache.Lease(t.Context(), "test", d)
	if lease == nil {
		t.Fatal("local artifact did not populate cache")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if !p.optional.transferNext.IsZero() {
		t.Fatal("local copy consumed network budget")
	}
}
