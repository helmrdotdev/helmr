//go:build linux || darwin

package nbd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestReadyReplyCustody(t *testing.T) {
	for _, tc := range []struct {
		name        string
		rights      int
		helperError string
		invalid     bool
	}{
		{"owned descriptor", 1, "", false},
		{"no claim", 0, "devices busy", false},
		{"missing descriptor", 0, "", true},
		{"extra descriptors", 2, "", true},
		{"many extra descriptors", 200, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, peer := controlPair(t)
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			done, result := make(chan struct{}), make(chan error, 1)
			go func() {
				defer close(done)
				dec := json.NewDecoder(peer)
				var ready request
				if err := dec.Decode(&ready); err != nil {
					result <- err
					return
				}
				var fds []int
				for range tc.rights {
					fds = append(fds, int(writer.Fd()))
				}
				var rights []byte
				if len(fds) > 0 {
					rights = unix.UnixRights(fds...)
				}
				if _, _, err := peer.WriteMsgUnix([]byte{0}, rights, nil); err != nil {
					result <- err
					return
				}
				writer.Close()
				if err := json.NewEncoder(peer).Encode(response{ID: ready.ID, Device: "/dev/nbd999999", Error: tc.helperError}); err != nil {
					result <- err
					return
				}
				if tc.invalid {
					result <- nil
					return
				}
				var release request
				if err := dec.Decode(&release); err != nil {
					result <- err
					return
				}
				result <- json.NewEncoder(peer).Encode(response{ID: release.ID})
			}()
			a := &Attachment{conn: parent, decoder: json.NewDecoder(parent), done: done}
			_, err = a.call(t.Context(), request{Op: "ready"})
			if (err != nil) != (tc.invalid || tc.helperError != "") {
				t.Fatalf("ready: %v", err)
			}
			owned := a.owned
			if tc.invalid {
				for range 2 {
					if a.Release(t.Context()) == nil || a.released {
						t.Fatal("invalid descriptor response did not quarantine transport")
					}
				}
			} else {
				if a.ready != (tc.rights == 1) {
					t.Fatal("readiness did not match descriptor evidence")
				}
				if err := a.Release(t.Context()); err != nil {
					t.Fatal(err)
				}
				if a.owned != nil {
					t.Fatal("exclusive descriptor retained after proven release")
				}
				if owned != nil {
					if _, err := owned.Stat(); !errors.Is(err, os.ErrClosed) {
						t.Fatalf("descriptor not closed: %v", err)
					}
				}
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			if err := reader.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			var b [1]byte
			if _, err := reader.Read(b[:]); err != io.EOF {
				t.Fatalf("received descriptor leaked: %v", err)
			}
		})
	}
}

func TestReleaseRetryReceivesLateAcknowledgement(t *testing.T) {
	parent, peer := controlPair(t)
	received, replyAllowed, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		defer close(done)
		var release request
		if err := json.NewDecoder(peer).Decode(&release); err != nil {
			result <- err
			return
		}
		close(received)
		<-replyAllowed
		result <- json.NewEncoder(peer).Encode(response{ID: release.ID})
		peer.Close()
	}()
	a := &Attachment{conn: parent, decoder: json.NewDecoder(parent), done: done}
	ctx, cancel := context.WithCancel(t.Context())
	first := make(chan error, 1)
	go func() { first <- a.Release(ctx) }()
	<-received
	cancel()
	if err := <-first; err == nil {
		t.Fatal("release accepted without acknowledgement")
	}
	close(replyAllowed)
	<-done
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if err := a.Release(t.Context()); err != nil {
		t.Fatalf("resume exact outstanding release: %v", err)
	}
}

type partialControlWrite struct {
	net.Conn
	writes int
}

func (c *partialControlWrite) Write(p []byte) (int, error) { c.writes++; return 1, io.ErrShortWrite }

func TestPartialControlWriteRemainsQuarantined(t *testing.T) {
	parent, _ := controlPair(t)
	conn := &partialControlWrite{Conn: parent}
	a := &Attachment{conn: conn, decoder: json.NewDecoder(conn)}
	if _, err := a.call(t.Context(), request{Op: "flush"}); err == nil {
		t.Fatal("partial write accepted")
	}
	if err := a.Release(t.Context()); err == nil {
		t.Fatal("partial request replayed")
	}
	if conn.writes != 1 {
		t.Fatal("appended to an uncertain partial request")
	}
}

type firstWriteTimeout struct {
	net.Conn
	attempted bool
}

func (c *firstWriteTimeout) Write(p []byte) (int, error) {
	if !c.attempted {
		c.attempted = true
		return 0, os.ErrDeadlineExceeded
	}
	return c.Conn.Write(p)
}

func TestReleaseAfterZeroByteWriteTimeout(t *testing.T) {
	parent, peer := controlPair(t)
	done, result := make(chan struct{}), make(chan error, 1)
	go func() {
		defer close(done)
		var r request
		if err := json.NewDecoder(peer).Decode(&r); err != nil {
			result <- err
			return
		}
		if r.Op != "release" {
			result <- errors.New("timed-out request reached helper")
			return
		}
		result <- json.NewEncoder(peer).Encode(response{ID: r.ID})
	}()
	a := &Attachment{conn: &firstWriteTimeout{Conn: parent}, decoder: json.NewDecoder(parent), done: done, ready: true, device: "/dev/nbd999999"}
	if err := a.Flush(t.Context()); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("expected zero-byte timeout: %v", err)
	}
	if err := a.Release(t.Context()); err != nil {
		t.Fatalf("release after unsent request: %v", err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestReleaseAfterCancelledFlushReply(t *testing.T) {
	parent, peer := controlPair(t)
	read, replyAllowed, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		defer close(done)
		dec := json.NewDecoder(peer)
		var flush request
		if err := dec.Decode(&flush); err != nil {
			result <- err
			return
		}
		body, _ := json.Marshal(response{ID: flush.ID})
		if _, err := peer.Write(body[:5]); err != nil {
			result <- err
			return
		}
		<-replyAllowed
		if _, err := peer.Write(append(body[5:], '\n')); err != nil {
			result <- err
			return
		}
		var release request
		if err := dec.Decode(&release); err != nil {
			result <- err
			return
		}
		result <- json.NewEncoder(peer).Encode(response{ID: release.ID})
	}()
	a := &Attachment{conn: parent, decoder: json.NewDecoder(&observedRead{Reader: parent, read: read}), done: done, ready: true, device: "/dev/nbd999999"}
	ctx, cancel := context.WithCancel(t.Context())
	flushed := make(chan error, 1)
	go func() { flushed <- a.Flush(ctx) }()
	<-read
	cancel()
	if err := <-flushed; err == nil {
		t.Fatal("flush accepted partial reply")
	}
	close(replyAllowed)
	if err := a.Release(t.Context()); err != nil {
		t.Fatalf("release after stale partial flush: %v", err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestClaimEOFBeforeJSONStaysQuarantined(t *testing.T) {
	parent, peer := controlPair(t)
	done, result := make(chan struct{}), make(chan error, 1)
	go func() {
		defer close(done)
		defer peer.Close()
		var ready request
		if err := json.NewDecoder(peer).Decode(&ready); err != nil {
			result <- err
			return
		}
		_, _, err := peer.WriteMsgUnix([]byte{0}, nil, nil)
		result <- err
	}()
	a := &Attachment{conn: parent, decoder: json.NewDecoder(parent), done: done}
	if _, err := a.call(t.Context(), request{Op: "ready"}); err == nil {
		t.Fatal("claim accepted missing reply")
	}
	for range 2 {
		if a.Release(t.Context()) == nil || a.released {
			t.Fatal("helper EOF did not quarantine attachment")
		}
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
