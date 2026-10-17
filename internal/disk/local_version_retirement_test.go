//go:build linux || darwin

package disk

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

func adoptedRetirementCapture(t *testing.T, p *LocalVersion, pub ContinuationPublication) *LocalCapture {
	t.Helper()
	if _, err := p.WriteAt(t.Context(), bytes.Repeat([]byte{8}, 4096), 0); err != nil {
		t.Fatal(err)
	}
	c, err := p.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Release)
	if err = c.Publish(t.Context(), pub); err != nil {
		t.Fatal(err)
	}
	if err = c.Adopt(t.Context()); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLiveRetirementDrainsOldReader(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	pub := adoptionPublisher(t, cfg)
	gate := &pausedWritableSource{RangeSource: cfg.BaseSource, entered: make(chan struct{}), release: make(chan struct{})}
	cfg.BaseSource = gate
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var release sync.Once
	defer release.Do(func() { close(gate.release) })
	p.reads.clear()
	gate.pause.Store(true)
	done := make(chan error, 1)
	data := make([]byte, 4096)
	go func() { _, err := p.ReadAt(t.Context(), data, 0); done <- err }()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not enter remote I/O")
	}
	c := adoptedRetirementCapture(t, p, pub)
	if c.ReadyForRetirement() {
		t.Fatal("retirement passed an old reader")
	}
	release.Do(func() { close(gate.release) })
	if err := <-done; err != nil || !bytes.Equal(data, bytes.Repeat([]byte{1}, 4096)) {
		t.Fatalf("old read lost: %v", err)
	}
	if !c.ReadyForRetirement() {
		t.Fatal("retirement did not progress after reader joined")
	}
	if localByte(t, p) != 8 {
		t.Fatal("current bytes lost")
	}
}

func TestLiveRetirementDefersBusyClosuresAndOlderCaptures(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	pub := adoptionPublisher(t, cfg)
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	old, err := p.PinCommitted(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer old.Release()
	c := adoptedRetirementCapture(t, p, pub)
	if c.ReadyForRetirement() {
		t.Fatal("older pressure capture did not block floor")
	}
	old.Release()
	for _, gate := range []struct {
		name         string
		lock, unlock func()
	}{
		{"publisher-backing", p.publication.RLock, p.publication.RUnlock},
		{"collector", p.collection.Lock, p.collection.Unlock},
		{"encoder", p.commit.Lock, p.commit.Unlock},
		{"reader-partial-write-flush", p.storage.RLock, p.storage.RUnlock},
	} {
		t.Run(gate.name, func(t *testing.T) {
			gate.lock()
			ready := c.ReadyForRetirement()
			gate.unlock()
			if ready {
				t.Fatal("busy closure passed readiness")
			}
			if !c.ReadyForRetirement() {
				t.Fatal("joined closure did not progress")
			}
		})
	}

}

func TestLiveRetirementPreservesCertifiedEvidence(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	pub := &countedContinuation{continuationTestPublication: adoptionPublisher(t, cfg)}
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	old := adoptedRetirementCapture(t, p, pub)
	old.Release()
	if _, err = p.WriteAt(t.Context(), bytes.Repeat([]byte{9}, 4096), 0); err != nil {
		t.Fatal(err)
	}
	c, err := p.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	if err = c.Publish(t.Context(), pub); err != nil {
		t.Fatal(err)
	}
	if err = c.Adopt(t.Context()); err != nil {
		t.Fatal(err)
	}
	beforeBytes, beforeCount := p.evidence.bytes, len(p.evidence.entries)
	if !c.ReadyForRetirement() {
		t.Fatal("healthy adopted capture not ready")
	}
	if p.evidence.bytes != beforeBytes || len(p.evidence.entries) != beforeCount {
		t.Fatal("readiness discarded certification")
	}
	oldLocator, _ := old.Root().Locator(old.Root().LogicalBytes)
	if _, remote, ok := p.evidence.get(oldLocator.Pack.Digest); !remote || !ok {
		t.Fatal("old certified evidence discarded")
	}
	uploads := pub.uploads
	if err = c.Publish(t.Context(), pub); err != nil {
		t.Fatal(err)
	}
	if pub.uploads != uploads {
		t.Fatal("readiness forced redundant PUT")
	}

}

func TestLiveRetirementRestartRequiresFreshAdoption(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	pub := adoptionPublisher(t, cfg)
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := adoptedRetirementCapture(t, p, pub)
	c.Release()
	if !c.ReadyForRetirement() {
		t.Fatal("initial adoption not ready")
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	p, err = OpenLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if localByte(t, p) != 8 {
		t.Fatal("persisted adopted head not reopened")
	}
	c, err = p.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	if c.ReadyForRetirement() {
		t.Fatal("restart inferred announced floor")
	}
	if err = c.Publish(t.Context(), pub); err != nil {
		t.Fatal(err)
	}
	if err = c.Adopt(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !c.ReadyForRetirement() {
		t.Fatal("fresh adoption after restart not ready")
	}
}

type retirementBlockedPublisher struct {
	ContinuationPublication
	entered, release chan struct{}
	once             sync.Once
}

func (p *retirementBlockedPublisher) RegisterBatch(ctx context.Context, i []blockformat.ObjectInspection) error {
	p.once.Do(func() { close(p.entered); <-p.release })
	return p.ContinuationPublication.RegisterBatch(ctx, i)
}

func TestLiveRetirementJoinsActivePublisher(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	pub := adoptionPublisher(t, cfg)
	gate := &pausedWritableSource{RangeSource: cfg.BaseSource, entered: make(chan struct{}), release: make(chan struct{})}
	cfg.BaseSource = gate
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	c := adoptedRetirementCapture(t, p, pub)
	if _, err = p.WriteAt(t.Context(), bytes.Repeat([]byte{9}, 4096), 0); err != nil {
		t.Fatal(err)
	}
	pending, err := p.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Release()
	publisher := &retirementBlockedPublisher{ContinuationPublication: pub, entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(publisher.release) })
	done := make(chan error, 1)
	go func() { done <- pending.Publish(t.Context(), publisher) }()
	select {
	case <-publisher.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("publisher did not register")
	}
	if c.ReadyForRetirement() {
		t.Fatal("active publisher passed retirement gate")
	}
	release.Do(func() { close(publisher.release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !c.ReadyForRetirement() {
		t.Fatal("joined publisher prevented progress")
	}

}

func TestLiveRetirementJoinsBackRemotelyAndCapture(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	pub := adoptionPublisher(t, cfg)
	gate := &pausedWritableSource{RangeSource: cfg.BaseSource, entered: make(chan struct{}), release: make(chan struct{})}
	cfg.BaseSource = gate
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	c := adoptedRetirementCapture(t, p, pub)
	if _, err = p.WriteAt(t.Context(), bytes.Repeat([]byte{9}, 4096), 0); err != nil {
		t.Fatal(err)
	}
	entered, releaseCapture := make(chan struct{}), make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(releaseCapture) })
	p.phase = func(phase string) error {
		if phase == "root-synced" {
			close(entered)
			<-releaseCapture
		}
		return nil
	}
	captures := make(chan *LocalCapture, 1)
	done := make(chan error, 1)
	go func() { capture, err := p.Capture(t.Context()); captures <- capture; done <- err }()
	<-entered
	if c.ReadyForRetirement() {
		t.Fatal("in-flight capture passed retirement gate")
	}
	release.Do(func() { close(releaseCapture) })
	pending := <-captures
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	defer pending.Release()
	p.phase = nil
	if err = pending.Publish(t.Context(), pub); err != nil {
		t.Fatal(err)
	}
	// Drop only test evidence to force an actual backing traversal. A production
	// bounded-cache miss follows this same path; custody remains with the capture.
	p.evidence = newVersionEvidenceCache()
	var releaseBacking sync.Once
	defer releaseBacking.Do(func() { close(gate.release) })
	gate.pause.Store(true)
	go func() { done <- pending.BackRemotely(t.Context(), 1000) }()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("backing did not read remote closure")
	}
	if c.ReadyForRetirement() {
		t.Fatal("active backing traversal passed retirement gate")
	}
	releaseBacking.Do(func() { close(gate.release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Refresh the candidate's retained positive sample after the deliberate miss.
	if err = c.Adopt(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !c.ReadyForRetirement() {
		t.Fatal("joined backing/capture prevented progress")
	}
}
