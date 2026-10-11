//go:build linux

package guestd

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// A launcher belongs to one verified Session process epoch. Its owning Session
// retains the image and secret mounts until close has joined every launch.
// Admission starts closed and is controlled by the Session's processing boundary.
// No authority, image root, user or cgroup path comes from the proxy request.
const nativeLaunchPendingLimit = 16
const nativeLaunchScopeLimit = 256

type nativeLauncher struct {
	program    programMounts
	parent     *linuxProcessCgroup
	imageRoot  string
	user       resolvedRuntimeUser
	secretRoot string
	directory  string
	listener   *net.UnixListener
	mu         sync.Mutex
	admission  bool
	pending    int
	stopped    bool
	launches   map[string]*nativeLaunch
}

type nativeLaunch struct {
	connection     *net.UnixConn
	proxyPID       int
	proxyFD        int
	scopeID        string
	done           chan struct{}
	group          *linuxProcessCgroup
	command        *exec.Cmd
	rootFD         int
	cancel         context.CancelFunc
	reported       bool
	started        bool
	convergenceErr error
}

func newNativeLauncher(parent *linuxProcessCgroup, imageRoot string, user *resolvedRuntimeUser, secretRoot string, program programMounts) (*nativeLauncher, error) {
	if parent == nil || parent.file == nil || user == nil || imageRoot == "" {
		return nil, errors.New("native launcher requires a Session scope, image and user")
	}
	directory, err := os.MkdirTemp("", "helmr-native-")
	if err != nil {
		return nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(directory)
		}
	}()
	if err := os.Chmod(directory, 0755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(directory, "launch"), nil, 0555); err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: filepath.Join(directory, "broker.sock"), Net: "unixpacket"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Join(directory, "broker.sock"), 0666); err != nil {
		_ = listener.Close()
		return nil, err
	}
	cleanup = false
	return &nativeLauncher{parent: parent, imageRoot: imageRoot, user: *user, secretRoot: secretRoot, program: program, directory: directory, listener: listener, launches: make(map[string]*nativeLaunch)}, nil
}

func (launcher *nativeLauncher) setAdmission(open bool) error {
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	if launcher.stopped {
		return errors.New("native launcher has stopped")
	}
	launcher.admission = open
	if !open {
		for _, launch := range launcher.launches {
			if !launch.started {
				launch.cancel()
				_ = launch.connection.Close()
			}
		}
	}
	return nil
}

func (launcher *nativeLauncher) run(ctx context.Context) error {
	stopAccept := context.AfterFunc(ctx, func() { _ = launcher.listener.Close() })
	defer stopAccept()
	defer func() {
		launcher.mu.Lock()
		launcher.admission = false
		launcher.stopped = true
		launcher.mu.Unlock()
	}()
	for {
		connection, err := launcher.listener.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		peer, err := launcher.authenticate(connection)
		if err != nil {
			_ = connection.Close()
			continue
		}
		launchCtx, cancel := context.WithCancel(ctx)
		launch := &nativeLaunch{connection: connection, proxyFD: peer.fd, proxyPID: peer.pid, rootFD: -1, scopeID: rand.Text(), done: make(chan struct{}), cancel: cancel}
		launcher.mu.Lock()
		if !launcher.admission || launcher.stopped || launcher.pending >= nativeLaunchPendingLimit || len(launcher.launches) >= nativeLaunchScopeLimit {
			launcher.mu.Unlock()
			cancel()
			_ = unix.Close(peer.fd)
			_ = connection.Close()
			continue
		}
		// Register before reading transferred descriptors or starting any child.
		launcher.launches[launch.scopeID] = launch
		launcher.pending++
		launcher.mu.Unlock()
		go launcher.serve(launchCtx, launch)
	}
}

type nativeProxyIdentity struct {
	fd  int
	pid int
}

func (launcher *nativeLauncher) authenticate(connection *net.UnixConn) (*nativeProxyIdentity, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return nil, err
	}
	var credential *unix.Ucred
	var controlErr error
	pidFD := -1
	err = raw.Control(func(fd uintptr) {
		credential, controlErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if controlErr == nil {
			pidFD, controlErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PEERPIDFD)
		}
	})
	if err != nil || controlErr != nil {
		if pidFD >= 0 {
			_ = unix.Close(pidFD)
		}
		return nil, errors.Join(err, controlErr)
	}
	valid := false
	defer func() {
		if !valid {
			_ = unix.Close(pidFD)
		}
	}()
	unix.CloseOnExec(pidFD)
	if credential.Uid != launcher.user.UID || credential.Pid <= 0 {
		return nil, errors.New("native proxy user does not match Session")
	}
	if err := liveNativePID(pidFD); err != nil {
		return nil, err
	}
	body, err := os.ReadFile("/proc/" + strconv.Itoa(int(credential.Pid)) + "/cgroup")
	if err != nil {
		return nil, err
	}
	expected := "0::" + strings.TrimPrefix(launcher.parent.path, "/sys/fs/cgroup")
	if strings.TrimSpace(string(body)) != expected {
		return nil, errors.New("native proxy is outside its Session scope")
	}
	// The socket's pidfd prevents a reused numeric PID from authenticating a peer
	// that exited while its current cgroup was being observed.
	if err := liveNativePID(pidFD); err != nil {
		return nil, err
	}
	valid = true
	return &nativeProxyIdentity{fd: pidFD, pid: int(credential.Pid)}, nil
}

func liveNativePID(fd int) error {
	if fd < 0 {
		return errors.New("native process identity is missing")
	}
	descriptors := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if _, err := unix.Poll(descriptors, 0); err != nil {
		return err
	}
	if descriptors[0].Revents != 0 {
		return errors.New("native process has exited")
	}
	return nil
}

func (launcher *nativeLauncher) serve(ctx context.Context, launch *nativeLaunch) {
	var failure error
	var convergenceErr error
	var watcherDone chan struct{}
	defer func() {
		launch.cancel()
		if failure != nil {
			_ = launch.connection.SetWriteDeadline(time.Now().Add(time.Second))
			_ = sendNativeLaunchReply(launch.connection, nativeLaunchReply{Error: "native launch failed"})
		}
		_ = launch.connection.Close()
		if watcherDone != nil {
			<-watcherDone
		}
		// Do not close stable identities until stop has finished using them under mu.
		launcher.mu.Lock()
		if !launch.started {
			launcher.pending--
		}
		if !launch.started || (!launch.reported && convergenceErr == nil) {
			if launch.group != nil {
				convergenceErr = launch.group.close()
			}
			if convergenceErr == nil {
				_ = unix.Close(launch.proxyFD)
				launch.proxyFD = -1
				if launch.rootFD >= 0 {
					_ = unix.Close(launch.rootFD)
					launch.rootFD = -1
				}
				delete(launcher.launches, launch.scopeID)
			}
		}
		launch.convergenceErr = convergenceErr
		close(launch.done)
		launcher.mu.Unlock()
	}()
	if err := launch.connection.SetDeadline(time.Now().Add(nativeLaunchTimeout)); err != nil {
		failure = err
		return
	}
	data := make([]byte, nativeLaunchPacketLimit)
	ancillary := make([]byte, unix.CmsgSpace(16*4))
	n, oob, flags, _, err := launch.connection.ReadMsgUnix(data, ancillary)
	descriptors, rightsErr := nativeMessageDescriptors(ancillary[:oob])
	defer func() {
		for _, fd := range descriptors {
			_ = unix.Close(fd)
		}
	}()
	if err != nil || rightsErr != nil || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || len(descriptors) != 3 {
		failure = errors.New("invalid native launch descriptors")
		return
	}
	var request nativeLaunchRequest
	decoder := json.NewDecoder(bytes.NewReader(data[:n]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		failure = err
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		failure = errors.New("native launch has trailing data")
		return
	}
	if !filepath.IsAbs(request.Command) || !filepath.IsAbs(request.Cwd) {
		failure = errors.New("native command and cwd must be absolute")
		return
	}
	proofReader, proofWriter, err := os.Pipe()
	if err != nil {
		failure = err
		return
	}
	defer proofReader.Close()
	defer proofWriter.Close()
	filler, err := os.Open(os.DevNull)
	if err != nil {
		failure = err
		return
	}
	defer filler.Close()
	// Starting and sealing admission are serialized. A pending launch cannot
	// create native work after stop has taken its registry snapshot.
	launcher.mu.Lock()
	if !launcher.admission || launcher.stopped {
		launcher.mu.Unlock()
		failure = errors.New("native launch admission is closed")
		return
	}
	group, err := createNativeProcessCgroup(launcher.parent, launch.scopeID)
	if err != nil {
		launcher.mu.Unlock()
		failure = err
		return
	}
	launch.group = group
	assignment, err := filepath.Rel(processCgroupRoot, group.path)
	if err != nil {
		launcher.mu.Unlock()
		failure = err
		return
	}
	command, err := imageCommand(ctx, request.Command, request.Args, request.Cwd, request.Env, launcher.imageRoot, &launcher.user, imageCommandOptions{Program: launcher.program, SecretRoot: launcher.secretRoot, CgroupNamespace: true, CgroupLeaf: assignment, StartProof: true})
	if err != nil {
		launcher.mu.Unlock()
		failure = err
		return
	}
	for index, descriptor := range descriptors {
		file := os.NewFile(uintptr(descriptor), "native stdio")
		defer file.Close()
		switch index {
		case 0:
			command.Stdin = file
		case 1:
			command.Stdout = file
		case 2:
			command.Stderr = file
		}
	}
	descriptors = nil // Ownership moved to the *os.File values above.
	command.ExtraFiles = []*os.File{filler, proofWriter}
	command.SysProcAttr.PidFD = &launch.rootFD
	command.Cancel = func() error { return killNativeProcess(group.kill, launch.rootFD) }
	if err := group.attach(command); err != nil {
		launcher.mu.Unlock()
		failure = err
		return
	}
	launch.command = command
	err = command.Start()
	if err == nil {
		launch.started = true
		launcher.pending--
	}
	launcher.mu.Unlock()
	if err != nil {
		failure = err
		return
	}
	_ = proofWriter.Close()
	// Connection loss independently stops the process; it does not wait for a
	// cooperative harness or readable output. The owner still joins group emptiness.
	watcherDone = make(chan struct{})
	go func() {
		defer close(watcherDone)
		var one [1]byte
		_, _ = launch.connection.Read(one[:])
		launch.cancel()
	}()
	if err := proofReader.SetReadDeadline(time.Now().Add(nativeLaunchTimeout)); err != nil {
		failure = err
		launch.cancel()
	}
	if failure == nil {
		proof, err := io.ReadAll(io.LimitReader(proofReader, 64*1024+1))
		if err != nil || len(proof) != 0 {
			failure = errors.New("native process setup did not complete")
			launch.cancel()
		}
	}
	if failure == nil {
		launcher.mu.Lock()
		if err := sendNativeLaunchReply(launch.connection, nativeLaunchReply{ScopeID: launch.scopeID, ProcessID: command.Process.Pid}); err != nil {
			failure = err
			launch.cancel()
		} else {
			launch.reported = true
		}
		launcher.mu.Unlock()
	}
	_ = launch.connection.SetDeadline(time.Time{})
	waitErr := command.Wait()
	// Exit of the direct child is insufficient when descendants retain streams.
	// Kill and join the whole native group before reporting process completion.
	killErr := group.kill()
	emptyErr := group.waitEmpty()
	convergenceErr = errors.Join(killErr, emptyErr)
	failure = errors.Join(failure, convergenceErr)
	code := command.ProcessState.ExitCode()
	if code < 0 {
		code = 128 + int(command.ProcessState.Sys().(syscall.WaitStatus).Signal())
	}
	if failure == nil {
		_ = waitErr // A nonzero native exit is carried by the exit receipt.
		failure = sendNativeLaunchReply(launch.connection, nativeLaunchReply{ExitCode: &code})
	}
}

func sendNativeLaunchReply(connection *net.UnixConn, reply nativeLaunchReply) error {
	body, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	n, err := connection.Write(body)
	if err == nil && n != len(body) {
		return io.ErrShortWrite
	}
	return err
}

func (launcher *nativeLauncher) stop(ctx context.Context) error {
	launcher.mu.Lock()
	launcher.admission = false
	launcher.stopped = true
	_ = launcher.listener.Close()
	launches := make([]*nativeLaunch, 0, len(launcher.launches))
	var failure error
	for _, launch := range launcher.launches {
		launches = append(launches, launch)
		launch.cancel()
		_ = launch.connection.Close()
		if err := signalNativeProcess(launch.proxyFD); err != nil {
			failure = errors.Join(failure, err)
		}
	}
	launcher.mu.Unlock()
	for _, launch := range launches {
		select {
		case <-ctx.Done():
			return errors.Join(failure, ctx.Err())
		case <-launch.done:
		}
		launcher.mu.Lock()
		if launch.convergenceErr != nil && launch.group != nil && launch.group.file != nil {
			// Retry the retained identity after a transient kill or join failure.
			_, populated, stateErr := launch.group.state()
			launch.convergenceErr = stateErr
			if stateErr == nil && populated {
				killErr := killNativeProcess(launch.group.kill, launch.rootFD)
				emptyErr := launch.group.waitEmptyContext(ctx)
				if emptyErr != nil {
					launch.convergenceErr = errors.Join(killErr, emptyErr)
				}
			}
		}
		failure = errors.Join(failure, launch.convergenceErr)
		launcher.mu.Unlock()
	}
	return failure
}

func (launcher *nativeLauncher) close(ctx context.Context) error {
	if err := launcher.stop(ctx); err != nil {
		return err
	}
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	var failure error
	for id, launch := range launcher.launches {
		if launch.group != nil {
			if err := launch.group.close(); err != nil {
				failure = errors.Join(failure, err)
				continue
			}
		}
		if launch.rootFD >= 0 {
			failure = errors.Join(failure, unix.Close(launch.rootFD))
		}
		if launch.proxyFD >= 0 {
			failure = errors.Join(failure, unix.Close(launch.proxyFD))
		}
		delete(launcher.launches, id)
	}
	if failure != nil {
		return failure
	}
	return os.RemoveAll(launcher.directory)
}

func (launcher *nativeLauncher) verifyConverged(scopes []nativeScopeEvidence) error {
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	return launcher.verifyConvergedLocked(scopes)
}

func (launcher *nativeLauncher) verifyConvergedLocked(scopes []nativeScopeEvidence) error {
	if launcher.admission || launcher.stopped {
		return errors.New("native launch admission is not sealed for completion")
	}
	count := 0
	for _, launch := range launcher.launches {
		if launch.started {
			count++
		}
	}
	if len(scopes) != count {
		return errors.New("native idle evidence does not cover every launch")
	}
	seen := make(map[string]bool, len(scopes))
	for _, scope := range scopes {
		id := scope.ScopeID
		launch := launcher.launches[id]
		if seen[id] || launch == nil {
			return errors.New("native idle scope identity is invalid")
		}
		seen[id] = true
		if launch.group == nil || launch.command == nil || launch.command.Process == nil {
			return errors.New("native launch is not ready")
		}
		if scope.State == "stopped" {
			select {
			case <-launch.done:
			default:
				return errors.New("native process stop is not joined")
			}
			_, populated, err := launch.group.state()
			if err != nil {
				return err
			}
			if populated {
				return errors.New("stopped native scope remains populated")
			}
			continue
		}
		if scope.State != "idle" {
			return errors.New("invalid native convergence state")
		}
		select {
		case <-launch.done:
			return errors.New("native idle process has exited")
		default:
		}
		if err := liveNativePID(launch.proxyFD); err != nil {
			return err
		}
		if err := liveNativePID(launch.rootFD); err != nil {
			return err
		}
		roots, err := nativeScopeRootCount(int(launch.group.file.Fd()), launch.command.Process.Pid)
		if err != nil {
			return err
		}
		if roots != 1 {
			return errors.New("native scope no longer contains its registered root")
		}
		if err := liveNativePID(launch.rootFD); err != nil {
			return err
		}
	}
	return nil
}

func nativeScopeRootCount(directory int, rootPID int) (int, error) {
	fd, err := unix.Openat(directory, "cgroup.procs", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return 0, err
	}
	file := os.NewFile(uintptr(fd), "native cgroup processes")
	body, readErr := io.ReadAll(io.LimitReader(file, 64*1024+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return 0, err
	}
	if len(body) > 64*1024 {
		return 0, errors.New("native scope process list exceeds its bound")
	}
	count := 0
	for _, field := range bytes.Fields(body) {
		pid, err := strconv.Atoi(string(field))
		if err != nil {
			return 0, errors.New("native scope contains an invalid process identity")
		}
		if pid != rootPID {
			name, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
			return 0, fmt.Errorf("native scope contains an unjoined descendant (root PID %d, PID %d, name %q)", rootPID, pid, strings.TrimSpace(string(name)))
		}
		count++
	}
	scanFD, err := unix.Openat(directory, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return 0, err
	}
	scan := os.NewFile(uintptr(scanFD), "native cgroup directory")
	entries, scanErr := scan.ReadDir(-1)
	if err := errors.Join(scanErr, scan.Close()); err != nil {
		return 0, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		child, err := unix.Openat(directory, entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return 0, err
		}
		children, readErr := nativeScopeRootCount(child, rootPID)
		closeErr := unix.Close(child)
		if err := errors.Join(readErr, closeErr); err != nil {
			return 0, err
		}
		count += children
	}
	return count, nil
}

// The root is PID 1 inside its private PID namespace. Killing this exact pidfd
// also makes the kernel terminate its descendants if the cgroup control fails.
func killNativeProcess(killGroup func() error, rootFD int) error {
	if err := killGroup(); err != nil {
		if rootFD < 0 {
			return err
		}
		return errors.Join(err, signalNativeProcess(rootFD))
	}
	return nil
}

func signalNativeProcess(fd int) error {
	if fd < 0 {
		return nil
	}
	err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

// Native scopes are not the entire Session: an authored child launched without
// the qualified adapter must also be gone before a Turn can settle. The only
// persistent direct processes are the exact runtime root and live stdio proxies.
func (launcher *nativeLauncher) verifySessionConverged(rootPID, rootFD int, scopes []nativeScopeEvidence) error {
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	if err := launcher.verifyConvergedLocked(scopes); err != nil {
		return err
	}
	if err := liveNativePID(rootFD); err != nil {
		return err
	}
	allowed := map[int]int{rootPID: rootFD}
	idle := make(map[string]bool, len(scopes))
	for _, scope := range scopes {
		idle[scope.ScopeID] = scope.State == "idle"
	}
	expectedGroups := make(map[string]*linuxProcessCgroup)
	for _, launch := range launcher.launches {
		if launch.started && launch.reported && idle[launch.scopeID] {
			allowed[launch.proxyPID] = launch.proxyFD
		}
		if launch.group != nil && launch.group.file != nil {
			expectedGroups[filepath.Base(launch.group.path)] = launch.group
		}
	}
	fd, err := unix.Openat(int(launcher.parent.file.Fd()), "cgroup.procs", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), "Session processes")
	body, readErr := io.ReadAll(io.LimitReader(file, 64*1024+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return err
	}
	if len(body) > 64*1024 {
		return errors.New("session process list exceeds its bound")
	}
	foundRoot := false
	for _, field := range bytes.Fields(body) {
		pid, err := strconv.Atoi(string(field))
		if err != nil {
			return err
		}
		identity, ok := allowed[pid]
		if !ok {
			return errors.New("session contains unjoined authored work")
		}
		if err := liveNativePID(identity); err != nil {
			return err
		}
		if pid == rootPID {
			foundRoot = true
		}
	}
	if !foundRoot {
		return errors.New("session root no longer belongs to its scope")
	}
	scanFD, err := unix.Openat(int(launcher.parent.file.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	scan := os.NewFile(uintptr(scanFD), "Session cgroup")
	entries, readErr := scan.ReadDir(-1)
	if err := errors.Join(readErr, scan.Close()); err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		group := expectedGroups[entry.Name()]
		if group == nil {
			return errors.New("session contains an unregistered process scope")
		}
		namedFD, err := unix.Openat(int(launcher.parent.file.Fd()), entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		namedFile := os.NewFile(uintptr(namedFD), entry.Name())
		named, statErr := namedFile.Stat()
		if err := errors.Join(statErr, namedFile.Close()); err != nil {
			return err
		}
		owned, err := group.file.Stat()
		if err != nil {
			return err
		}
		if !os.SameFile(named, owned) {
			return errors.New("session native scope identity changed")
		}
	}
	return liveNativePID(rootFD)
}
