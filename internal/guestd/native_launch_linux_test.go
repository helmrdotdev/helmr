//go:build linux

package guestd

import (
	"bufio"
	"context"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func init() {
	if len(os.Args) > 1 && os.Args[1] == "__helmr-native-test-background" {
		for {
			time.Sleep(time.Hour)
		}
	}
	if len(os.Args) > 1 && os.Args[1] == "__helmr-native-test-child" {
		if err := os.WriteFile("/workspace/native-marker", []byte("shared"), 0600); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		_, _ = fmt.Fprintf(os.Stdout, "pid=%d\n", os.Getpid())
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			if scanner.Text() == "spawn" {
				executable, err := os.Executable()
				if err != nil {
					os.Exit(1)
				}
				child := exec.Command(executable, "__helmr-native-test-background")
				child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
				if err := child.Start(); err != nil {
					_, _ = fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
				_, _ = fmt.Fprintf(os.Stdout, "child=%d\n", child.Process.Pid)
			} else {
				_, _ = fmt.Fprintln(os.Stdout, scanner.Text())
			}
		}
		os.Exit(0)
	}
}

func TestNativeLauncherRunsNonRootThroughPrivateProxy(t *testing.T) { testNativeLaunch(t, "stop") }
func TestNativeLauncherProxyDeathStopsDescendants(t *testing.T)     { testNativeLaunch(t, "death") }
func TestNativeLauncherInitFailureDoesNotPoisonConvergence(t *testing.T) {
	testNativeLaunch(t, "init_failure")
}
func TestNativeLauncherCleanExitHasStoppedEvidence(t *testing.T) { testNativeLaunch(t, "clean") }
func testNativeLaunch(t *testing.T, mode string) {
	t.Helper()
	if os.Getenv("HELMR_PRIVILEGED_PROGRAM_TEST") != "1" {
		t.Skip("requires a disposable privileged Linux guest")
	}
	if os.Geteuid() != 0 {
		t.Fatal("native launch test requires root")
	}
	if _, err := os.Stat(processCgroupRoot); errors.Is(err, os.ErrNotExist) {
		prepareProgramTestCgroup(t)
	} else if err != nil {
		t.Fatal(err)
	}
	resolver, resolverErr := os.OpenFile(imageRuntimeResolverPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if resolverErr == nil {
		_, writeErr := resolver.WriteString("nameserver 127.0.0.1\n")
		if err := errors.Join(writeErr, resolver.Close()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(imageRuntimeResolverPath) })
	} else if !errors.Is(resolverErr, os.ErrExist) {
		t.Fatal(resolverErr)
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"workspace", "tmp", "etc", "opt", "run"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0777); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "workspace"), 0777); err != nil {
		t.Fatal(err)
	}
	copyNativeTestLibraries(t, root)
	for _, dir := range []string{"/var/lib/helmr/program/runtime", "/var/lib/helmr/program/artifact"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Open("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	defer executable.Close()
	artifact := "/var/lib/helmr/program/artifact/native.test"
	destination, err := os.OpenFile(artifact, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(destination, executable)
	if err := errors.Join(copyErr, destination.Close()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(artifact) })
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
	peerLeaf, err := programCgroupLeafName(t.Name()+"-peer", 1, "lease")
	if err != nil {
		t.Fatal(err)
	}
	peerGroup, err := createProcessCgroup(peerLeaf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peerGroup.kill(); _ = peerGroup.waitEmpty(); _ = peerGroup.close() })
	peer := exec.Command("/bin/sleep", "30")
	peer.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := peerGroup.attach(peer); err != nil {
		t.Fatal(err)
	}
	if err := peer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Process.Kill(); _ = peer.Wait() })
	sessionRoot := exec.Command("/bin/sleep", "30")
	sessionFD := -1
	sessionRoot.SysProcAttr = &syscall.SysProcAttr{PidFD: &sessionFD}
	if err := parent.attach(sessionRoot); err != nil {
		t.Fatal(err)
	}
	if err := sessionRoot.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessionRoot.Process.Kill(); _ = sessionRoot.Wait(); _ = unix.Close(sessionFD) })
	user := &resolvedRuntimeUser{UID: 1001, GID: 1001}
	launcher, err := newNativeLauncher(parent, root, user, "", bootProgramMounts())
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
	serving := make(chan error, 1)
	go func() { serving <- launcher.run(t.Context()) }()
	nativeCommand := "/opt/helmr/program/native.test"
	if mode == "init_failure" {
		if err := os.WriteFile(filepath.Join(root, "workspace", "missing-interpreter"), []byte("#!/missing-interpreter\n"), 0755); err != nil {
			t.Fatal(err)
		}
		nativeCommand = "/workspace/missing-interpreter"
	}
	proxy, err := imageCommand(t.Context(), nativeLauncherExecutable, []string{nativeProxyArg, nativeCommand, "__helmr-native-test-child"}, "/workspace", []string{"PATH=/bin:/usr/bin"}, root, user, imageCommandOptions{Program: bootProgramMounts(), NativeRoot: launcher.directory, CgroupNamespace: true, CgroupLeaf: leaf})
	if err != nil {
		t.Fatal(err)
	}
	metadata, metadataWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	defer metadataWriter.Close()
	proxy.ExtraFiles = []*os.File{metadataWriter}
	input, err := proxy.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := proxy.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	proxy.Stderr = os.Stderr
	if err := parent.attach(proxy); err != nil {
		t.Fatal(err)
	}
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Process.Kill(); _ = proxy.Wait() })
	_ = metadataWriter.Close()
	if err := metadata.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var identity nativeLaunchReply
	if mode == "init_failure" {
		if err := json.NewDecoder(metadata).Decode(&identity); err == nil {
			t.Fatal("failed init received a successful identity")
		}
		if err := proxy.Wait(); err == nil {
			t.Fatal("missing interpreter unexpectedly launched")
		}
		if err := launcher.setAdmission(false); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for launcher.verifyConverged(nil) != nil {
			if time.Now().After(deadline) {
				t.Fatal("failed native init poisoned convergence")
			}
			time.Sleep(time.Millisecond)
		}
		return
	}
	if err := json.NewDecoder(metadata).Decode(&identity); err != nil {
		t.Fatal(err)
	}
	if identity.ScopeID == "" || identity.ProcessID <= 0 || identity.ProcessID == proxy.Process.Pid {
		t.Fatalf("missing distinct native identity: %+v", identity)
	}
	scanner := bufio.NewScanner(output)
	if !scanner.Scan() || scanner.Text() != "pid=1" {
		t.Fatalf("native namespace output = %q, %v", scanner.Text(), scanner.Err())
	}
	if _, err := io.WriteString(input, "round trip\n"); err != nil {
		t.Fatal(err)
	}
	if !scanner.Scan() || scanner.Text() != "round trip" {
		t.Fatalf("native stdio = %q, %v", scanner.Text(), scanner.Err())
	}
	if body, err := os.ReadFile(filepath.Join(root, "workspace", "native-marker")); err != nil || strings.TrimSpace(string(body)) != "shared" {
		t.Fatalf("shared native disk = %q, %v", body, err)
	}
	if err := launcher.setAdmission(false); err != nil {
		t.Fatal(err)
	}
	if err := launcher.verifySessionConverged(sessionRoot.Process.Pid, sessionFD, []nativeScopeEvidence{{ScopeID: identity.ScopeID, State: "idle"}}); err != nil {
		t.Fatal(err)
	}
	if err := launcher.verifyConverged(nil); err == nil {
		t.Fatal("incomplete scope evidence was accepted")
	}
	if mode == "clean" {
		if err := input.Close(); err != nil {
			t.Fatal(err)
		}
		if err := proxy.Wait(); err != nil {
			t.Fatal(err)
		}
		launcher.mu.Lock()
		launch := launcher.launches[identity.ScopeID]
		launcher.mu.Unlock()
		select {
		case <-launch.done:
		case <-time.After(5 * time.Second):
			t.Fatal("clean exit was not joined")
		}
		if err := launcher.verifySessionConverged(sessionRoot.Process.Pid, sessionFD, []nativeScopeEvidence{{ScopeID: identity.ScopeID, State: "idle"}}); err == nil {
			t.Fatal("exited process accepted as reusable idle")
		}
		if err := launcher.verifySessionConverged(sessionRoot.Process.Pid, sessionFD, []nativeScopeEvidence{{ScopeID: identity.ScopeID, State: "stopped"}}); err != nil {
			t.Fatal(err)
		}
		// A connector that received a complete launch/exit exchange cannot keep
		// doing authored work under the stopped scope's proxy exemption.
		connector := exec.Command("/bin/sleep", "30")
		connectorFD := -1
		connector.SysProcAttr = &syscall.SysProcAttr{PidFD: &connectorFD}
		if err := parent.attach(connector); err != nil {
			t.Fatal(err)
		}
		if err := connector.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = connector.Process.Kill(); _ = connector.Wait(); _ = unix.Close(connectorFD) }()
		launcher.mu.Lock()
		priorPID, priorFD := launch.proxyPID, launch.proxyFD
		launch.proxyPID, launch.proxyFD = connector.Process.Pid, connectorFD
		launcher.mu.Unlock()
		defer func() { launcher.mu.Lock(); launch.proxyPID, launch.proxyFD = priorPID, priorFD; launcher.mu.Unlock() }()
		if err := launcher.verifySessionConverged(sessionRoot.Process.Pid, sessionFD, []nativeScopeEvidence{{ScopeID: identity.ScopeID, State: "stopped"}}); err == nil {
			t.Fatal("stopped scope exempted a live connector")
		}
		if err := peer.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatal(err)
		}
		launcher.mu.Lock()
		launch.convergenceErr = context.DeadlineExceeded
		launcher.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := launcher.stop(ctx); err != nil {
			t.Fatalf("empty scope did not recover from earlier timeout: %v", err)
		}
		return
	}
	if _, err := io.WriteString(input, "spawn\n"); err != nil {
		t.Fatal(err)
	}
	if !scanner.Scan() || !strings.HasPrefix(scanner.Text(), "child=") {
		t.Fatalf("native child output = %q", scanner.Text())
	}
	if err := launcher.verifySessionConverged(sessionRoot.Process.Pid, sessionFD, []nativeScopeEvidence{{ScopeID: identity.ScopeID, State: "idle"}}); err == nil {
		t.Fatal("unjoined native descendant was accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if mode == "death" {
		if err := proxy.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		if err := proxy.Wait(); err == nil {
			t.Fatal("proxy survived termination")
		}
		launcher.mu.Lock()
		launch := launcher.launches[identity.ScopeID]
		launcher.mu.Unlock()
		select {
		case <-launch.done:
		case <-ctx.Done():
			t.Fatal("native tree survived proxy death")
		}
		if err := launch.group.waitEmpty(); err != nil {
			t.Fatal(err)
		}
	}
	if err := launcher.stop(ctx); err != nil {
		t.Fatal(err)
	}
	if mode != "death" {
		if err := proxy.Wait(); err == nil {
			t.Fatal("proxy survived native stop")
		}
	}
	if err := peer.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("native stop affected another Session: %v", err)
	}
	if err := <-serving; err != nil {
		t.Fatal(err)
	}
}

// Race-instrumented test binaries use the platform loader. Production guestd
// remains static; this only supplies the test binary's real shared libraries.
func copyNativeTestLibraries(t *testing.T, root string) {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := elf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer binary.Close()
	dynamic := false
	for _, program := range binary.Progs {
		if program.Type == elf.PT_INTERP {
			dynamic = true
		}
	}
	if !dynamic {
		return
	}
	output, err := exec.Command("ldd", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range strings.Fields(string(output)) {
		if !filepath.IsAbs(field) {
			continue
		}
		destination := filepath.Join(root, field)
		if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
			t.Fatal(err)
		}
		source, err := os.Open(field)
		if err != nil {
			t.Fatal(err)
		}
		target, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			_ = source.Close()
			t.Fatal(err)
		}
		_, copyErr := io.Copy(target, source)
		if err := errors.Join(copyErr, source.Close(), target.Close()); err != nil {
			t.Fatal(err)
		}
	}
}
