package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"

	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
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
	if header.Type != wire.StreamTypeAgentComputer || bodySize != 0 {
		return relayStreams(client, upstream, in, out)
	}
	var requestBytes bytes.Buffer
	request := new(agentv1.ComputerSessionControl)
	if err := frameio.ReadProtoFrameBounded(io.TeeReader(in, &requestBytes), 16*1024*1024+64*1024, request); err != nil {
		return err
	}
	if !r.fault.awaitInspect(request.GetInspect()) {
		return nil
	}
	if _, err := upstream.Write(requestBytes.Bytes()); err != nil {
		return err
	}
	if request.GetInstall() != nil {
		// Installation is bidirectional before its final receipt. Relay the guest
		// nonce and host clock observation byte-for-byte, without recording either.
		var challengeBytes bytes.Buffer
		challenge := new(agentv1.ComputerAuthorityChallenge)
		if err := frameio.ReadProtoFrameBounded(io.TeeReader(out, &challengeBytes), 256, challenge); err != nil {
			return err
		}
		if _, err := client.Write(challengeBytes.Bytes()); err != nil {
			return err
		}
		var observationBytes bytes.Buffer
		observation := new(agentv1.ComputerAuthorityObservation)
		if err := frameio.ReadProtoFrameBounded(io.TeeReader(in, &observationBytes), 256, observation); err != nil {
			return err
		}
		if _, err := upstream.Write(observationBytes.Bytes()); err != nil {
			return err
		}
	}
	var responseBytes bytes.Buffer
	response := new(agentv1.ComputerSessionReceipt)
	if err := frameio.ReadProtoFrameBounded(io.TeeReader(out, &responseBytes), 64*1024, response); err != nil {
		return err
	}
	if r.fault.guestResponse(request, response) {
		return nil
	}
	_, err = client.Write(responseBytes.Bytes())
	return err
}

// Every matching Inspect is gated, including retries after the worker's
// request timeout. Session transport and renewal streams remain independent.
func (f *replyFault) awaitInspect(c *agentv1.ComputerSessionCapture) bool {
	f.mu.Lock()
	if c == nil || f.capture == nil || !proto.Equal(c, f.capture) || f.released {
		f.mu.Unlock()
		return true
	}
	gate := f.gate
	f.mu.Unlock()
	select {
	case <-gate:
		return true
	case <-time.After(3 * time.Minute):
		f.fail(errors.New("capture inspection gate exceeded deadline"))
		f.release()
		return false
	}
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

func (f *replyFault) guestResponse(request *agentv1.ComputerSessionControl, receipt *agentv1.ComputerSessionReceipt) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failure != "" || f.target == nil || f.target.Mode != "drop" {
		return false
	}
	if c := request.GetCapture(); c != nil {
		if !f.selected(c) {
			return false
		}
		if receipt.Error != "" || receipt.ErrorCode != 0 {
			return false
		}
		if receipt.CheckpointId != c.CheckpointId || receipt.DesiredVersion != c.DesiredVersion || !receipt.Frozen || receipt.Installed || receipt.Activated || receipt.ActivationStarted {
			f.failure = "capture receipt was not a complete frozen source"
			return false
		}
		if f.capture != nil && !proto.Equal(f.capture, c) {
			f.failure = "capture replay changed request"
			return false
		}
		f.capture = proto.Clone(c).(*agentv1.ComputerSessionCapture)
		f.target.CheckpointID = c.CheckpointId
		return f.record(replyEvent{Kind: "guest-capture", Version: c.DesiredVersion, Digest: digest(c), ExpiresAt: time.Unix(0, c.Envelope.OperationExpiresAtUnixNano).UTC()}, 0)
	}
	if c := request.GetInspect(); c != nil {
		if !proto.Equal(c, f.capture) {
			return false
		}
		f.record(replyEvent{Kind: "guest-inspect", Version: receipt.DesiredVersion, Installed: receipt.Installed, Activated: receipt.Activated}, -1)
		return false
	}
	if c := request.GetControls(); c != nil {
		if f.capture == nil || c.CheckpointId != f.capture.CheckpointId || c.GetEnvelope().GetComputerInstanceId() != f.target.InstanceID {
			return false
		}
		if receipt.Error != "" || receipt.ErrorCode != 0 {
			return false
		}
		if f.baseline == nil || c.DesiredVersion != f.baseline.DesiredVersion || receipt.CheckpointId != c.CheckpointId || receipt.DesiredVersion != c.DesiredVersion || !receipt.Installed || !receipt.Frozen || receipt.Activated || !bytes.Equal(receipt.ControlsDigest, messageDigest(c)) {
			f.failure = "controls receipt changed installed request"
			return false
		}
		f.controls = proto.Clone(c).(*agentv1.ComputerSessionControls)
		stopped := []string{}
		for _, s := range c.Sessions {
			if s.Stopped {
				stopped = append(stopped, s.GetIdentity().GetSessionId())
			}
		}
		f.record(replyEvent{Kind: "guest-controls", Version: c.DesiredVersion, Digest: digest(c), Stopped: stopped}, -1)
		return false
	}
	p, kind, stage := request.GetInstall(), "guest-install", 2
	if p == nil {
		p, kind, stage = request.GetActivate(), "guest-activate", 3
	}
	if p == nil || !f.selected(p.Capture) {
		return false
	}
	if receipt.Error != "" || receipt.ErrorCode != 0 {
		return false
	}
	if f.baseline == nil || !proto.Equal(f.baseline, p) || receipt.CheckpointId != p.Capture.CheckpointId || receipt.DesiredVersion != p.DesiredVersion || !receipt.Installed {
		f.failure = "guest continuation changed CP installation"
		return false
	}
	if kind == "guest-install" {
		if !receipt.Activated && (!receipt.Frozen || receipt.ActivationStarted) {
			f.failure = "installation receipt was not frozen"
			return false
		}
		f.installed = true
	} else {
		if receipt.Frozen || !receipt.Activated || !receipt.ActivationStarted || f.controls == nil || !bytes.Equal(receipt.ControlsDigest, messageDigest(f.controls)) {
			f.failure = "activation omitted current controls"
			return false
		}
	}
	return f.record(replyEvent{Kind: kind, Version: p.DesiredVersion, Digest: digest(p), Installed: receipt.Installed, Activated: receipt.Activated}, stage)
}
