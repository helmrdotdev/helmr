//go:build linux || darwin

package disk

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"testing/synctest"
	"time"
)

func TestLocalVersionTentativeWriteDoesNotAnnouncePressure(t *testing.T) {
	for _, trim := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "trim"}[trim], func(t *testing.T) {
			cfg, _ := localVersionFixture(t)
			p, err := CreateLocalVersion(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			// Leave headroom for one new block, but not the two-block request.
			reserved := p.disk.writer.Sink.(*reservedVersionSink)
			reserved.remaining = p.disk.budget.Bytes([]uint64{1})
			data := bytes.Repeat([]byte{9}, 8192)
			if trim {
				data = nil
			}
			if _, err := p.write(t.Context(), data, 4096, 8192, false); !errors.Is(err, ErrVersionStagingFull) {
				t.Fatalf("tentative admission: %v", err)
			}
			changes := p.Changes()
			if changes.Generation != 0 || !changes.PressureSince.IsZero() {
				t.Fatalf("tentative rejection changed state: %+v", changes)
			}
			select {
			case <-changes.PressureWake:
				t.Fatal("tentative rejection announced pressure")
			default:
			}
			var first []byte
			if !trim {
				first = data[:4096]
			}
			if _, err := p.write(t.Context(), first, 4096, 4096, true); err != nil {
				t.Fatalf("first block: %v", err)
			}
			if !p.Changes().PressureSince.IsZero() {
				t.Fatal("admitted block announced pressure")
			}
			if _, err := p.write(t.Context(), first, 8192, 4096, true); !errors.Is(err, ErrVersionStagingFull) {
				t.Fatalf("real pressure: %v", err)
			}
			if p.Changes().PressureSince.IsZero() {
				t.Fatal("rejected fallback block did not announce pressure")
			}
			select {
			case <-changes.PressureWake:
			default:
				t.Fatal("real pressure did not wake its owner")
			}
		})
	}
}

func TestLocalVersionPressureProgressAndCancellation(t *testing.T) {
	for _, cancelWrite := range []bool{false, true} {
		t.Run(map[bool]string{false: "recover-after-five-minutes", true: "cancel-after-five-minutes"}[cancelWrite], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg, _ := localVersionFixture(t)
				cfg.StagedBytes = 128 << 10
				publisher := adoptionPublisher(t, cfg)
				p, err := CreateLocalVersion(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer p.Close()
				offset := int64(4096)
				for {
					_, err = p.WriteAt(t.Context(), bytes.Repeat([]byte{7}, 4096), offset)
					if errors.Is(err, ErrVersionStagingFull) {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					if _, err = p.Flush(t.Context()); err != nil {
						t.Fatal(err)
					}
					offset += 4096
					if offset >= cfg.Base.LogicalBytes {
						t.Fatal("fixture failed to reach pressure")
					}
				}
				if p.Changes().PressureSince.IsZero() {
					t.Fatal("no pressure observation")
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				watchCtx, stopWatch := context.WithCancel(t.Context())
				watchDone := make(chan error, 1)
				go func() { watchDone <- p.watchGuestRequests(watchCtx) }()
				defer func() {
					stopWatch()
					if err := <-watchDone; !errors.Is(err, context.Canceled) {
						t.Errorf("pressure killed device watchdog: %v", err)
					}
				}()
				result := make(chan error, 1)
				want := bytes.Repeat([]byte{9}, 8192)
				go func() { _, err := (versionDevice{p}).WriteAt(ctx, want, offset); result <- err }()
				synctest.Wait()
				time.Sleep(5*time.Minute + 11*time.Second)
				select {
				case err := <-result:
					t.Fatalf("stalled write returned before capacity: %v", err)
				default:
				}
				if _, err := os.Stat(cfg.Directory); err != nil {
					t.Fatal("staging removed", err)
				}
				if cancelWrite {
					cancel()
					if err := <-result; !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
					if localByte(t, p) != 1 {
						t.Fatal("cancellation damaged committed data")
					}
					return
				}
				// This pin and export do not touch the blocked request or guest flush.
				pinned, err := p.PinCommitted(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if err = pinned.Publish(t.Context(), publisher); err != nil {
					t.Fatal(err)
				}
				if err = pinned.BackRemotely(t.Context(), 1000); err != nil {
					t.Fatal(err)
				}
				pinned.Release()
				if p.head.Saved != cfg.Base {
					t.Fatal("object retention advanced recovery head")
				}
				removed, err := pinned.Collect(t.Context(), 1000)
				if err != nil || removed == 0 {
					t.Fatalf("pressure collection=%d %v", removed, err)
				}
				if err = <-result; err != nil {
					t.Fatal(err)
				}
				if err = (versionDevice{p}).Flush(t.Context()); err != nil {
					t.Fatal(err)
				}
				actual := make([]byte, len(want))
				if _, err = p.ReadAt(t.Context(), actual, offset); err != nil || !bytes.Equal(actual, want) {
					t.Fatal("unblocked data lost", err)
				}
				if !p.Changes().PressureSince.IsZero() {
					t.Fatal("pressure did not resolve")
				}
				if p.head.Saved != cfg.Base {
					t.Fatal("local flush advanced published recovery head")
				}
				cut, err := p.Capture(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer cut.Release()
				if err = cut.Publish(t.Context(), publisher); err != nil {
					t.Fatal(err)
				}
				if err = cut.Adopt(t.Context()); err != nil {
					t.Fatal(err)
				}
				if p.head.Saved != cut.Root() {
					t.Fatal("covering save not adopted")
				}
			})
		})
	}
}

func TestLocalVersionWatchdogDetectsNonPressureHang(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, _ := localVersionFixture(t)
		p, err := CreateLocalVersion(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		p.beginGuestRequest()
		defer p.endGuestRequest()
		done := make(chan error, 1)
		go func() { done <- p.watchGuestRequests(t.Context()) }()
		time.Sleep(guestRequestHangLimit + time.Second)
		if err := <-done; !errors.Is(err, ErrDeviceHung) {
			t.Fatal(err)
		}
	})
}
