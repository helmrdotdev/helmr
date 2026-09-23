//go:build linux || darwin

package nbd

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
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
	a := &Attachment{conn: parent, decoder: json.NewDecoder(parent), consumer: reapedChild(t), consumerDone: make(chan struct{})}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := a.Release(ctx); err == nil {
		t.Fatal("unproven consumer accepted")
	}
	if a.released {
		t.Fatal("marked released")
	}
	if err := a.StartConsumer(exec.Command("false")); err == nil {
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
	a := &Attachment{conn: parent, decoder: json.NewDecoder(parent), done: done, consumer: reapedChild(t), consumerDone: make(chan struct{})}
	short, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := a.Release(short); err == nil {
		t.Fatal("expected stop failure")
	}
	close(a.consumerDone)
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
func TestCancelledReleaseDoesNotKillConsumer(t *testing.T) {
	cmd := exec.Command("sleep", "10")
	a := &Attachment{ready: true}
	if err := a.StartConsumer(cmd); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); <-a.consumerDone }()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !errors.Is(a.Release(ctx), context.Canceled) {
		t.Fatal("cancel ignored")
	}
	select {
	case <-a.consumerDone:
		t.Fatal("cancelled release killed consumer")
	default:
	}
}

func reapedChild(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd
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
	if err := a.StartConsumer(exec.Command("true")); err == nil {
		t.Fatal("uncertain attachment reused")
	}
}
