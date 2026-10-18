//go:build linux

package guestd

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// runSessionInit remains namespace PID 1 for an Agent Session. Only children
// whose parent has exited are adopted here; a native application's own waits
// remain its responsibility. Live adoptees are never signaled by the reaper.
func runSessionInit(path string, argv, env []string) error {
	// Route signals addressed to namespace init (kill(1, sig)) to the managed app.
	// Forward only to the managed application; never signal living adoptees here.
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP,
		syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGWINCH,
		syscall.SIGCONT, syscall.SIGTSTP, syscall.SIGTTIN, syscall.SIGTTOU)
	defer signal.Stop(signals)
	// Go preserves inherited descriptors above Files unless marked close-on-exec.
	syscall.CloseOnExec(4)
	syscall.CloseOnExec(5)
	events := os.NewFile(3, "Session events")
	childFD := -1
	child, err := os.StartProcess(path, argv, &os.ProcAttr{
		Env:   env,
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr, events},
		Sys:   &syscall.SysProcAttr{PidFD: &childFD, Setpgid: true},
	})
	if err != nil {
		return fmt.Errorf("exec Session runtime: %w", err)
	}
	childPID := child.Pid
	// wait4 below is the sole wait owner, including for the managed child.
	_ = child.Release()
	defer unix.Close(childFD)
	if err := unix.Sendmsg(5, []byte{1}, unix.UnixRights(childFD), nil, 0); err != nil {
		return fmt.Errorf("send Session runtime identity: %w", err)
	}
	_ = unix.Close(5)
	// StartProcess has joined Go's exec-error pipe. The application does not
	// inherit these proof descriptors or a second copy of the event/log writers.
	closeAgentFiles(os.Stdin, os.Stdout, os.Stderr, events)
	_ = unix.Close(4)
	done := make(chan struct{})
	joined := make(chan struct{})
	defer func() { close(done); <-joined }()
	go func() {
		defer close(joined)
		for {
			select {
			case sig := <-signals:
				_ = unix.PidfdSendSignal(childFD, sig.(syscall.Signal), nil, 0)
			case <-done:
				return
			}
		}
	}()
	for {
		var status unix.WaitStatus
		pid, err := unix.Wait4(-1, &status, 0, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("wait Session child: %w", err)
		}
		if pid != childPID {
			continue
		}
		// Namespace-init exit contains any remaining descendants. Signals are
		// represented by the conventional 128+signal exit code; PID 1 cannot reliably
		// reproduce default signal termination by signaling itself.
		if status.Signaled() {
			os.Exit(128 + int(status.Signal()))
		}
		os.Exit(status.ExitStatus())
	}
}

// The sender publishes exactly one kernel-owned pidfd before closing exec proof.
// Receiving rights avoids translating a PID from the child's private namespace.
func receiveSessionChild(socket int) (int, error) {
	data := make([]byte, 1)
	control := make([]byte, unix.CmsgSpace(4))
	n, oobn, flags, _, err := unix.Recvmsg(socket, data, control, unix.MSG_DONTWAIT|unix.MSG_CMSG_CLOEXEC)
	if err != nil {
		return -1, fmt.Errorf("receive Session runtime identity: %w", err)
	}
	messages, err := unix.ParseSocketControlMessage(control[:oobn])
	if err != nil {
		return -1, err
	}
	var descriptors []int
	for _, message := range messages {
		rights, err := unix.ParseUnixRights(&message)
		if err != nil {
			for _, fd := range descriptors {
				_ = unix.Close(fd)
			}
			return -1, err
		}
		descriptors = append(descriptors, rights...)
	}
	if n != 1 || data[0] != 1 || flags&unix.MSG_CTRUNC != 0 || len(descriptors) != 1 {
		for _, fd := range descriptors {
			_ = unix.Close(fd)
		}
		return -1, errors.New("invalid Session runtime identity")
	}
	return descriptors[0], nil
}
