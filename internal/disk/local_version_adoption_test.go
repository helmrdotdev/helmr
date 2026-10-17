//go:build linux || darwin

package disk

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

func adoptionPublisher(t *testing.T, cfg LocalVersionConfig) *continuationTestPublication {
	t.Helper()
	remote := cfg.BaseSource.(*cas.File)
	p := &continuationTestPublication{versionTestPublication: &versionTestPublication{remote: remote, registered: map[string]blockformat.ObjectInspection{}, certified: map[string]blockformat.ObjectInspection{}}}
	locator, err := cfg.Base.Locator(cfg.Base.LogicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishVersion(t.Context(), remote, remote, cfg.Scope, cfg.Keys, locator, cfg.Base.LogicalBytes, 1000, p, nil); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLocalAdoptionReclaimsSavedBytesPreservingSuccessorsAndReopen(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	cfg.StagedBytes = 256 << 10
	pub := adoptionPublisher(t, cfg)
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { p.Close() }()
	var reclaimed int64
	for i := 1; i <= 32; i++ {
		offset := int64(i * 4096)
		want := bytes.Repeat([]byte{byte(i)}, 4096)
		if _, err = p.WriteAt(t.Context(), want, offset); err != nil {
			t.Fatal(err)
		}
		capture, err := p.Capture(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err = capture.Adopt(t.Context()); err == nil {
			t.Fatal("unpublished capture adopted")
		}
		if err = capture.Publish(t.Context(), pub); err != nil {
			t.Fatal(err)
		}
		// The fake publisher models remote retention only. The Control Plane
		// receipt/acknowledgement protocol is outside this storage-owner test.
		if _, err = p.WriteAt(t.Context(), []byte{byte(i + 1)}, 7); err != nil {
			t.Fatal(err)
		}
		if _, err = p.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err = p.WriteAt(t.Context(), []byte{99}, 8); err != nil {
			t.Fatal(err)
		}
		if err = capture.Adopt(t.Context()); err != nil {
			t.Fatal(err)
		}
		n, err := p.Collect(t.Context(), 1000)
		if err != nil || n <= 0 {
			t.Fatalf("collect %d: %d %v", i, n, err)
		}
		reclaimed += n
		if p.head.Base != cfg.Base || p.head.Saved != capture.Root() || p.head.Root == capture.Root() {
			t.Fatal("adoption changed the execution base or rolled back successor")
		}
		if err = capture.Publish(t.Context(), pub); err != nil {
			t.Fatalf("retained capture could not reuse evicted bytes: %v", err)
		}
		capture.Release()
		got := make([]byte, 4096)
		if _, err = p.ReadAt(t.Context(), got, offset); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("remote read after eviction: %v", err)
		}
		got = make([]byte, 2)
		if _, err = p.ReadAt(t.Context(), got, 7); err != nil || !bytes.Equal(got, []byte{byte(i + 1), 99}) {
			t.Fatalf("successor data: %v %v", got, err)
		}
		if _, err = p.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
		if i == 16 {
			if err = p.Close(); err != nil {
				t.Fatal(err)
			}
			p, err = OpenLocalVersion(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if p.head.Saved != capture.Root() || localByte(t, p) != byte(i+1) {
				t.Fatal("reopen lost adopted source or newer local data")
			}
		}
	}
	if reclaimed <= cfg.StagedBytes {
		t.Fatalf("lifetime writes did not exceed admission: %d", reclaimed)
	}
}

func TestLocalAdoptionAndBackingFailurePreservesLocalData(t *testing.T) {
	for _, failure := range []string{"missing remote", "missing remote segment", "budget", "root-synced", "root-renamed", "root-committed"} {
		t.Run(failure, func(t *testing.T) {
			cfg, remotePath := localVersionFixture(t)
			pub := adoptionPublisher(t, cfg)
			p, err := CreateLocalVersion(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			p.WriteAt(t.Context(), []byte{8}, 7)
			capture, err := p.Capture(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer capture.Release()
			if err = capture.Publish(t.Context(), pub); err != nil {
				t.Fatal(err)
			}
			budget := 1000
			if failure == "missing remote" {
				if err = os.Remove(filepath.Join(remotePath, "sha256", capture.Root().Pack.Digest[len("sha256:"):])); err != nil {
					t.Fatal(err)
				}
			} else if failure == "missing remote segment" {
				removed := false
				for _, inspection := range pub.certified {
					if inspection.Segment == nil {
						continue
					}
					segment := inspection.Segment
					local, e := versionObjectLocal(t.Context(), p.store, segment.Digest, segment.Size)
					if e != nil {
						t.Fatal(e)
					}
					if !local {
						continue
					}
					descriptor := inspectionDescriptor(inspection)
					if e = os.Remove(filepath.Join(remotePath, "sha256", descriptor.Digest[len("sha256:"):])); e != nil {
						t.Fatal(e)
					}
					removed = true
					break
				}
				if !removed {
					t.Fatal("fixture has no new segment")
				}
			} else if failure == "budget" {
				budget = 1
			} else {
				p.phase = func(phase string) error {
					if phase == failure {
						return errors.New("injected adoption commit failure")
					}
					return nil
				}
			}
			if failure == "missing remote" || failure == "missing remote segment" {
				// Force the independent backing verification to read remote bytes.
				// Evicting inspections does not revoke a capture's publication proof.
				p.evidence = newVersionEvidenceCache()
			}
			if failure == "missing remote" || failure == "missing remote segment" || failure == "budget" {
				err = capture.BackRemotely(t.Context(), budget)
			} else {
				err = capture.Adopt(t.Context())
			}
			if err == nil {
				t.Fatal("backing or adoption unexpectedly succeeded")
			}
			if capture.adopted || capture.ReadyForRetirement() {
				t.Fatal("failed operation authorized retirement")
			}
			p.phase = nil
			if p.head.Saved != cfg.Base || localByte(t, p) != 8 {
				t.Fatal("failed adoption discarded local state")
			}
			if _, err = p.Collect(t.Context(), 1000); err != nil {
				t.Fatal(err)
			}
			if localByte(t, p) != 8 {
				t.Fatal("collection lost data after failed adoption")
			}
			if err = p.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenLocalVersion(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			wantSaved := cfg.Base
			if failure == "root-renamed" || failure == "root-committed" {
				wantSaved = capture.Root()
			}
			if reopened.head.Saved != wantSaved || localByte(t, reopened) != 8 {
				t.Fatal("ambiguous adoption reopen lost durable data")
			}
		})
	}
}

func TestLocalAdoptionCanSkipReleasedPublishedCuts(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	pub := adoptionPublisher(t, cfg)
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { p.Close() }()
	var latest *LocalCapture
	for i := byte(1); i <= 3; i++ {
		if _, err = p.WriteAt(t.Context(), bytes.Repeat([]byte{i}, 4096), 4096); err != nil {
			t.Fatal(err)
		}
		cut, err := p.Capture(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err = cut.Publish(t.Context(), pub); err != nil {
			t.Fatal(err)
		}
		if latest != nil {
			latest.Release()
		}
		latest = cut
	}
	defer latest.Release()
	// A successor stays local and must survive both a failed adoption of the
	// latest publication and collection after its older pins were released.
	if _, err = p.WriteAt(t.Context(), []byte{99}, 7); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := p.head
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err = latest.Adopt(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled adoption: %v", err)
	}
	if p.head != before || latest.ReadyForRetirement() {
		t.Fatal("failed adoption advanced authority")
	}
	check := func() {
		t.Helper()
		got := make([]byte, 4096)
		if _, err := p.ReadAt(t.Context(), got, 4096); err != nil || !bytes.Equal(got, bytes.Repeat([]byte{3}, 4096)) {
			t.Fatalf("latest bytes: %v", err)
		}
		if localByte(t, p) != 99 {
			t.Fatal("unpublished successor lost")
		}
	}
	if _, err = p.Collect(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	check()
	if err = latest.Adopt(t.Context()); err != nil {
		t.Fatal(err)
	}
	latest.Release()
	if _, err = p.Collect(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	check()
	if p.head.Saved != latest.Root() || p.head.Root == latest.Root() {
		t.Fatal("adoption rolled back current root")
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	p, err = OpenLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	check()
	if p.head.Saved != latest.Root() {
		t.Fatal("reopen lost adopted source")
	}
}

func TestLocalAdoptionCollectionSkipsEvictionUntilPublisherJoins(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	pub := adoptionPublisher(t, cfg)
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err = p.WriteAt(t.Context(), []byte{8}, 7); err != nil {
		t.Fatal(err)
	}
	capture, err := p.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Release()
	if err = capture.Publish(t.Context(), pub); err != nil {
		t.Fatal(err)
	}
	if err = capture.Adopt(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Evict receipts to exercise a publisher that must reopen local files.
	p.evidence = newVersionEvidenceCache()
	blocked := &blockedVersionPublication{continuationTestPublication: pub, entered: make(chan struct{}), resume: make(chan struct{})}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- capture.Publish(ctx, blocked) }()
	select {
	case <-blocked.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	collected := make(chan error, 1)
	go func() {
		n, e := p.Collect(ctx, 1000)
		if n != 0 && e == nil {
			e = errors.New("evicted a publisher's local files")
		}
		collected <- e
	}()
	select {
	case err = <-collected:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("collection waited for upload", ctx.Err())
	}
	if _, err = p.WriteAt(ctx, []byte{9}, 7); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	close(blocked.resume)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if n, e := p.Collect(ctx, 1000); e != nil || n <= 0 {
		t.Fatalf("joined eviction: %d %v", n, e)
	}
	if localByte(t, p) != 9 {
		t.Fatal("collection lost successor")
	}
}

func TestLocalAdoptionReusesExactPublicationAfterInspectionEviction(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	pub := adoptionPublisher(t, cfg)
	remote := &measuredRangeSource{RangeSource: cfg.BaseSource}
	cfg.BaseSource = remote
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err = p.WriteAt(t.Context(), []byte{8}, 7); err != nil {
		t.Fatal(err)
	}
	capture, err := p.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Release()
	if err = capture.Publish(t.Context(), pub); err != nil {
		t.Fatal(err)
	}
	// A completed exact publication survives eviction of individual inspections.
	// The caller's confirmed recovery-head receipt is modeled by this fixture.
	p.evidence = newVersionEvidenceCache()
	before := remote.calls
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err = capture.Adopt(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled adoption: %v", err)
	}
	if capture.adopted || p.head.Saved != cfg.Base || capture.ReadyForRetirement() {
		t.Fatal("cancellation advanced adoption or retirement")
	}
	if err = capture.Adopt(t.Context()); err != nil {
		t.Fatal(err)
	}
	if remote.calls != before || !capture.adopted || p.head.Saved != capture.Root() {
		t.Fatalf("publication proof was not reused: remote reads %d, adopted %v", remote.calls-before, capture.adopted)
	}
}

func TestLocalCollectionRejectsDamagedCertifiedRemoteBacking(t *testing.T) {
	for _, kind := range []string{"pack", "segment"} {
		for _, damage := range []string{"missing", "corrupt"} {
			t.Run(kind+"/"+damage, func(t *testing.T) {
				cfg, remotePath := localVersionFixture(t)
				pub := adoptionPublisher(t, cfg)
				p, err := CreateLocalVersion(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer p.Close()
				if _, err = p.WriteAt(t.Context(), []byte{8}, 7); err != nil {
					t.Fatal(err)
				}
				capture, err := p.Capture(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer capture.Release()
				if err = capture.Publish(t.Context(), pub); err != nil {
					t.Fatal(err)
				}
				if err = capture.Adopt(t.Context()); err != nil {
					t.Fatal(err)
				}
				digest := capture.Root().Pack.Digest
				if kind == "segment" {
					digest = ""
					for _, inspection := range pub.certified {
						if inspection.Segment == nil {
							continue
						}
						segment := inspection.Segment
						local, e := versionObjectLocal(t.Context(), p.store, segment.Digest, segment.Size)
						if e != nil {
							t.Fatal(e)
						}
						if local {
							digest = inspectionDescriptor(inspection).Digest
							break
						}
					}
					if digest == "" {
						t.Fatal("fixture has no local segment")
					}
				}
				path := filepath.Join(remotePath, "sha256", digest[len("sha256:"):])
				if damage == "missing" {
					err = os.Remove(path)
				} else {
					if err = os.Chmod(path, 0600); err != nil {
						t.Fatal(err)
					}
					err = os.WriteFile(path, []byte("corrupt"), 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
				p.evidence = newVersionEvidenceCache()
				capture.Release()
				if removed, err := p.Collect(t.Context(), 1000); err == nil || removed != 0 {
					t.Fatalf("collection accepted damaged backing: %d %v", removed, err)
				}
				if localByte(t, p) != 8 {
					t.Fatal("failed backing verification discarded local bytes")
				}
			})
		}
	}
}
