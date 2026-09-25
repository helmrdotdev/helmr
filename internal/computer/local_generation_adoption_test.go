//go:build linux || darwin

package computer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

func adoptionPublisher(t *testing.T, cfg LocalGenerationConfig) *continuationTestPublication {
	t.Helper()
	remote := cfg.BaseSource.(*cas.File)
	p := &continuationTestPublication{generationTestPublication: &generationTestPublication{remote: remote, registered: map[string]blockformat.ObjectInspection{}, certified: map[string]blockformat.ObjectInspection{}}}
	locator, err := cfg.Base.Locator(cfg.Base.LogicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishGeneration(t.Context(), remote, remote, cfg.Scope, cfg.Keys, locator, cfg.Base.LogicalBytes, 1000, p, nil); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLocalAdoptionReclaimsSavedBytesPreservingSuccessorsAndReopen(t *testing.T) {
	cfg, _ := localGenerationFixture(t)
	cfg.StagedBytes = 96 << 10
	pub := adoptionPublisher(t, cfg)
	p, err := CreateLocalGeneration(t.Context(), cfg)
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
		if err = capture.Adopt(t.Context(), 1000); err == nil {
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
		if err = capture.Adopt(t.Context(), 1000); err != nil {
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
			p, err = OpenLocalGeneration(t.Context(), cfg)
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

func TestLocalAdoptionFailurePreservesLocalData(t *testing.T) {
	for _, failure := range []string{"missing remote", "missing remote segment", "budget", "root-synced", "root-renamed", "root-committed"} {
		t.Run(failure, func(t *testing.T) {
			cfg, remotePath := localGenerationFixture(t)
			pub := adoptionPublisher(t, cfg)
			p, err := CreateLocalGeneration(t.Context(), cfg)
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
					local, e := generationObjectLocal(t.Context(), p.store, segment.Digest, segment.Size)
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
			if err = capture.Adopt(t.Context(), budget); err == nil {
				t.Fatal("adoption unexpectedly succeeded")
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
			reopened, err := OpenLocalGeneration(t.Context(), cfg)
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

func TestLocalAdoptionCollectionSkipsEvictionUntilPublisherJoins(t *testing.T) {
	cfg, _ := localGenerationFixture(t)
	pub := adoptionPublisher(t, cfg)
	p, err := CreateLocalGeneration(t.Context(), cfg)
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
	if err = capture.Adopt(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	blocked := &blockedGenerationPublication{continuationTestPublication: pub, entered: make(chan struct{}), resume: make(chan struct{})}
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
