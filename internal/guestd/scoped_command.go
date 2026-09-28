package guestd

import (
	"errors"
	"io"
	"os"
	"os/exec"
)

// runScopedCommand observes the command's exit before draining descendant pipes.
// A child retaining stdout or stdin must not keep the scope alive after its
// command exits. Cgroup exclusion, rather than a process-group signal, accounts
// for descendants that create their own sessions.
func runScopedCommand(cmd *exec.Cmd, scope processCgroup) (runErr, cleanupErr error) {
	defer func() { cleanupErr = errors.Join(cleanupErr, scope.close()) }()
	if err := scope.attach(cmd); err != nil {
		return err, nil
	}
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return err, nil
	}
	defer stdinR.Close()
	defer stdinW.Close()
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return err, nil
	}
	defer stdoutR.Close()
	defer stdoutW.Close()
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		return err, nil
	}
	defer stderrR.Close()
	defer stderrW.Close()
	stdin, stdout, stderr := cmd.Stdin, cmd.Stdout, cmd.Stderr
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR, stdoutW, stderrW
	var cancelScopeErr error
	cmd.Cancel = func() error {
		cancelScopeErr = scope.kill()
		if cancelScopeErr != nil {
			// Main-process termination lets Wait finish even when the cgroup
			// controller fails. It does not establish descendant exclusion.
			return errors.Join(cancelScopeErr, cmd.Process.Kill())
		}
		return nil
	}
	if err := cmd.Start(); err != nil {
		return err, nil
	}
	_ = stdinR.Close()
	_ = stdoutW.Close()
	_ = stderrW.Close()
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		defer stdinW.Close()
		if stdin != nil {
			_, _ = io.Copy(stdinW, stdin)
		}
	}()
	outputDone := make(chan error, 2)
	for _, stream := range []struct {
		dst io.Writer
		src *os.File
	}{{stdout, stdoutR}, {stderr, stderrR}} {
		go func() {
			dst := stream.dst
			if dst == nil {
				dst = io.Discard
			}
			_, err := io.Copy(dst, stream.src)
			outputDone <- err
		}()
	}
	runErr = cmd.Wait()
	cleanupErr = errors.Join(cancelScopeErr, scope.kill())
	if err := scope.waitEmpty(); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	_ = stdinW.Close()
	if cleanupErr != nil {
		// An unexcluded descendant can retain pipes indefinitely. The failure
		// remains a physical cleanup failure; closing readers does not prove exit.
		_ = stdoutR.Close()
		_ = stderrR.Close()
	}
	<-inputDone
	runErr = errors.Join(runErr, <-outputDone, <-outputDone)
	return runErr, cleanupErr
}
