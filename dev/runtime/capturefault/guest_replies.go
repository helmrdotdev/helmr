package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

type guestRelay struct {
	mu                sync.Mutex
	path, saved       string
	original, proxy   os.FileInfo
	listener          *net.UnixListener
	restored          bool
	unlinked          bool
	active, forwarded atomic.Int64
	streamErrors      atomic.Int64
	fault             *replyFault
}

func installGuestRelay(path string, fault *replyFault) (_ *guestRelay, err error) {
	original, err := os.Lstat(path)
	if err != nil || original.Mode()&os.ModeSocket == 0 {
		return nil, errors.New("source vsock endpoint is not a socket")
	}
	saved := path + ".reply-upstream"
	if _, err := os.Lstat(saved); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("source relay sibling already exists")
	}
	if err := os.Rename(path, saved); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			// Never overwrite another owner's newly created endpoint.
			if _, missing := os.Lstat(path); errors.Is(missing, os.ErrNotExist) {
				err = errors.Join(err, os.Rename(saved, path))
			}
		}
	}()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	proxy, err := os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	r := &guestRelay{path: path, saved: saved, original: original, proxy: proxy, listener: listener, fault: fault}
	stat, ok := original.Sys().(*syscall.Stat_t)
	if !ok {
		err = errors.New("socket ownership unavailable")
	} else if err = os.Chown(path, int(stat.Uid), int(stat.Gid)); err == nil {
		err = os.Chmod(path, original.Mode().Perm())
	}
	if err != nil {
		err = errors.Join(err, r.restore())
		return nil, err
	}
	return r, nil
}

// Restoring the listener name does not close accepted Program streams. They
// drain naturally through subsequent capture/source reclamation.
func (r *guestRelay) restore() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.restored {
		return nil
	}
	current, err := os.Lstat(r.path)
	if (!r.unlinked && (err != nil || !os.SameFile(current, r.proxy))) || (r.unlinked && !errors.Is(err, os.ErrNotExist)) {
		return errors.New("source relay pathname changed; not restoring")
	}
	saved, err := os.Lstat(r.saved)
	if err != nil || !os.SameFile(saved, r.original) {
		return errors.New("original source socket changed; not restoring")
	}
	if err := r.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	if !r.unlinked {
		if err := os.Remove(r.path); err != nil {
			return err
		}
		r.unlinked = true
	}
	if err := os.Rename(r.saved, r.path); err != nil {
		return err
	}
	r.restored = true
	return nil
}

func (r *guestRelay) status() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return map[string]any{"restored": r.restored, "active_streams": r.active.Load(), "forwarded_streams": r.forwarded.Load(), "stream_errors": r.streamErrors.Load()}
}

func (r *guestRelay) serve() {
	for {
		conn, err := r.listener.AcceptUnix()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			r.fault.fail(errors.New("source relay accept failed"))
			return
		}
		r.active.Add(1)
		go func() {
			defer r.active.Add(-1)
			defer conn.Close()
			if err := r.forward(conn); err != nil && !errors.Is(err, io.EOF) {
				r.streamErrors.Add(1)
			}
		}()
	}
}

func line(reader *bufio.Reader) ([]byte, error) {
	value, err := reader.ReadSlice('\n')
	if len(value) > 64 {
		return nil, errors.New("vsock handshake exceeds bound")
	}
	return value, err
}

func (r *guestRelay) forward(client *net.UnixConn) error {
	r.mu.Lock()
	path := r.saved
	if r.restored {
		path = r.path
	}
	r.mu.Unlock()
	upstream, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return errors.New("source relay upstream unavailable")
	}
	defer upstream.Close()
	_ = client.SetDeadline(time.Now().Add(30 * time.Second))
	_ = upstream.SetDeadline(time.Now().Add(30 * time.Second))
	in, out := bufio.NewReader(client), bufio.NewReader(upstream)
	connect, err := line(in)
	if err != nil {
		return err
	}
	if _, err = upstream.Write(connect); err != nil {
		return err
	}
	ack, err := line(out)
	if err != nil {
		return err
	}
	if _, err = client.Write(ack); err != nil {
		return err
	}
	_ = client.SetDeadline(time.Time{})
	_ = upstream.SetDeadline(time.Time{})
	if string(connect) != "CONNECT 5000\n" {
		return relayStreams(client, upstream, in, out)
	}
	var prefix bytes.Buffer
	headerBytes, bodySize, err := frameio.ReadStreamFrameHeaderBounded(io.TeeReader(in, &prefix), 65536, ^uint64(0))
	if err != nil {
		return err
	}
	var header wire.StreamHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return err
	}
	if _, err := upstream.Write(prefix.Bytes()); err != nil {
		return err
	}
	r.forwarded.Add(1)
	if header.Type != wire.StreamTypeComputerCaptureAbort || bodySize != 0 {
		return relayStreams(client, upstream, in, out)
	}
	var requestBytes bytes.Buffer
	var request computerv0.ComputerCaptureAbortRequest
	if err := frameio.ReadProtoFrameBounded(io.TeeReader(in, &requestBytes), 1<<20, &request); err != nil {
		return err
	}
	if _, err := upstream.Write(requestBytes.Bytes()); err != nil {
		return err
	}
	var responseBytes bytes.Buffer
	var response computerv0.ComputerCaptureAbortResponse
	if err := frameio.ReadProtoFrameBounded(io.TeeReader(out, &responseBytes), 1<<20, &response); err != nil {
		r.fault.guestReadError(&request)
		return err
	}
	if r.fault.guestResponse(&request, &response) {
		return nil
	}
	_, err = client.Write(responseBytes.Bytes())
	return err
}

func (f *replyFault) guestReadError(request *computerv0.ComputerCaptureAbortRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.target == nil || f.target.Mode != "drop" || request.GetCapture().GetCheckpointId() != f.target.CheckpointID || request.GetCapture().GetComputerInstanceId() != f.target.InstanceID {
		return
	}
	if len(f.events) >= 100 {
		f.failure = "reply fault exceeded bounded case observations"
		return
	}
	kind := "guest-prepare"
	if request.Activate {
		kind = "guest-activate"
	}
	f.events = append(f.events, replyEvent{Kind: kind, At: time.Now().UTC(), Error: "complete response not received", Version: request.AbortDesiredVersion})
}

func relayStreams(client, upstream net.Conn, in, out io.Reader) error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(upstream, in)
		if c, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
	}()
	_, _ = io.Copy(client, out)
	if c, ok := client.(interface{ CloseWrite() error }); ok {
		_ = c.CloseWrite()
	}
	<-done
	return nil
}

func (f *replyFault) guestResponse(request *computerv0.ComputerCaptureAbortRequest, response *computerv0.ComputerCaptureAbortResponse) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	capture := request.GetCapture()
	if f.failure != "" || f.target == nil || f.target.Mode != "drop" || capture.GetCheckpointId() != f.target.CheckpointID || capture.GetComputerInstanceId() != f.target.InstanceID {
		return false
	}
	b := f.baseline
	if b == nil || capture.GetComputerId() != b.ComputerID || capture.GetWriterGeneration() != b.WriterGeneration || capture.GetMembershipRevision() != b.MembershipRevision || capture.GetDesiredVersion() != b.DesiredVersion || request.AbortDesiredVersion != b.AbortDesiredVersion || response.CheckpointId != capture.CheckpointId || response.AbortDesiredVersion != request.AbortDesiredVersion || response.Activated != request.Activate {
		f.failure = "guest abort success changed source fence"
		return false
	}
	ids := make([]string, 0, len(request.Members))
	for _, member := range request.Members {
		ids = append(ids, member.GetMember().GetRunId())
		matched := false
		for _, prior := range b.Members {
			current := member.GetMember()
			if current.GetRunId() == prior.RunID && current.GetAttemptNumber() == uint32(prior.AttemptNumber) && current.GetRunWaitId() == prior.RunWaitID && current.GetRunLeaseId() == prior.Lease.ID && member.Cancelled == prior.Cancelled {
				matched = true
			}
		}
		if !matched {
			f.failure = "guest abort changed CP member authority"
			return false
		}
		found := false
		for _, sealed := range capture.Runs {
			if proto.Equal(sealed, member.GetMember()) {
				found = true
			}
		}
		if !found {
			f.failure = "guest abort changed sealed member"
			return false
		}
	}
	slices.Sort(ids)
	if len(capture.Runs) != len(ids) || !slices.Equal(ids, f.target.RunIDs) {
		f.failure = "guest abort changed member set"
		return false
	}
	kind, stage := "guest-prepare", 1
	if request.Activate {
		kind, stage = "guest-activate", 2
	}
	drop := f.stage == stage
	if drop {
		f.stage++
	}
	f.events = append(f.events, replyEvent{Kind: kind, At: time.Now().UTC(), Dropped: drop, Version: request.AbortDesiredVersion})
	if len(f.events) > 100 {
		f.failure = "reply fault exceeded bounded case observations"
		return false
	}
	return drop
}
