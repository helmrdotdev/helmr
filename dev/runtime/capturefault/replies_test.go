package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

var testTarget = replyTarget{Mode: "drop", ComputerID: "00000000-0000-0000-0000-000000000001", InstanceID: "00000000-0000-0000-0000-000000000002", WriterGeneration: 2, Members: []replyMember{{"00000000-0000-0000-0000-000000000004", 1}, {"00000000-0000-0000-0000-000000000005", 1}}}

const testCheckpoint = "00000000-0000-0000-0000-000000000003"

func relayFixture(t *testing.T, handler func(net.Conn, *bufio.Reader)) (*replyFault, string) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "reply-") // Stay below AF_UNIX's path bound on both test hosts.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	dir := filepath.Join(root, testTarget.InstanceID, "root")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "vsock.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				if _, err := reader.ReadString('\n'); err != nil {
					return
				}
				_, _ = io.WriteString(conn, "OK 1234\n")
				handler(conn, reader)
			}()
		}
	}()
	f := &replyFault{jailRoot: root}
	t.Cleanup(func() {
		if f.relay != nil {
			_ = f.relay.restore()
		}
	})
	return f, path
}

func connectRelay(t *testing.T, path string, kind wire.StreamType) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(conn, "CONNECT 5000\n")
	reader := bufio.NewReader(conn)
	if line, err := reader.ReadString('\n'); err != nil || line != "OK 1234\n" {
		t.Fatalf("handshake %q: %v", line, err)
	}
	if err := wire.WriteStreamFrameHeader(conn, wire.StreamHeader{Type: kind}, 0); err != nil {
		t.Fatal(err)
	}
	return conn, reader
}

func testCapture() *agentv1.ComputerSessionCapture {
	return &agentv1.ComputerSessionCapture{CheckpointId: testCheckpoint, DesiredVersion: 5, MembershipRevision: 3, Envelope: &computerv0.ComputerOperationEnvelope{OperationId: testCheckpoint, ComputerId: testTarget.ComputerID, ComputerInstanceId: testTarget.InstanceID, WriterGeneration: 2, ChannelCredential: "must-not-appear-in-evidence", OperationExpiresAtUnixNano: time.Now().Add(time.Minute).UnixNano()}, Sessions: []*agentv1.SessionIdentity{{SessionId: testTarget.Members[0].SessionID, ProcessEpoch: 1}, {SessionId: testTarget.Members[1].SessionID, ProcessEpoch: 1}}}
}
func testInstallation(c *agentv1.ComputerSessionCapture) *agentv1.ComputerSessionInstallation {
	return &agentv1.ComputerSessionInstallation{Capture: c, Envelope: proto.Clone(c.Envelope).(*computerv0.ComputerOperationEnvelope), DesiredVersion: 6, SourceAbort: true, StoppedSessions: []*agentv1.SessionIdentity{c.Sessions[1]}}
}
func encoded(t *testing.T, p proto.Message) []byte {
	t.Helper()
	b, e := proto.Marshal(p)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestAppliedRepliesAreLostOnceAndReplayRestoresSocket(t *testing.T) {
	capture := testCapture()
	installation := testInstallation(capture)
	controls := &agentv1.ComputerSessionControls{Envelope: installation.Envelope, CheckpointId: testCheckpoint, DesiredVersion: 6, Sessions: []*agentv1.SessionContinuationControl{{Identity: capture.Sessions[0], AuthorityGeneration: 3}, {Identity: capture.Sessions[1], AuthorityGeneration: 4, Stopped: true}}}
	f, path := relayFixture(t, func(conn net.Conn, reader *bufio.Reader) {
		if _, _, err := wire.ReadStreamFrameHeader(reader); err != nil {
			return
		}
		q := new(agentv1.ComputerSessionControl)
		if frameio.ReadProtoFrame(reader, q) != nil {
			return
		}
		if q.GetInstall() != nil {
			nonce := bytes.Repeat([]byte{7}, 32)
			_ = frameio.WriteProtoFrame(conn, &agentv1.ComputerAuthorityChallenge{Nonce: nonce})
			observed := new(agentv1.ComputerAuthorityObservation)
			if frameio.ReadProtoFrame(reader, observed) != nil || !bytes.Equal(observed.Nonce, nonce) || observed.AuthorityTimeUnixNano <= 0 {
				t.Error("lost install clock exchange")
				return
			}
		}
		receipt := &agentv1.ComputerSessionReceipt{CheckpointId: testCheckpoint, DesiredVersion: 6, Frozen: true, Installed: true}
		if q.GetCapture() != nil || q.GetInspect() != nil {
			receipt.DesiredVersion = 5
			receipt.Installed = false
		}
		if q.GetControls() != nil || q.GetActivate() != nil {
			receipt.ControlsDigest = messageDigest(controls)
		}
		if q.GetActivate() != nil {
			receipt.Frozen = false
			receipt.Activated = true
			receipt.ActivationStarted = true
		}
		_ = frameio.WriteProtoFrame(conn, receipt)
	})
	if err := f.arm(testTarget); err != nil {
		t.Fatal(err)
	}
	guest := func(q *agentv1.ComputerSessionControl, lost bool) {
		t.Helper()
		conn, reader := connectRelay(t, path, wire.StreamTypeAgentComputer)
		defer conn.Close()
		if err := frameio.WriteProtoFrame(conn, q); err != nil {
			t.Fatal(err)
		}
		if q.GetInstall() != nil {
			challenge := new(agentv1.ComputerAuthorityChallenge)
			if err := frameio.ReadProtoFrame(reader, challenge); err != nil {
				t.Fatal(err)
			}
			if err := frameio.WriteProtoFrame(conn, &agentv1.ComputerAuthorityObservation{Nonce: challenge.Nonce, AuthorityTimeUnixNano: time.Now().UnixNano()}); err != nil {
				t.Fatal(err)
			}
		}
		err := frameio.ReadProtoFrame(reader, new(agentv1.ComputerSessionReceipt))
		if (err != nil) != lost {
			t.Fatalf("guest lost=%v err=%v", lost, err)
		}
	}
	guest(&agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Capture{Capture: capture}}, true)
	// Two attempts must both wait; neither a retry nor a foreign capture releases it.
	waits := make(chan bool, 2)
	for range 2 {
		go func() { waits <- f.awaitInspect(capture) }()
	}
	other := proto.Clone(capture).(*agentv1.ComputerSessionCapture)
	other.CheckpointId = "another"
	if !f.awaitInspect(other) {
		t.Fatal("blocked another capture")
	}
	select {
	case <-waits:
		t.Fatal("inspection escaped closed gate")
	case <-time.After(20 * time.Millisecond):
	}
	f.release()
	for range 2 {
		if !<-waits {
			t.Fatal("inspection not released")
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer unit-test" {
			t.Error("authentication changed")
		}
		if r.URL.Path == abortPath+"/prepare" {
			_ = json.NewEncoder(w).Encode(workerapi.AgentComputerInstallationResponse{Installation: encoded(t, installation)})
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer upstream.Close()
	proxy := httptest.NewServer(f.proxy(strings.TrimPrefix(upstream.URL, "http://")))
	defer proxy.Close()
	call := func(path string, request any, lost bool) {
		t.Helper()
		body, _ := json.Marshal(request)
		q, _ := http.NewRequest("POST", proxy.URL+path, bytes.NewReader(body))
		q.Header.Set("Authorization", "Bearer unit-test")
		r, err := http.DefaultClient.Do(q)
		if (err != nil) != lost {
			t.Fatalf("CP lost=%v err=%v", lost, err)
		}
		if r != nil {
			defer r.Body.Close()
			if r.StatusCode < 200 || r.StatusCode >= 300 {
				t.Fatal(r.Status)
			}
		}
	}
	prepare := workerapi.AgentComputerSourceAbortRequest{CheckpointID: testCheckpoint, LeaseEpoch: 2}
	call(abortPath+"/prepare", prepare, true)
	call(abortPath+"/prepare", prepare, false)
	guest(&agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Install{Install: installation}}, true)
	guest(&agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Install{Install: installation}}, false)
	guest(&agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Controls{Controls: controls}}, false)
	guest(&agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Activate{Activate: installation}}, true)
	guest(&agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Activate{Activate: installation}}, false)
	complete := workerapi.AgentComputerInstallationRequest{Installation: encoded(t, installation), Receipt: encoded(t, &agentv1.ComputerSessionReceipt{CheckpointId: testCheckpoint, DesiredVersion: 6, Installed: true, Activated: true, ActivationStarted: true, ControlsDigest: messageDigest(controls)})}
	call(abortPath+"/complete", complete, true)
	call(abortPath+"/complete", complete, false)
	if f.stage != 5 || f.failure != "" || !f.relay.restored {
		t.Fatalf("stage=%d failure=%q restored=%v", f.stage, f.failure, f.relay.restored)
	}
	evidence, _ := json.Marshal(f.events)
	if strings.Contains(string(evidence), "must-not-appear") {
		t.Fatal("credentials in evidence")
	}
}

func TestListenerRestoreKeepsEstablishedProgramStream(t *testing.T) {
	f, path := relayFixture(t, func(conn net.Conn, reader *bufio.Reader) {
		if _, _, err := wire.ReadStreamFrameHeader(reader); err == nil {
			_, _ = io.Copy(conn, reader)
		}
	})
	target := testTarget
	target.Mode = "passthrough"
	if err := f.arm(target); err != nil {
		t.Fatal(err)
	}
	conn, reader := connectRelay(t, path, wire.StreamTypeAgentSession)
	exchange := func(text string) {
		t.Helper()
		_, _ = io.WriteString(conn, text)
		got := make([]byte, len(text))
		if _, err := io.ReadFull(reader, got); err != nil || string(got) != text {
			t.Fatalf("stream %q: %v", got, err)
		}
	}
	exchange("before")
	if err := f.relay.restore(); err != nil {
		t.Fatal(err)
	}
	exchange("after")
	if f.relay.active.Load() != 1 || f.relay.forwarded.Load() != 1 {
		t.Fatal("restoring the pathname lost the existing stream")
	}
	info, _ := os.Lstat(path)
	if !os.SameFile(info, f.relay.original) {
		t.Fatal("original socket inode was not restored")
	}
}

func TestRelayRestoreRefusesChangedSocket(t *testing.T) {
	f, path := relayFixture(t, func(net.Conn, *bufio.Reader) {})
	if err := f.arm(testTarget); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("other owner"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.relay.restore(); err == nil {
		t.Fatal("overwrote a changed socket")
	}
	_ = f.relay.listener.Close()
	if content, _ := os.ReadFile(path); string(content) != "other owner" {
		t.Fatal("changed endpoint was modified")
	}
}

func TestGuestReplayRejectsChangedInstalledAuthority(t *testing.T) {
	for _, field := range []string{"credential", "expiry", "member", "generation"} {
		t.Run(field, func(t *testing.T) {
			capture := testCapture()
			baseline := testInstallation(capture)
			baseline.Grants = []*agentv1.SessionGrant{{Identity: capture.Sessions[0], AuthorityGeneration: 1, ChannelCredential: "current", ExpiresAtUnixNano: 123}}
			target := testTarget
			target.CheckpointID = testCheckpoint
			f := &replyFault{target: &target, capture: capture, baseline: baseline, installed: true, stage: 3}
			p := proto.Clone(baseline).(*agentv1.ComputerSessionInstallation)
			switch field {
			case "credential":
				p.Grants[0].ChannelCredential = "other"
			case "expiry":
				p.Grants[0].ExpiresAtUnixNano++
			case "member":
				p.Grants[0].Identity.ProcessEpoch++
			case "generation":
				p.Grants[0].AuthorityGeneration++
			}
			receipt := &agentv1.ComputerSessionReceipt{CheckpointId: testCheckpoint, DesiredVersion: 6, Frozen: true, Installed: true}
			if f.guestResponse(&agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Install{Install: p}}, receipt) || f.failure == "" {
				t.Fatal("changed installation accepted")
			}
		})
	}
}

func TestRelayPreservesGuestHalfClose(t *testing.T) {
	received := make(chan string, 1)
	f, path := relayFixture(t, func(conn net.Conn, reader *bufio.Reader) {
		if _, _, err := wire.ReadStreamFrameHeader(reader); err != nil {
			return
		}
		_, _ = io.WriteString(conn, "guest done")
		_ = conn.(*net.UnixConn).CloseWrite()
		body, _ := io.ReadAll(reader)
		received <- string(body)
	})
	target := testTarget
	target.Mode = "passthrough"
	if err := f.arm(target); err != nil {
		t.Fatal(err)
	}
	conn, reader := connectRelay(t, path, wire.StreamTypeAgentSession)
	if body, err := io.ReadAll(reader); err != nil || string(body) != "guest done" {
		t.Fatalf("guest half-close: %q %v", body, err)
	}
	if _, err := io.WriteString(conn, "host still writable"); err != nil {
		t.Fatal(err)
	}
	_ = conn.(*net.UnixConn).CloseWrite()
	select {
	case body := <-received:
		if body != "host still writable" {
			t.Fatalf("host direction lost: %q", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("guest did not receive host data after half-close")
	}
}

func TestRelayCanRestoreAfterListenerAlreadyClosed(t *testing.T) {
	f, path := relayFixture(t, func(net.Conn, *bufio.Reader) {})
	if err := f.arm(testTarget); err != nil {
		t.Fatal(err)
	}
	_ = f.relay.listener.Close()
	if err := f.relay.restore(); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Lstat(path)
	if !os.SameFile(info, f.relay.original) {
		t.Fatal("original socket not restored")
	}
}

func TestCaptureFaultLeavesOtherTargetsAndFailedReceiptsAlone(t *testing.T) {
	for _, name := range []string{"computer", "instance", "generation", "member", "epoch", "failed", "incomplete"} {
		t.Run(name, func(t *testing.T) {
			target := testTarget
			f := &replyFault{target: &target, gate: make(chan struct{})}
			c := testCapture()
			r := &agentv1.ComputerSessionReceipt{CheckpointId: c.CheckpointId, DesiredVersion: c.DesiredVersion, Frozen: true}
			switch name {
			case "computer":
				c.Envelope.ComputerId = "another"
			case "instance":
				c.Envelope.ComputerInstanceId = "another"
			case "generation":
				c.Envelope.WriterGeneration++
			case "member":
				c.Sessions[0].SessionId = "another"
			case "epoch":
				c.Sessions[0].ProcessEpoch++
			case "failed":
				r.Error = "capture refused"
			case "incomplete":
				r.Frozen = false
			}
			if f.guestResponse(&agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Capture{Capture: c}}, r) || f.stage != 0 || f.capture != nil {
				t.Fatal("lost an unrelated or unapplied receipt")
			}
			if name == "incomplete" && f.failure == "" {
				t.Fatal("invalid complete capture receipt was not reported")
			}
		})
	}
}

func TestActivationRequiresExactCurrentControlsReceipt(t *testing.T) {
	c := testCapture()
	p := testInstallation(c)
	target := testTarget
	target.CheckpointID = c.CheckpointId
	controls := &agentv1.ComputerSessionControls{Envelope: p.Envelope, CheckpointId: c.CheckpointId, DesiredVersion: p.DesiredVersion}
	f := &replyFault{target: &target, capture: c, baseline: p, installed: true, controls: controls, stage: 3}
	stale := proto.Clone(controls).(*agentv1.ComputerSessionControls)
	stale.Sessions = []*agentv1.SessionContinuationControl{{Identity: c.Sessions[1], Stopped: true, AuthorityGeneration: 3}}
	r := &agentv1.ComputerSessionReceipt{CheckpointId: c.CheckpointId, DesiredVersion: p.DesiredVersion, Installed: true, Activated: true, ActivationStarted: true, ControlsDigest: messageDigest(stale)}
	if f.guestResponse(&agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Activate{Activate: p}}, r) || f.failure == "" {
		t.Fatal("activation counted with a different controls receipt")
	}
}
