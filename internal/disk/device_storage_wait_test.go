//go:build linux || darwin

package disk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

type storageWaitGate struct {
	active           atomic.Bool
	once             sync.Once
	entered, release chan struct{}
}

func newStorageWaitGate() *storageWaitGate {
	return &storageWaitGate{entered: make(chan struct{}), release: make(chan struct{})}
}
func (g *storageWaitGate) wait(ctx context.Context) error {
	if !g.active.Load() {
		return nil
	}
	var blocked bool
	g.once.Do(func() { blocked = true; close(g.entered) })
	if !blocked {
		return nil
	}
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type storageWaitSource struct {
	source blockformat.RangeSource
	gate   *storageWaitGate
	stage  string
}

func (s storageWaitSource) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	if s.stage == "acquire" {
		if err := s.gate.wait(ctx); err != nil {
			return nil, err
		}
	}
	body, err := s.source.GetRange(ctx, digest, size, offset, length)
	if body == nil {
		return body, err
	}
	return storageWaitBody{ctx, body, s.gate, s.stage}, err
}

type storageWaitBody struct {
	ctx context.Context
	io.ReadCloser
	gate  *storageWaitGate
	stage string
}

func (b storageWaitBody) Read(p []byte) (int, error) {
	if b.stage == "read" {
		if err := b.gate.wait(b.ctx); err != nil {
			return 0, err
		}
	}
	return b.ReadCloser.Read(p)
}
func (b storageWaitBody) Close() error {
	if b.stage == "close" {
		if err := b.gate.wait(b.ctx); err != nil {
			return errors.Join(err, b.ReadCloser.Close())
		}
	}
	return b.ReadCloser.Close()
}

func TestDeviceStorageWaitColdReadSurvivesHangLimit(t *testing.T) {
	for _, stage := range []string{"acquire", "read", "close"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg, _ := localVersionFixture(t)
				gate := newStorageWaitGate()
				cfg.BaseSource = storageWaitSource{cfg.BaseSource, gate, stage}
				p, err := CreateLocalVersion(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer p.Close()
				p.reads.clear()
				gate.active.Store(true)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				watch := make(chan error, 1)
				go func() { watch <- p.watchGuestRequests(ctx) }()
				read := make(chan error, 1)
				go func() {
					b := make([]byte, 1)
					_, err := (versionDevice{p}).ReadAt(ctx, b, 0)
					if err == nil && b[0] != 1 {
						err = errors.New("cold bytes changed")
					}
					read <- err
				}()
				<-gate.entered
				time.Sleep(2 * guestRequestHangLimit)
				select {
				case err := <-watch:
					t.Fatal("storage killed watchdog", err)
				default:
				}
				close(gate.release)
				if err := <-read; err != nil {
					t.Fatal(err)
				}
				if p.guestRequest.Load() != nil {
					t.Fatal("completed request stayed active")
				}
				cancel()
				if err := <-watch; !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestDeviceStorageWaitBackgroundReadCannotMaskHungRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, _ := localVersionFixture(t)
		p, err := CreateLocalVersion(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		gate := newStorageWaitGate()
		gate.active.Store(true)
		source := guestIORangeSource{storageWaitSource{cacheRangeSource(func(context.Context, string, int64, int64, int64) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader([]byte{1})), nil
		}), gate, "acquire"}}
		background := make(chan error, 1)
		go func() {
			body, err := source.GetRange(t.Context(), "object", 1, 0, 1)
			if body != nil {
				err = errors.Join(err, body.Close())
			}
			background <- err
		}()
		<-gate.entered
		p.beginGuestRequest()
		defer p.endGuestRequest()
		watch := make(chan error, 1)
		go func() { watch <- p.watchGuestRequests(t.Context()) }()
		if err := <-watch; !errors.Is(err, ErrDeviceHung) {
			t.Fatal(err)
		}
		close(gate.release)
		if err := <-background; err != nil {
			t.Fatal(err)
		}
	})
}

func TestDeviceStorageWaitBodyLifetimeDoesNotHideParserHang(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, _ := localVersionFixture(t)
		p, err := CreateLocalVersion(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		p.beginGuestRequest()
		defer p.endGuestRequest()
		ctx := p.guestRequestContext(t.Context())
		source := guestIORangeSource{cacheRangeSource(func(context.Context, string, int64, int64, int64) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader([]byte{1})), nil
		})}
		body, err := source.GetRange(ctx, "object", 1, 0, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer body.Close()
		if _, err = io.Copy(io.Discard, body); err != nil {
			t.Fatal(err)
		}
		if err = p.watchGuestRequests(t.Context()); !errors.Is(err, ErrDeviceHung) {
			t.Fatal(err)
		}
	})
}

func TestDeviceStorageWaitPreservesCancellationAndErrors(t *testing.T) {
	for _, failure := range []error{context.Canceled, io.ErrUnexpectedEOF} {
		cfg, _ := localVersionFixture(t)
		p, err := CreateLocalVersion(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		p.beginGuestRequest()
		ctx := p.guestRequestContext(t.Context())
		source := guestIORangeSource{cacheRangeSource(func(context.Context, string, int64, int64, int64) (io.ReadCloser, error) { return nil, failure })}
		if _, err = source.GetRange(ctx, "object", 1, 0, 1); err != failure {
			t.Fatal("storage error changed", err)
		}
		if p.guestRequest.Load().state.Load().waits != 0 {
			t.Fatal("error left request suspended")
		}
		p.endGuestRequest()
		if err = p.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeviceStorageWaitPersistenceCallbackDoesNotMaskHang(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, _ := localVersionFixture(t)
		p, err := CreateLocalVersion(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		if _, err = p.WriteAt(t.Context(), []byte{9}, 0); err != nil {
			t.Fatal(err)
		}
		gate := newStorageWaitGate()
		gate.active.Store(true)
		p.phase = func(phase string) error {
			if phase == "root-synced" {
				return gate.wait(t.Context())
			}
			return nil
		}
		flushed := make(chan error, 1)
		go func() { flushed <- (versionDevice{p}).Flush(t.Context()) }()
		<-gate.entered
		if err = p.watchGuestRequests(t.Context()); !errors.Is(err, ErrDeviceHung) {
			t.Fatal("callback incorrectly marked as native IO", err)
		}
		close(gate.release)
		if err = <-flushed; err != nil {
			t.Fatal(err)
		}
	})
}

func TestDeviceStorageWaitCompletionResetsOnlySameRequest(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "same", true: "replaced"}[replace], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg, _ := localVersionFixture(t)
				p, err := CreateLocalVersion(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer p.Close()
				p.beginGuestRequest()
				ctx := p.guestRequestContext(t.Context())
				wait := cas.BeginGuestIOWait(ctx)
				if replace {
					p.endGuestRequest()
					p.beginGuestRequest()
				}
				defer p.endGuestRequest()
				watch := make(chan error, 1)
				go func() { watch <- p.watchGuestRequests(t.Context()) }()
				time.Sleep(9 * time.Second)
				wait.End()
				if replace {
					if err := <-watch; !errors.Is(err, ErrDeviceHung) {
						t.Fatal(err)
					}
					return
				}
				time.Sleep(9 * time.Second)
				select {
				case err := <-watch:
					t.Fatal("dependency completion did not reset request", err)
				default:
				}
				if err := <-watch; !errors.Is(err, ErrDeviceHung) {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestDeviceStorageWaitFlushBehindCollectionSurvivesHangLimit(t *testing.T) {

	cfg, _ := localVersionFixture(t)
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	raw := []byte("unreachable immutable object")
	if err = p.store.StoreObject(t.Context(), sha256.Sum256(raw), raw); err != nil {
		t.Fatal(err)
	}
	gate := newStorageWaitGate()
	gate.active.Store(true)
	defer func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	}()
	p.phase = func(phase string) error {
		if phase == "objects-unlinked" {
			return gate.wait(t.Context())
		}
		return nil
	}
	collected := make(chan error, 1)
	go func() { _, err := p.Collect(t.Context(), 1000); collected <- err }()
	<-gate.entered
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	watch := make(chan error, 1)
	go func() { watch <- p.watchGuestRequests(ctx) }()
	flushed := make(chan error, 1)
	go func() { flushed <- (versionDevice{p}).Flush(ctx) }()
	// Mutex blocking is not virtual-time durable in synctest. Hold the real
	// collection lock across the actual watchdog deadline.
	time.Sleep(guestRequestHangLimit + time.Second)
	select {
	case err := <-watch:
		t.Fatal("collection wait killed watchdog", err)
	default:
	}
	close(gate.release)
	if err := <-collected; err != nil {
		t.Fatal(err)
	}
	if err := <-flushed; err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-watch; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
