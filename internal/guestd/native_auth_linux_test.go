//go:build linux

package guestd

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func init() {
	if len(os.Args) > 3 && os.Args[1] == "__helmr-native-socket-peer" {
		connection, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: os.Args[2], Net: "unixpacket"})
		if err != nil {
			os.Exit(1)
		}
		if os.Args[3] == "transfer" {
			file, err := connection.File()
			if err != nil {
				os.Exit(2)
			}
			if err := unix.Sendmsg(3, []byte("socket"), unix.UnixRights(int(file.Fd())), nil, 0); err != nil {
				os.Exit(3)
			}
			os.Exit(0)
		}
		_, _ = os.Stdout.WriteString("connected\n")
		if os.Args[3] == "bad_rights" || os.Args[3] == "truncated_rights" {
			reader, writer, err := os.Pipe()
			if err != nil {
				os.Exit(4)
			}
			count := 2
			if os.Args[3] == "truncated_rights" {
				count = 17
			}
			fds := make([]int, count)
			for i := range fds {
				fds[i] = int(writer.Fd())
			}
			if _, _, err := connection.WriteMsgUnix([]byte("{}"), unix.UnixRights(fds...), nil); err != nil {
				os.Exit(5)
			}
			_ = writer.Close()
			_ = reader.SetReadDeadline(time.Now().Add(5 * time.Second))
			var one [1]byte
			if _, err := reader.Read(one[:]); !errors.Is(err, io.EOF) {
				os.Exit(6)
			}
			os.Exit(0)
		}
		var data [1]byte
		_, _ = connection.Read(data[:])
		os.Exit(0)
	}
}

func nativeAuthFixture(t *testing.T) (*nativeLauncher, *linuxProcessCgroup) {
	t.Helper()
	if os.Getenv("HELMR_PRIVILEGED_PROGRAM_TEST") != "1" {
		t.Skip("requires a disposable privileged Linux guest")
	}
	if os.Geteuid() != 0 {
		t.Fatal("native authentication test requires root")
	}
	if _, err := os.Stat(processCgroupRoot); errors.Is(err, os.ErrNotExist) {
		prepareProgramTestCgroup(t)
	} else if err != nil {
		t.Fatal(err)
	}
	leaf, err := programCgroupLeafName(t.Name(), 1, "lease")
	if err != nil {
		t.Fatal(err)
	}
	group, err := createProcessCgroup(leaf)
	if err != nil {
		t.Fatal(err)
	}
	parent := group.(*linuxProcessCgroup)
	t.Cleanup(func() { _ = parent.kill(); _ = parent.waitEmpty(); _ = parent.close() })
	launcher, err := newNativeLauncher(parent, t.TempDir(), &resolvedRuntimeUser{UID: 1001, GID: 1001}, "", bootProgramMounts())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := launcher.close(ctx); err != nil {
			t.Error(err)
		}
	})
	if err := launcher.setAdmission(true); err != nil {
		t.Fatal(err)
	}
	return launcher, parent
}

func nativeSocketPeer(t *testing.T, launcher *nativeLauncher, group *linuxProcessCgroup, uid uint32, mode string, extra *os.File) *exec.Cmd {
	t.Helper()
	command := exec.Command(os.Args[0], "__helmr-native-socket-peer", filepath.Join(launcher.directory, "broker.sock"), mode)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: &syscall.Credential{Uid: uid, Gid: uid, Groups: []uint32{}}}
	if extra != nil {
		command.ExtraFiles = []*os.File{extra}
	}
	if err := group.attach(command); err != nil {
		t.Fatal(err)
	}
	ready, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	if mode != "transfer" {
		scanner := bufio.NewScanner(ready)
		if !scanner.Scan() || scanner.Text() != "connected" {
			t.Fatalf("peer failed to connect: %q", scanner.Text())
		}
	}
	return command
}

func TestNativeLauncherRejectsForeignScopeAndWrongUser(t *testing.T) {
	for _, mode := range []string{"foreign_scope", "wrong_user"} {
		t.Run(mode, func(t *testing.T) {
			launcher, parent := nativeAuthFixture(t)
			serving := make(chan error, 1)
			go func() { serving <- launcher.run(t.Context()) }()
			uid := uint32(1001)
			peerGroup := parent
			if mode == "wrong_user" {
				uid = 1002
			} else {
				leaf, err := programCgroupLeafName(t.Name()+"-peer", 1, "lease")
				if err != nil {
					t.Fatal(err)
				}
				group, err := createProcessCgroup(leaf)
				if err != nil {
					t.Fatal(err)
				}
				peerGroup = group.(*linuxProcessCgroup)
				defer func() { _ = peerGroup.kill(); _ = peerGroup.waitEmpty(); _ = peerGroup.close() }()
			}
			peer := nativeSocketPeer(t, launcher, peerGroup, uid, "wait", nil)
			if err := peer.Wait(); err != nil {
				t.Fatal(err)
			}
			launcher.mu.Lock()
			count := len(launcher.launches)
			launcher.mu.Unlock()
			if count != 0 {
				t.Fatal("unauthorized peer entered the launch registry")
			}
			if err := launcher.setAdmission(false); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				if err := launcher.verifyConverged(nil); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("rejected launch poisoned idle verification")
				}
				time.Sleep(time.Millisecond)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := launcher.stop(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-serving; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNativeLauncherRejectsExitedPeerWithTransferredSocket(t *testing.T) {
	launcher, parent := nativeAuthFixture(t)
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	serverFile := os.NewFile(uintptr(pair[0]), "peer transfer receiver")
	childFile := os.NewFile(uintptr(pair[1]), "peer transfer sender")
	defer childFile.Close()
	control, err := net.FileConn(serverFile)
	_ = serverFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if err := control.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	peer := nativeSocketPeer(t, launcher, parent, 1001, "transfer", childFile)
	data := make([]byte, 32)
	ancillary := make([]byte, unix.CmsgSpace(4))
	_, oob, _, _, err := control.(*net.UnixConn).ReadMsgUnix(data, ancillary)
	if err != nil {
		t.Fatal(err)
	}
	fds, err := nativeMessageDescriptors(ancillary[:oob])
	if err != nil || len(fds) != 1 {
		for _, fd := range fds {
			_ = unix.Close(fd)
		}
		t.Fatalf("transferred socket: %v, %v", fds, err)
	}
	retainedFile := os.NewFile(uintptr(fds[0]), "retained peer socket")
	retained, err := net.FileConn(retainedFile)
	_ = retainedFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer retained.Close()
	if err := peer.Wait(); err != nil {
		t.Fatal(err)
	}
	// The original connector has exited before accept/authentication. Its open
	// connection survives in this process, so EOF alone cannot attest the peer.
	serving := make(chan error, 1)
	go func() { serving <- launcher.run(t.Context()) }()
	if err := retained.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := retained.Read(data); !errors.Is(err, io.EOF) {
		t.Fatalf("exited peer was not rejected: %v", err)
	}
	launcher.mu.Lock()
	count := len(launcher.launches)
	launcher.mu.Unlock()
	if count != 0 {
		t.Fatal("exited peer entered the launch registry")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := launcher.stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-serving; err != nil {
		t.Fatal(err)
	}
}

func TestNativeLauncherStopJoinsRegisteredRequestBeforeStart(t *testing.T) {
	launcher, parent := nativeAuthFixture(t)
	serving := make(chan error, 1)
	go func() { serving <- launcher.run(t.Context()) }()
	peer := nativeSocketPeer(t, launcher, parent, 1001, "wait", nil)
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	var pending *nativeLaunch
	for pending == nil {
		launcher.mu.Lock()
		for _, launch := range launcher.launches {
			pending = launch
		}
		launcher.mu.Unlock()
		if pending != nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("peer was not registered")
		case <-ticker.C:
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := launcher.stop(ctx); err != nil {
		t.Fatal(err)
	}
	_ = peer.Wait()
	if pending.group != nil || pending.command != nil {
		t.Fatal("stopped pending request created a native process")
	}
	if err := launcher.setAdmission(true); err == nil {
		t.Fatal("stopped launcher reopened admission")
	}
	select {
	case <-pending.done:
	default:
		t.Fatal("pending request was not joined")
	}
	if err := <-serving; err != nil {
		t.Fatal(err)
	}
}

func TestNativeLauncherClosesRejectedDescriptorRights(t *testing.T) {
	for _, mode := range []string{"bad_rights", "truncated_rights"} {
		t.Run(mode, func(t *testing.T) {
			launcher, parent := nativeAuthFixture(t)
			serving := make(chan error, 1)
			go func() { serving <- launcher.run(t.Context()) }()
			peer := nativeSocketPeer(t, launcher, parent, 1001, mode, nil)
			// The helper observes EOF on a pipe only after every transferred write
			// descriptor has been closed, including those delivered with MSG_CTRUNC.
			if err := peer.Wait(); err != nil {
				t.Fatalf("rejected descriptor rights leaked: %v", err)
			}
			if err := launcher.setAdmission(false); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				if err := launcher.verifyConverged(nil); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("rejected launch poisoned idle verification")
				}
				time.Sleep(time.Millisecond)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := launcher.stop(ctx); err != nil {
				t.Fatal(err)
			}
			for _, launch := range launcher.launches {
				if launch.group != nil || launch.command != nil {
					t.Fatal("malformed request launched a process")
				}
			}
			if err := <-serving; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNativeLauncherBoundsPendingConnections(t *testing.T) {
	launcher, parent := nativeAuthFixture(t)
	serving := make(chan error, 1)
	go func() { serving <- launcher.run(t.Context()) }()
	for range nativeLaunchPendingLimit {
		nativeSocketPeer(t, launcher, parent, 1001, "wait", nil)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		launcher.mu.Lock()
		pending := launcher.pending
		launcher.mu.Unlock()
		if pending == nativeLaunchPendingLimit {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pending requests did not register")
		}
		time.Sleep(time.Millisecond)
	}
	overflow := nativeSocketPeer(t, launcher, parent, 1001, "wait", nil)
	if err := overflow.Wait(); err != nil {
		t.Fatal(err)
	}
	launcher.mu.Lock()
	count := len(launcher.launches)
	launcher.mu.Unlock()
	if count != nativeLaunchPendingLimit {
		t.Fatalf("pending registry size = %d", count)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := launcher.stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-serving; err != nil {
		t.Fatal(err)
	}
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	if len(launcher.launches) != 0 || launcher.pending != 0 {
		t.Fatal("pending launches leaked after stop")
	}
}

func TestNativeKillFallsBackToRetainedPID(t *testing.T) {
	child := exec.Command("/bin/sleep", "30")
	fd := -1
	child.SysProcAttr = &syscall.SysProcAttr{PidFD: &fd}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	defer child.Process.Kill()
	injected := errors.New("cgroup kill unavailable")
	if err := killNativeProcess(func() error { return injected }, fd); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("process survived fallback")
	}
	if err := liveNativePID(fd); err == nil {
		t.Fatal("pidfd remains live")
	}
}

func TestNativeLaunchPacketBoundFitsSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker.sock")
	listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if err := client.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := server.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	packet := make([]byte, nativeLaunchPacketLimit)
	if n, err := client.Write(packet); err != nil || n != len(packet) {
		t.Fatalf("bounded packet send: %d, %v", n, err)
	}
	if n, err := server.Read(packet); err != nil || n != len(packet) {
		t.Fatalf("bounded packet receive: %d, %v", n, err)
	}
}

func TestNativeLauncherSealCancelsPendingWithoutPoisoningConvergence(t *testing.T) {
	launcher, parent := nativeAuthFixture(t)
	serving := make(chan error, 1)
	go func() { serving <- launcher.run(t.Context()) }()
	peer := nativeSocketPeer(t, launcher, parent, 1001, "wait", nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		launcher.mu.Lock()
		count := launcher.pending
		launcher.mu.Unlock()
		if count == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pending launch missing")
		}
		time.Sleep(time.Millisecond)
	}
	if err := launcher.setAdmission(false); err != nil {
		t.Fatal(err)
	}
	if err := launcher.verifyConverged(nil); err != nil {
		t.Fatal(err)
	}
	if err := peer.Wait(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := launcher.stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-serving; err != nil {
		t.Fatal(err)
	}
}

func TestNativeLauncherSessionConvergenceRejectsUnregisteredWork(t *testing.T) {
	launcher, parent := nativeAuthFixture(t)
	start := func() (*exec.Cmd, int) {
		child := exec.Command("/bin/sleep", "30")
		fd := -1
		child.SysProcAttr = &syscall.SysProcAttr{PidFD: &fd}
		if err := parent.attach(child); err != nil {
			t.Fatal(err)
		}
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait(); _ = unix.Close(fd) })
		return child, fd
	}
	root, rootFD := start()
	if err := launcher.setAdmission(false); err != nil {
		t.Fatal(err)
	}
	if err := launcher.verifySessionConverged(root.Process.Pid, rootFD, nil); err != nil {
		t.Fatal(err)
	}
	unregistered, pendingFD := start()
	if err := launcher.verifySessionConverged(root.Process.Pid, rootFD, nil); err == nil {
		t.Fatal("unregistered authored child passed convergence")
	}
	// A sealed, never-started connector is still authored work even while
	// its canceled request remains in the launch map awaiting cleanup.
	launcher.mu.Lock()
	launcher.launches["pending"] = &nativeLaunch{proxyPID: unregistered.Process.Pid, proxyFD: pendingFD}
	launcher.mu.Unlock()
	defer func() { launcher.mu.Lock(); delete(launcher.launches, "pending"); launcher.mu.Unlock() }()
	if err := launcher.verifySessionConverged(root.Process.Pid, rootFD, nil); err == nil {
		t.Fatal("pending connector passed Session convergence")
	}
	launcher.mu.Lock()
	delete(launcher.launches, "pending")
	launcher.mu.Unlock()
	_ = unregistered.Process.Kill()
	_ = unregistered.Wait()
	if err := launcher.verifySessionConverged(root.Process.Pid, rootFD, nil); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkdirat(int(parent.file.Fd()), "unregistered", 0755); err != nil {
		t.Fatal(err)
	}
	if err := launcher.verifySessionConverged(root.Process.Pid, rootFD, nil); err == nil {
		t.Fatal("unregistered cgroup passed convergence")
	}
	if err := unix.Unlinkat(int(parent.file.Fd()), "unregistered", unix.AT_REMOVEDIR); err != nil {
		t.Fatal(err)
	}
	_ = root.Process.Kill()
	_ = root.Wait()
	if err := launcher.verifySessionConverged(root.Process.Pid, rootFD, nil); err == nil {
		t.Fatal("lost runtime root passed convergence")
	}
}
