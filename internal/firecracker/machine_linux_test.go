//go:build linux

package firecracker

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestIgnoreExpectedStopErrorsDropsFirecrackerSIGTERM(t *testing.T) {
	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatal("waitErr = nil, want signal error")
	}
	if err := ignoreExpectedStopErrors(waitErr); err != nil {
		t.Fatalf("ignoreExpectedStopErrors = %v, want nil", err)
	}

	cleanupErr := os.ErrPermission
	if err := ignoreExpectedStopErrors(testWrappedErrors{waitErr, cleanupErr}); !errors.Is(err, cleanupErr) {
		t.Fatalf("ignoreExpectedStopErrors wrapped = %v, want %v", err, cleanupErr)
	}
}

func TestIgnoreStopSignalErrorDropsForcedSIGKILL(t *testing.T) {
	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatal("waitErr = nil, want signal error")
	}
	if err := ignoreStopSignalError(waitErr, syscall.SIGKILL); err != nil {
		t.Fatalf("ignoreStopSignalError = %v, want nil", err)
	}
	if err := ignoreExpectedStopErrors(waitErr); err == nil {
		t.Fatal("ignoreExpectedStopErrors ignored SIGKILL outside force-kill path")
	}
}

type testWrappedErrors []error

func (e testWrappedErrors) Error() string {
	return "wrapped errors"
}

func (e testWrappedErrors) WrappedErrors() []error {
	return []error(e)
}
