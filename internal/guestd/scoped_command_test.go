package guestd

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This scope uses a process group to exercise command pipe sequencing without
// requiring a cgroup filesystem. Linux cgroup tests cover physical exclusion.
type commandTestScope struct {
	cmd         *exec.Cmd
	cleanupErr  error
	killFailure error
	closed      bool
	killed      bool
}

func (s *commandTestScope) attach(cmd *exec.Cmd) error {
	s.cmd = cmd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}
func (s *commandTestScope) freeze(context.Context) error { return nil }
func (s *commandTestScope) thaw(context.Context) error   { return nil }
func (s *commandTestScope) kill() error {
	s.killed = true
	if s.killFailure != nil {
		return s.killFailure
	}
	if s.cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
func (s *commandTestScope) waitEmpty() error { return s.cleanupErr }
func (s *commandTestScope) close() error     { s.closed = true; return nil }

func TestScopedCommandCleansDescendantPipesAfterMainExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "cat; sleep 30 & echo complete; exit 7")
	cmd.Stdin = strings.NewReader("input\n")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	scope := &commandTestScope{}
	runErr, cleanupErr := runScopedCommand(cmd, scope)
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 7 || cleanupErr != nil {
		t.Fatalf("run=%v cleanup=%v", runErr, cleanupErr)
	}
	if ctx.Err() != nil {
		t.Fatal("descendant-held pipes delayed command cleanup")
	}
	if stdout.String() != "input\ncomplete\n" || !scope.killed || !scope.closed {
		t.Fatalf("output=%q scope=%+v", stdout.String(), scope)
	}
}

func TestScopedCommandPreservesCleanupFailure(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "sh", "-c", "exit 0")
	failure := errors.New("scope exclusion failed")
	scope := &commandTestScope{cleanupErr: failure}
	_, cleanupErr := runScopedCommand(cmd, scope)
	if !errors.Is(cleanupErr, failure) || !scope.closed {
		t.Fatalf("cleanup=%v closed=%v", cleanupErr, scope.closed)
	}
}

func TestScopedCommandClosesScopeAfterLaunchFailure(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "/does-not-exist/exec")
	scope := &commandTestScope{}
	runErr, cleanupErr := runScopedCommand(cmd, scope)
	if runErr == nil || cleanupErr != nil || !scope.closed {
		t.Fatalf("run=%v cleanup=%v closed=%v", runErr, cleanupErr, scope.closed)
	}
}

func TestScopedCommandCancellationSurvivesCgroupKillFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "exec sleep 30")
	failure := errors.New("cgroup kill unavailable")
	scope := &commandTestScope{killFailure: failure}
	done := make(chan error, 1)
	go func() { _, cleanupErr := runScopedCommand(cmd, scope); done <- cleanupErr }()
	select {
	case cleanupErr := <-done:
		if !errors.Is(cleanupErr, failure) || !scope.closed {
			t.Fatalf("cleanup=%v closed=%v", cleanupErr, scope.closed)
		}
	case <-time.After(2 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		t.Fatal("cgroup kill failure blocked cancellation")
	}
}
