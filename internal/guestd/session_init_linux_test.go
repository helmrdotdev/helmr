//go:build linux

package guestd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSessionInitHelper(t *testing.T) {
	mode := os.Getenv("HELMR_SESSION_INIT_TEST")
	if mode == "" {
		return
	}
	switch mode {
	case "init":
		if os.Getpid() != 1 {
			panic("requires private PID namespace")
		}
		if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
			panic(err)
		}
		if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
			panic(err)
		}
		path := os.Args[0]
		if os.Getenv("HELMR_SESSION_INIT_FAIL") == "1" {
			path = "/missing-session-runtime"
		}
		env := []string{}
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "HELMR_SESSION_INIT_TEST=") {
				env = append(env, value)
			}
		}
		env = append(env, "HELMR_SESSION_INIT_TEST=runtime")
		for _, fd := range []int{4, 5} {
			target, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
			if err != nil {
				panic(err)
			}
			env = append(env, fmt.Sprintf("HELMR_SESSION_INIT_FD%d=%s", fd, target))
		}
		if err := runSessionInit(path, []string{path, "-test.run=^TestSessionInitHelper$"}, env); err != nil {
			writeImageRuntimeInitFailure(err)
			os.Exit(127)
		}
	case "runtime":
		files, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			panic(err)
		}
		for _, file := range files {
			target, _ := os.Readlink("/proc/self/fd/" + file.Name())
			if target == os.Getenv("HELMR_SESSION_INIT_FD4") || target == os.Getenv("HELMR_SESSION_INIT_FD5") {
				panic("startup descriptor leaked: " + target)
			}
		}
		// A child with a living parent must deliver its exact status to that parent.
		native := exec.Command(os.Args[0], "-test.run=^TestSessionInitHelper$")
		native.Env = append(os.Environ(), "HELMR_SESSION_INIT_TEST=status")
		err = native.Run()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 37 {
			panic(fmt.Sprintf("native wait: %v", err))
		}
		dir := os.Getenv("HELMR_SESSION_INIT_DIR")
		for range 40 {
			parent := exec.Command(os.Args[0], "-test.run=^TestSessionInitHelper$")
			parent.Env = append(os.Environ(), "HELMR_SESSION_INIT_TEST=parent")
			output, err := parent.Output()
			if err != nil {
				panic(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
			if err != nil {
				panic(err)
			}
			path := fmt.Sprintf("/proc/%d/stat", pid)
			stat, err := os.ReadFile(path)
			if err != nil {
				panic("living orphan disappeared")
			}
			fields := strings.Fields(string(stat)[strings.LastIndex(string(stat), ")")+1:])
			if len(fields) < 2 || fields[0] == "Z" || fields[1] != "1" {
				panic("probe is not a living adoptee: " + string(stat))
			}
			if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)), nil, 0600); err != nil {
				panic(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				_, err := os.Stat(path)
				if errors.Is(err, os.ErrNotExist) {
					break
				}
				if time.Now().After(deadline) {
					panic("dead orphan not reaped")
				}
				time.Sleep(time.Millisecond)
			}
		}
		events := os.NewFile(3, "events")
		_, _ = events.WriteString("native-wait-and-40-orphans-ok\n")
		_ = events.Close()
		// Event EOF must reach the observer while both runtime and init are alive.
		for {
			if _, err := os.Stat(filepath.Join(dir, "exit")); err == nil {
				os.Exit(42)
			}
			time.Sleep(time.Millisecond)
		}
	case "parent":
		leaf := exec.Command(os.Args[0], "-test.run=^TestSessionInitHelper$")
		leaf.Env = append(os.Environ(), "HELMR_SESSION_INIT_TEST=leaf")
		if err := leaf.Start(); err != nil {
			panic(err)
		}
		fmt.Println(leaf.Process.Pid)
		os.Exit(0)
	case "leaf":
		gate := filepath.Join(os.Getenv("HELMR_SESSION_INIT_DIR"), strconv.Itoa(os.Getpid()))
		for {
			if _, err := os.Stat(gate); err == nil {
				os.Exit(0)
			}
			time.Sleep(time.Millisecond)
		}
	case "status":
		os.Exit(37)
	}
	os.Exit(126)
}

func TestSessionInitReapsOnlyExitedAdoptees(t *testing.T) {
	if os.Getenv("HELMR_PRIVILEGED_PROGRAM_TEST") != "1" {
		t.Skip("requires disposable privileged Linux namespace")
	}
	if os.Geteuid() != 0 {
		t.Fatal("Session init test requires root")
	}
	dir := t.TempDir()
	cmd, proof, events, identity := startSessionInitTest(t, dir, false)
	body, err := io.ReadAll(proof)
	if err != nil || len(body) != 0 {
		t.Fatalf("exec proof %q: %v", body, err)
	}
	fd, err := receiveSessionChild(int(identity.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	_ = events.SetReadDeadline(time.Now().Add(20 * time.Second))
	body, err = io.ReadAll(events)
	if err != nil || string(body) != "native-wait-and-40-orphans-ok\n" {
		t.Fatalf("events %q: %v", body, err)
	}
	if err := liveSessionPID(fd); err != nil {
		t.Fatalf("runtime stopped before event EOF: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "exit"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	var status *exec.ExitError
	if !errors.As(err, &status) || status.ExitCode() != 42 {
		t.Fatalf("managed status: %v", err)
	}
	if err := liveSessionPID(fd); err == nil {
		t.Fatal("dead managed child is live")
	}
}

func TestSessionInitExecFailureDoesNotProveReadiness(t *testing.T) {
	if os.Getenv("HELMR_PRIVILEGED_PROGRAM_TEST") != "1" {
		t.Skip("requires disposable privileged Linux namespace")
	}
	if os.Geteuid() != 0 {
		t.Fatal("Session init test requires root")
	}
	cmd, proof, _, identity := startSessionInitTest(t, t.TempDir(), true)
	body, err := io.ReadAll(proof)
	if err != nil || !strings.Contains(string(body), "exec Session runtime") {
		t.Fatalf("proof %q: %v", body, err)
	}
	if fd, err := receiveSessionChild(int(identity.Fd())); err == nil {
		unix.Close(fd)
		t.Fatal("failed exec supplied identity")
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("failed exec succeeded")
	}
}

func startSessionInitTest(t *testing.T, dir string, fail bool) (*exec.Cmd, *os.File, *os.File, *os.File) {
	t.Helper()
	events, eventWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	proof, proofWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	identity := os.NewFile(uintptr(pair[0]), "identity")
	sender := os.NewFile(uintptr(pair[1]), "sender")
	cmd := exec.Command(os.Args[0], "-test.run=^TestSessionInitHelper$")
	cmd.Env = append(os.Environ(), "HELMR_SESSION_INIT_TEST=init", "HELMR_SESSION_INIT_DIR="+dir)
	if fail {
		cmd.Env = append(cmd.Env, "HELMR_SESSION_INIT_FAIL=1")
	}
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{eventWriter, proofWriter, sender}
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWPID | unix.CLONE_NEWNS}
	err = cmd.Start()
	closeAgentFiles(eventWriter, proofWriter, sender)
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		closeAgentFiles(events, proof, identity)
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = proof.SetReadDeadline(time.Now().Add(10 * time.Second))
	return cmd, proof, events, identity
}

func TestSessionChildIdentityRejectsMissingAndExtraRights(t *testing.T) {
	for _, count := range []int{0, 1, 3} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(pair[0])
			defer unix.Close(pair[1])
			if _, err := receiveSessionChild(pair[0]); err == nil {
				t.Fatal("missing message accepted")
			}
			source, err := unix.PidfdOpen(os.Getpid(), 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(source)
			rights := make([]int, count)
			for i := range rights {
				rights[i] = source
			}
			control := []byte(nil)
			if count > 0 {
				control = unix.UnixRights(rights...)
			}
			if err := unix.Sendmsg(pair[1], []byte{1}, control, nil, 0); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadDir("/proc/self/fd")
			if err != nil {
				t.Fatal(err)
			}
			fd, err := receiveSessionChild(pair[0])
			if count == 1 {
				if err != nil {
					t.Fatal(err)
				}
				flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
				if err != nil || flags&unix.FD_CLOEXEC == 0 {
					t.Fatalf("identity fd flags %d: %v", flags, err)
				}
				_ = unix.Close(fd)
			} else if err == nil {
				_ = unix.Close(fd)
				t.Fatal("invalid rights accepted")
			}
			after, err := os.ReadDir("/proc/self/fd")
			if err != nil {
				t.Fatal(err)
			}
			if len(before) != len(after) {
				t.Fatal("received descriptors leaked")
			}
		})
	}
}

func TestSessionInitForwardsTerminationAndDetectsRuntimeDeath(t *testing.T) {
	if os.Getenv("HELMR_PRIVILEGED_PROGRAM_TEST") != "1" {
		t.Skip("requires disposable privileged Linux namespace")
	}
	if os.Geteuid() != 0 {
		t.Fatal("Session init test requires root")
	}
	for _, runtimeDeath := range []bool{false, true} {
		t.Run(strconv.FormatBool(runtimeDeath), func(t *testing.T) {
			cmd, proof, events, identity := startSessionInitTest(t, t.TempDir(), false)
			body, err := io.ReadAll(proof)
			if err != nil || len(body) != 0 {
				t.Fatalf("proof %q: %v", body, err)
			}
			fd, err := receiveSessionChild(int(identity.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			_ = events.SetReadDeadline(time.Now().Add(20 * time.Second))
			body, err = io.ReadAll(events)
			if err != nil || len(body) == 0 {
				t.Fatalf("events %q: %v", body, err)
			}
			want := 143
			if runtimeDeath {
				err = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
				want = 137
			} else {
				err = cmd.Process.Signal(syscall.SIGTERM)
			}
			if err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			var status *exec.ExitError
			if !errors.As(err, &status) || status.ExitCode() != want {
				t.Fatalf("status %v, want %d", err, want)
			}
			if err := liveSessionPID(fd); err == nil {
				t.Fatal("dead managed runtime remains live")
			}
		})
	}
}

func TestSessionRuntimeDeathRejectsLiveSupervisor(t *testing.T) {
	if os.Getenv("HELMR_PRIVILEGED_PROGRAM_TEST") != "1" {
		t.Skip("requires disposable privileged Linux namespace")
	}
	if os.Geteuid() != 0 {
		t.Fatal("Session init test requires root")
	}
	cmd, proof, events, identity := startSessionInitTest(t, t.TempDir(), false)
	body, err := io.ReadAll(proof)
	if err != nil || len(body) != 0 {
		t.Fatalf("proof %q: %v", body, err)
	}
	managedFD, err := receiveSessionChild(int(identity.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(managedFD)
	_ = events.SetReadDeadline(time.Now().Add(20 * time.Second))
	body, err = io.ReadAll(events)
	if err != nil || len(body) == 0 {
		t.Fatalf("events %q: %v", body, err)
	}
	rootFD, err := unix.PidfdOpen(cmd.Process.Pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(rootFD)
	// Hold the supervisor alive after application death, modeling the window
	// before its wait loop can observe the exit (including a frozen capture).
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	if err := unix.PidfdSendSignal(managedFD, unix.SIGKILL, nil, 0); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for liveSessionPID(managedFD) == nil {
		if time.Now().After(deadline) {
			t.Fatal("managed process did not exit")
		}
		time.Sleep(time.Millisecond)
	}
	if err := liveSessionPID(rootFD); err != nil {
		t.Fatal("supervisor exited before liveness check")
	}
	process := &agentProcess{started: true, managedFD: managedFD, rootFD: rootFD}
	if err := process.verifyRunning(); err == nil {
		t.Fatal("dead managed runtime accepted")
	}
	if err := process.verifyFrozen(); !errors.Is(err, errComputerCaptureUnusable) {
		t.Fatalf("dead managed capture accepted: %v", err)
	}
}
