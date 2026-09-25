//go:build linux || darwin

package nbd

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaimNeverOverwritesEvidence(t *testing.T) {
	arena := t.TempDir()
	if err := os.Chmod(arena, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(arena, "config.json")
	want := []byte("prior ownership")
	if err := os.WriteFile(path, want, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Claim(t.Context(), Config{Helper: "unused", Socket: filepath.Join(arena, "backend.sock"), Arena: arena, Devices: []string{"/dev/nbd15"}, Size: 4096})
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("got %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(want) {
		t.Fatal("ownership evidence changed")
	}
}
func TestUnprovenConsumerDoesNotDisconnectOrPermitReuse(t *testing.T) {
	parent, peer := net.Pipe()
	defer parent.Close()
	defer peer.Close()
	a := &Attachment{conn: parent, decoder: json.NewDecoder(parent), consumerExit: make(chan struct{})}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := a.Release(ctx); err == nil {
		t.Fatal("unproven consumer accepted")
	}
	if a.released {
		t.Fatal("marked released")
	}
	if err := a.BindConsumer(make(chan struct{})); err == nil {
		t.Fatal("second consumer accepted")
	}
	peer.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	var b [1]byte
	if _, err := peer.Read(b[:]); err == nil {
		t.Fatal("issued disconnect despite consumer uncertainty")
	}
}
func TestReleaseCanRetryAfterUnprovenConsumer(t *testing.T) {
	parent, peer := net.Pipe()
	defer peer.Close()
	done := make(chan struct{})
	consumerExit := make(chan struct{})
	a := &Attachment{conn: parent, decoder: json.NewDecoder(parent), done: done, consumerExit: consumerExit}
	short, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := a.Release(short); err == nil {
		t.Fatal("expected stop failure")
	}
	close(consumerExit)
	go func() {
		var r request
		_ = json.NewDecoder(peer).Decode(&r)
		_ = json.NewEncoder(peer).Encode(response{ID: r.ID})
		close(done)
	}()
	if err := a.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !a.released {
		t.Fatal("successful retry not released")
	}
	if err := a.Flush(t.Context()); err == nil {
		t.Fatal("flush after release accepted")
	}
}
func TestConsumerBindingIsExclusiveAndReleaseDoesNotSignalExit(t *testing.T) {
	exited := make(chan struct{})
	a := &Attachment{ready: true}
	if err := a.BindConsumer(nil); err == nil {
		t.Fatal("nil proof accepted")
	}
	if err := a.BindConsumer(exited); err != nil {
		t.Fatal(err)
	}
	if err := a.BindConsumer(make(chan struct{})); err == nil {
		t.Fatal("consumer replaced")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !errors.Is(a.Release(ctx), context.Canceled) {
		t.Fatal("cancel ignored")
	}
	select {
	case <-exited:
		t.Fatal("release fabricated exit proof")
	default:
	}
}

func TestPreCancelledClaimCreatesNothing(t *testing.T) {
	arena := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Claim(ctx, Config{Arena: arena})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(arena)
	if len(entries) != 0 {
		t.Fatal("cancelled claim wrote evidence")
	}
}

func TestAmbiguousReleaseRetainsAttachment(t *testing.T) {
	parent, peer := net.Pipe()
	defer parent.Close()
	defer peer.Close()
	a := &Attachment{ready: true, conn: parent, decoder: json.NewDecoder(parent), done: make(chan struct{})}
	go func() {
		var r request
		_ = json.NewDecoder(peer).Decode(&r)
		_ = json.NewEncoder(peer).Encode(response{ID: r.ID, Error: "driver exit unproven"})
	}()
	if err := a.Release(t.Context()); err == nil {
		t.Fatal("uncertain cleanup accepted")
	}
	if a.released {
		t.Fatal("uncertain attachment released")
	}
	if err := a.BindConsumer(make(chan struct{})); err == nil {
		t.Fatal("uncertain attachment reused")
	}
}
