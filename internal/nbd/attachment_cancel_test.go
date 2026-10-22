//go:build linux || darwin

package nbd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func controlPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	connections := make([]*net.UnixConn, 2)
	for i, fd := range pair {
		file := os.NewFile(uintptr(fd), "control")
		conn, err := net.FileConn(file)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		connections[i] = conn.(*net.UnixConn)
		t.Cleanup(func() { conn.Close() })
	}
	return connections[0], connections[1]
}

func TestReleaseAfterCancelledClaimDescriptorReceive(t *testing.T) {
	parent, peer := controlPair(t)
	claimReceived, replyAllowed, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	helperResult := make(chan error, 1)
	owned, err := os.CreateTemp(t.TempDir(), "claim")
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Close()
	go func() {
		defer close(done)
		dec := json.NewDecoder(peer)
		var ready request
		if err := dec.Decode(&ready); err != nil {
			helperResult <- err
			return
		}
		close(claimReceived)
		<-replyAllowed
		if _, _, err := peer.WriteMsgUnix([]byte{0}, unix.UnixRights(int(owned.Fd())), nil); err != nil {
			helperResult <- err
			return
		}
		if err := json.NewEncoder(peer).Encode(response{ID: ready.ID, Device: "/dev/nbd999999"}); err != nil {
			helperResult <- err
			return
		}
		var release request
		if err := dec.Decode(&release); err != nil {
			helperResult <- err
			return
		}
		helperResult <- json.NewEncoder(peer).Encode(response{ID: release.ID})
	}()
	a := &Attachment{conn: parent, decoder: json.NewDecoder(parent), done: done}
	ctx, cancel := context.WithCancel(t.Context())
	claimed := make(chan error, 1)
	go func() { _, err := a.call(ctx, request{Op: "ready"}); claimed <- err }()
	<-claimReceived
	cancel()
	if err := <-claimed; err == nil {
		t.Fatal("claim succeeded before its descriptor response")
	}
	close(replyAllowed)
	cleanup, stop := context.WithTimeout(t.Context(), 2*time.Second)
	defer stop()
	if err := a.Release(cleanup); err != nil {
		t.Fatalf("release after cancelled claim: %v", err)
	}
	if err := <-helperResult; err != nil {
		t.Fatal(err)
	}
	if !a.released {
		t.Fatal("helper release and exit were not accepted")
	}
	if !a.ready || a.device != "/dev/nbd999999" {
		t.Fatal("resumed readiness did not restore device inactivity evidence")
	}
	if a.owned != nil {
		t.Fatal("owned descriptor retained after release")
	}
}

type observedRead struct {
	io.Reader
	read chan struct{}
}

func (r *observedRead) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 && r.read != nil {
		close(r.read)
		r.read = nil
	}
	return n, err
}

func TestReleaseAfterCancelledPartialClaimJSON(t *testing.T) {
	parent, peer := controlPair(t)
	read, replyAllowed, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	helperResult := make(chan error, 1)
	owned, err := os.CreateTemp(t.TempDir(), "claim")
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Close()
	go func() {
		defer close(done)
		dec := json.NewDecoder(peer)
		var ready request
		if err := dec.Decode(&ready); err != nil {
			helperResult <- err
			return
		}
		if _, _, err := peer.WriteMsgUnix([]byte{0}, unix.UnixRights(int(owned.Fd())), nil); err != nil {
			helperResult <- err
			return
		}
		body, _ := json.Marshal(response{ID: ready.ID, Device: "/dev/nbd999999"})
		if _, err := peer.Write(body[:5]); err != nil {
			helperResult <- err
			return
		}
		<-replyAllowed
		if _, err := peer.Write(append(body[5:], '\n')); err != nil {
			helperResult <- err
			return
		}
		var release request
		if err := dec.Decode(&release); err != nil {
			helperResult <- err
			return
		}
		helperResult <- json.NewEncoder(peer).Encode(response{ID: release.ID})
	}()
	a := &Attachment{conn: parent, decoder: json.NewDecoder(&observedRead{Reader: parent, read: read}), done: done}
	ctx, cancel := context.WithCancel(t.Context())
	claimed := make(chan error, 1)
	go func() { _, err := a.call(ctx, request{Op: "ready"}); claimed <- err }()
	<-read
	cancel()
	if err := <-claimed; err == nil {
		t.Fatal("claim accepted incomplete JSON")
	}
	if a.owned == nil {
		t.Fatal("descriptor was not received before cancellation")
	}
	receivedFile := a.owned
	close(replyAllowed)
	cleanup, stop := context.WithTimeout(t.Context(), 2*time.Second)
	defer stop()
	if err := a.Release(cleanup); err != nil {
		t.Fatalf("release after partial claim JSON: %v", err)
	}
	if err := <-helperResult; err != nil {
		t.Fatal(err)
	}
	if !a.released {
		t.Fatal("helper release and exit were not accepted")
	}
	if !a.ready || a.device != "/dev/nbd999999" {
		t.Fatal("resumed readiness did not restore device inactivity evidence")
	}
	if a.owned != nil {
		t.Fatal("owned descriptor retained after release")
	}
	if _, err := receivedFile.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("received descriptor not closed: %v", err)
	}
}

func TestReleaseRetryAfterAcknowledgedHelperExitTimeout(t *testing.T) {
	parent, peer := controlPair(t)
	done := make(chan struct{})
	a := &Attachment{conn: parent, decoder: json.NewDecoder(parent), done: done}
	helperResult := make(chan error, 1)
	go func() {
		var release request
		if err := json.NewDecoder(peer).Decode(&release); err != nil {
			helperResult <- err
			return
		}
		helperResult <- json.NewEncoder(peer).Encode(response{ID: release.ID})
	}()
	first, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := a.Release(first); err == nil || !strings.Contains(err.Error(), "helper exit unproven") {
		t.Fatalf("expected accepted release with unproved helper exit, got %v", err)
	}
	if err := <-helperResult; err != nil {
		t.Fatal(err)
	}
	peer.Close()
	close(done)
	if err := a.Release(t.Context()); err != nil {
		t.Fatalf("resume terminal wait: %v", err)
	}
	if !a.released {
		t.Fatal("completed release not recorded")
	}
}
