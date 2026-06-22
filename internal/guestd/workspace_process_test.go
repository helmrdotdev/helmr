package guestd

import (
	"bytes"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

type recordingWriteCloser struct {
	bytes.Buffer
	closed bool
}

func (w *recordingWriteCloser) Close() error {
	w.closed = true
	return nil
}

func TestWorkspaceProcessInputDeliveryIsOffsetIdempotent(t *testing.T) {
	stdin := &recordingWriteCloser{}
	process := &workspaceProcess{resourceKind: "exec", resourceID: "exec-1", stdin: stdin}

	ack, err := process.writeInput(0, []byte("hello"))
	if err != nil {
		t.Fatalf("write input: %v", err)
	}
	if ack != 5 {
		t.Fatalf("ack offset = %d, want 5", ack)
	}
	ack, err = process.writeInput(0, []byte("hello"))
	if err != nil {
		t.Fatalf("duplicate write input: %v", err)
	}
	if ack != 5 {
		t.Fatalf("duplicate ack offset = %d, want 5", ack)
	}
	if got := stdin.String(); got != "hello" {
		t.Fatalf("stdin = %q, want one delivery", got)
	}
	if _, err := process.writeInput(0, []byte("HELLO")); err == nil || !strings.Contains(err.Error(), "offset conflict") {
		t.Fatalf("conflicting duplicate error = %v, want offset conflict", err)
	}
	if _, err := process.writeInput(7, []byte("!")); err == nil || !strings.Contains(err.Error(), "offset conflict") {
		t.Fatalf("gap write error = %v, want offset conflict", err)
	}
	if err := process.closeInput(5); err != nil {
		t.Fatalf("close input: %v", err)
	}
	if !stdin.closed {
		t.Fatal("stdin was not closed")
	}
	if err := process.closeInput(5); err != nil {
		t.Fatalf("duplicate close input: %v", err)
	}
}

func TestWorkspaceProcessInputDedupeRetainsNewestOffsets(t *testing.T) {
	stdin := &recordingWriteCloser{}
	process := &workspaceProcess{resourceKind: "exec", resourceID: "exec-1", stdin: stdin}

	for offset := uint64(0); offset < 1100; offset++ {
		ack, err := process.writeInput(offset, []byte("x"))
		if err != nil {
			t.Fatalf("write input at %d: %v", offset, err)
		}
		if ack != offset+1 {
			t.Fatalf("ack at %d = %d, want %d", offset, ack, offset+1)
		}
	}
	ack, err := process.writeInput(1099, []byte("x"))
	if err != nil {
		t.Fatalf("duplicate recent input after trim: %v", err)
	}
	if ack != 1100 {
		t.Fatalf("duplicate recent ack = %d, want 1100", ack)
	}
	if got := stdin.Len(); got != 1100 {
		t.Fatalf("stdin len = %d, want one delivery per chunk", got)
	}
	if _, err := process.writeInput(0, []byte("x")); err == nil || !strings.Contains(err.Error(), "offset conflict") {
		t.Fatalf("old evicted duplicate error = %v, want offset conflict", err)
	}
}

func TestWorkspaceInputMissingProcessDoesNotAckDelivery(t *testing.T) {
	entry := &workspaceMaterializationEntry{}
	if ack, err := entry.writeWorkspaceInput("exec", "missing-exec", 0, []byte("hello")); err == nil {
		t.Fatalf("missing process ack = %d, want error", ack)
	}
	if err := entry.closeWorkspaceInput("exec", "missing-exec", 0); err == nil {
		t.Fatal("missing process close returned nil, want error")
	}

	stdin := &recordingWriteCloser{}
	process := &workspaceProcess{resourceKind: "exec", resourceID: "exec-1", stdin: stdin}
	if err := entry.registerWorkspaceProcess(process); err != nil {
		t.Fatalf("register process: %v", err)
	}
	ack, err := entry.writeWorkspaceInput("exec", "exec-1", 0, []byte("hello"))
	if err != nil {
		t.Fatalf("write registered process: %v", err)
	}
	if ack != 5 {
		t.Fatalf("registered process ack = %d, want 5", ack)
	}
	if got := stdin.String(); got != "hello" {
		t.Fatalf("registered process stdin = %q, want hello", got)
	}
}

func TestWorkspacePtyControlMissingProcessFails(t *testing.T) {
	entry := &workspaceMaterializationEntry{}
	if err := entry.resizeWorkspacePty(`{"pty_id":"missing-pty","cols":120,"rows":40}`); err == nil || !strings.Contains(err.Error(), "missing-pty") {
		t.Fatalf("missing pty resize error = %v, want target error", err)
	}
	if err := entry.closeWorkspacePty(`{"pty_id":"missing-pty"}`); err == nil || !strings.Contains(err.Error(), "missing-pty") {
		t.Fatalf("missing pty close error = %v, want target error", err)
	}
}

func TestWorkspacePtyInputCloseIsUnsupported(t *testing.T) {
	entry := &workspaceMaterializationEntry{}
	stdin := &recordingWriteCloser{}
	process := &workspaceProcess{resourceKind: "pty", resourceID: "pty-1", stdin: stdin}
	if err := entry.registerWorkspaceProcess(process); err != nil {
		t.Fatalf("register pty process: %v", err)
	}
	if err := entry.closeWorkspaceInput("workspace_pty", "pty-1", 0); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("pty input close error = %v, want unsupported", err)
	}
	if stdin.closed {
		t.Fatal("pty stdin was closed by input close frame")
	}
}

func TestSignalWorkspaceProcessDoesNotSignalCallerProcessGroup(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			<-done
		}
	}()

	if err := signalWorkspaceProcess(cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("signal child: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("child was not terminated")
	}
}
