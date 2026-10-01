package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

var testTarget = replyTarget{Mode: "drop", ComputerID: "00000000-0000-0000-0000-000000000001", InstanceID: "00000000-0000-0000-0000-000000000002", CheckpointID: "00000000-0000-0000-0000-000000000003", RunIDs: []string{"00000000-0000-0000-0000-000000000004", "00000000-0000-0000-0000-000000000005"}}

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

func testAbortReceipt() workerapi.CaptureAbortResponse {
	return workerapi.CaptureAbortResponse{ComputerID: testTarget.ComputerID, ComputerInstanceID: testTarget.InstanceID, CheckpointID: testTarget.CheckpointID, WorkerHostID: "host", WorkerEpoch: 1, DesiredVersion: 5, AbortDesiredVersion: 6, WriterGeneration: 2, MembershipRevision: 3, VMPlatformID: "platform", Disposition: workerapi.CaptureAborted, WriteCapability: "must-not-appear-in-evidence", Members: []workerapi.CaptureAbortMember{
		{RunID: testTarget.RunIDs[0], AttemptNumber: 1, RunWaitID: "wait-" + testTarget.RunIDs[0], Lease: workerapi.RunLeaseFence{ID: "lease-" + testTarget.RunIDs[0], LeaseSequence: 1}, ExpiresAt: time.Now().Add(time.Minute)},
		{RunID: testTarget.RunIDs[1], AttemptNumber: 1, RunWaitID: "wait-" + testTarget.RunIDs[1], Lease: workerapi.RunLeaseFence{ID: "lease-" + testTarget.RunIDs[1], LeaseSequence: 1}, Cancelled: true},
	}}
}

func testGuestAbort(activate bool) *computerv0.ComputerCaptureAbortRequest {
	members := []*computerv0.ComputerCaptureAbortMember{}
	runs := []*computerv0.ComputerCaptureRun{}
	for i, id := range testTarget.RunIDs {
		member := &computerv0.ComputerCaptureRun{RunId: id, AttemptNumber: 1, RunWaitId: "wait-" + id, RunLeaseId: "lease-" + id}
		runs = append(runs, member)
		members = append(members, &computerv0.ComputerCaptureAbortMember{Member: member, Cancelled: i == 1})
	}
	return &computerv0.ComputerCaptureAbortRequest{Capture: &computerv0.FreezeComputerRequest{ComputerId: testTarget.ComputerID, ComputerInstanceId: testTarget.InstanceID, CheckpointId: testTarget.CheckpointID, WriterGeneration: 2, DesiredVersion: 5, MembershipRevision: 3, Runs: runs}, Members: members, AbortDesiredVersion: 6, Activate: activate}
}

func TestAppliedRepliesAreLostOnceAndReplayRestoresSocket(t *testing.T) {
	var guestApplied atomic.Int32
	f, path := relayFixture(t, func(conn net.Conn, reader *bufio.Reader) {
		if _, _, err := wire.ReadStreamFrameHeader(reader); err != nil {
			return
		}
		var request computerv0.ComputerCaptureAbortRequest
		if frameio.ReadProtoFrame(reader, &request) != nil {
			return
		}
		guestApplied.Add(1)
		_ = frameio.WriteProtoFrame(conn, &computerv0.ComputerCaptureAbortResponse{CheckpointId: request.Capture.CheckpointId, AbortDesiredVersion: request.AbortDesiredVersion, Activated: request.Activate})
	})
	if err := f.arm(testTarget); err != nil {
		t.Fatal(err)
	}
	var completed atomic.Bool
	var applied atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer unit-test" {
			t.Error("authentication was changed")
		}
		applied.Add(1)
		if r.URL.Path == abortPath+"/complete" {
			completed.Store(true)
			_ = json.NewEncoder(w).Encode(workerapi.ComputerCheckpointResponse{ComputerInstanceID: testTarget.InstanceID, CheckpointID: testTarget.CheckpointID, WorkerEpoch: 1, DesiredVersion: 6})
		} else {
			receipt := testAbortReceipt()
			if completed.Load() {
				receipt.Disposition, receipt.Members = workerapi.CaptureAbortAcknowledged, nil
			}
			_ = json.NewEncoder(w).Encode(receipt)
		}
	}))
	defer upstream.Close()
	proxy := httptest.NewServer(f.proxy(strings.TrimPrefix(upstream.URL, "http://")))
	defer proxy.Close()
	call := func(path string, wantLost bool) {
		t.Helper()
		body, _ := json.Marshal(workerapi.CaptureAbortRequest{ComputerInstanceID: testTarget.InstanceID, CheckpointID: testTarget.CheckpointID, WorkerEpoch: 1, DesiredVersion: 5})
		request, _ := http.NewRequest("POST", proxy.URL+path, bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer unit-test")
		response, err := http.DefaultClient.Do(request)
		if (err != nil) != wantLost {
			t.Fatalf("lost=%v, response=%v err=%v", wantLost, response, err)
		}
		if response != nil {
			defer response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatalf("response %d", response.StatusCode)
			}
		}
	}
	guest := func(activate, wantLost bool) {
		t.Helper()
		conn, reader := connectRelay(t, path, wire.StreamTypeComputerCaptureAbort)
		defer conn.Close()
		if err := frameio.WriteProtoFrame(conn, testGuestAbort(activate)); err != nil {
			t.Fatal(err)
		}
		var response computerv0.ComputerCaptureAbortResponse
		err := frameio.ReadProtoFrame(reader, &response)
		if (err != nil) != wantLost {
			t.Fatalf("guest loss=%v err=%v", wantLost, err)
		}
	}
	call(abortPath, true)
	call(abortPath, false)
	guest(false, true)
	call(abortPath, false)
	guest(false, false)
	guest(true, true)
	call(abortPath, false)
	guest(false, false)
	guest(true, false)
	call(abortPath+"/complete", true)
	call(abortPath, false)
	if applied.Load() != 6 || guestApplied.Load() != 5 || f.stage != 4 || f.failure != "" || !f.relay.restored {
		t.Fatalf("applied=%d guest=%d stage=%d failure=%q restored=%v", applied.Load(), guestApplied.Load(), f.stage, f.failure, f.relay.restored)
	}
	evidence, _ := json.Marshal(f.events)
	if strings.Contains(string(evidence), "must-not-appear") {
		t.Fatal("capability leaked into fault evidence")
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
	conn, reader := connectRelay(t, path, wire.StreamTypeProgramRun)
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

func TestAbortReplayRejectsChangedMemberAuthority(t *testing.T) {
	for name, mutate := range map[string]func(*workerapi.CaptureAbortMember){
		"attempt":  func(m *workerapi.CaptureAbortMember) { m.AttemptNumber++ },
		"wait":     func(m *workerapi.CaptureAbortMember) { m.RunWaitID = "other" },
		"lease":    func(m *workerapi.CaptureAbortMember) { m.Lease.ID = "other" },
		"sequence": func(m *workerapi.CaptureAbortMember) { m.Lease.LeaseSequence++ },
		"disk":     func(m *workerapi.CaptureAbortMember) { m.BaseComputerDiskVersionID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			baseline := testAbortReceipt()
			f := &replyFault{target: &testTarget, baseline: &baseline, stage: 1}
			receipt := testAbortReceipt()
			mutate(&receipt.Members[0])
			body, _ := json.Marshal(receipt)
			request := httptest.NewRequest("POST", abortPath, nil)
			request = request.WithContext(context.WithValue(request.Context(), replyRequestKey{}, workerapi.CaptureAbortRequest{ComputerInstanceID: testTarget.InstanceID, CheckpointID: testTarget.CheckpointID, WorkerEpoch: 1, DesiredVersion: 5}))
			response := &http.Response{StatusCode: 200, Request: request, Body: io.NopCloser(bytes.NewReader(body))}
			if err := f.response(response); err != nil || f.failure == "" || len(f.events) != 0 {
				t.Fatal("changed member was recorded as a valid replay")
			}
			forwarded, _ := io.ReadAll(response.Body)
			if !bytes.Equal(forwarded, body) {
				t.Fatal("anomalous authority was not forwarded unchanged")
			}
		})
	}
	baseline := testAbortReceipt()
	f := &replyFault{target: &testTarget, baseline: &baseline, stage: 1}
	request := testGuestAbort(false)
	request.Members[0].Member.RunLeaseId = "other"
	// The altered capture and member point to the same tuple; only comparison
	// with the CP receipt detects this change.
	response := &computerv0.ComputerCaptureAbortResponse{CheckpointId: testTarget.CheckpointID, AbortDesiredVersion: 6}
	if drop := f.guestResponse(request, response); f.failure == "" || drop {
		t.Fatal("guest member was not bound to CP authority")
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
	conn, reader := connectRelay(t, path, wire.StreamTypeProgramRun)
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
