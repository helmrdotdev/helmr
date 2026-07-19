//go:build linux

package guestd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"golang.org/x/sys/unix"
)

const dependencyProcessTestHelper = "__helmr-dependency-process-test-helper"

func init() {
	if len(os.Args) < 3 || os.Args[1] != dependencyProcessTestHelper {
		return
	}
	if err := runDependencyProcessTestHelper(os.Args[2]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runDependencyProcessTestHelper(operation string) error {
	if os.Getuid() != 65532 || os.Getgid() != 65532 {
		return errors.New("helper identity is not 65532")
	}
	if cwd, err := os.Getwd(); err != nil || cwd != "/work" {
		return fmt.Errorf("helper cwd = %q: %w", cwd, err)
	}
	if _, err := os.Stat("/proc"); !errors.Is(err, os.ErrNotExist) {
		return errors.New("helper can reach /proc")
	}
	if _, err := os.Stat("/etc/os-release"); !errors.Is(err, os.ErrNotExist) {
		return errors.New("helper can reach the guest root")
	}
	device, err := os.Open("/dev/null")
	if err != nil {
		return fmt.Errorf("helper cannot open /dev/null: %w", err)
	}
	if err := device.Close(); err != nil {
		return err
	}
	if _, err := unix.FcntlInt(dependencyReadyFD, unix.F_GETFD, 0); !errors.Is(err, syscall.EBADF) {
		return errors.New("helper inherited the readiness descriptor")
	}
	if err := unix.Mount("none", "/tmp", "tmpfs", 0, ""); !errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("helper mount error = %v, want EPERM", err)
	}
	if fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0); !errors.Is(err, syscall.EPERM) {
		if err == nil {
			_ = unix.Close(fd)
		}
		return fmt.Errorf("helper vsock error = %v, want EPERM", err)
	}
	switch operation {
	case "probe":
		if err := os.WriteFile("/work/probe", []byte("ready"), 0o600); err != nil {
			return fmt.Errorf("write helper work file: %w", err)
		}
		_, err := os.Stdout.WriteString("1.2.3\n")
		return err
	case "handshake":
		raw, err := os.ReadFile("/work/probe")
		if err != nil || string(raw) != "ready" {
			return errors.New("helper did not observe probe work")
		}
		return nil
	case "wait":
		child := exec.Command(
			"/opt/helmr/manager/helper",
			dependencyProcessTestHelper,
			"hold",
		)
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			return fmt.Errorf("start helper descendant: %w", err)
		}
		time.Sleep(30 * time.Second)
		return nil
	case "hold":
		time.Sleep(30 * time.Second)
		return nil
	default:
		return fmt.Errorf("helper operation %q is invalid", operation)
	}
}

func TestDependencyOutputBoundsCombinedStreams(t *testing.T) {
	t.Parallel()

	output := &dependencyOutput{limit: 8}
	output.append([]byte("abcde"), true)
	output.append([]byte("fghij"), false)
	result := output.result(nil)
	if !result.overflow {
		t.Fatal("combined dependency output did not overflow")
	}
	if string(result.stdout) != "abcde" || string(result.stderr) != "fgh" {
		t.Fatalf(
			"dependency output = stdout %q stderr %q",
			result.stdout,
			result.stderr,
		)
	}
}

func TestDependencyOutputDrainRecordsReadFailure(t *testing.T) {
	t.Parallel()

	output := &dependencyOutput{limit: 8}
	output.drain(dependencyFailingReader{}, true)
	if output.readErr == nil {
		t.Fatal("dependency output read failure was ignored")
	}
}

type dependencyFailingReader struct{}

func (dependencyFailingReader) Read(destination []byte) (int, error) {
	copy(destination, "x")
	return 1, errors.New("read failed")
}

func TestValidateDependencyProbe(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		result dependencyCommandResult
		valid  bool
	}{
		{name: "exact", result: dependencyCommandResult{stdout: []byte("1.2.3")}, valid: true},
		{name: "newline", result: dependencyCommandResult{stdout: []byte("1.2.3\n")}, valid: true},
		{name: "carriage return", result: dependencyCommandResult{stdout: []byte("1.2.3\r\n")}},
		{name: "stderr allowed", result: dependencyCommandResult{stdout: []byte("1.2.3"), stderr: []byte("notice")}, valid: true},
		{name: "overflow", result: dependencyCommandResult{stdout: []byte("1.2.3"), overflow: true}},
		{name: "failed", result: dependencyCommandResult{stdout: []byte("1.2.3"), waitErr: errors.New("failed")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			failure := validateDependencyProbe(test.result, "1.2.3")
			if (failure == nil) != test.valid {
				t.Fatalf("validateDependencyProbe() failure = %#v, valid = %v", failure, test.valid)
			}
		})
	}
}

func TestParseDependencyCgroupPopulated(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		raw     string
		want    bool
		wantErr bool
	}{
		{name: "empty", raw: "populated 0\nfrozen 0\n"},
		{name: "populated", raw: "populated 1\nfrozen 0\n", want: true},
		{name: "missing", raw: "frozen 0\n", wantErr: true},
		{name: "duplicate", raw: "populated 0\npopulated 0\n", wantErr: true},
		{name: "invalid", raw: "populated 2\n", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseDependencyCgroupPopulated([]byte(test.raw))
			if (err != nil) != test.wantErr {
				t.Fatalf("parseDependencyCgroupPopulated() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("parseDependencyCgroupPopulated() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDependencySeccompPolicy(t *testing.T) {
	t.Parallel()

	filter, err := dependencySeccompFilter()
	if err != nil {
		t.Fatal(err)
	}
	architecture, err := dependencyAuditArchitecture()
	if err != nil {
		t.Fatal(err)
	}
	allow := uint32(unix.SECCOMP_RET_ALLOW)
	deny := uint32(unix.SECCOMP_RET_ERRNO | uint32(syscall.EPERM))
	noSys := uint32(unix.SECCOMP_RET_ERRNO | uint32(syscall.ENOSYS))
	kill := uint32(unix.SECCOMP_RET_KILL_PROCESS)
	namespace := uint64(unix.CLONE_NEWNET)

	for _, test := range []struct {
		name string
		data dependencySeccompData
		want uint32
	}{
		{name: "ordinary syscall", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_READ)}, want: allow},
		{name: "wrong architecture", data: dependencySeccompData{architecture: architecture ^ 1, number: uint32(unix.SYS_READ)}, want: kill},
		{name: "unix socket", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_SOCKET), arguments: [6]uint64{unix.AF_UNIX, unix.SOCK_STREAM}}, want: allow},
		{name: "vsock", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_SOCKET), arguments: [6]uint64{unix.AF_VSOCK, unix.SOCK_STREAM}}, want: deny},
		{name: "netlink", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_SOCKET), arguments: [6]uint64{unix.AF_NETLINK, unix.SOCK_DGRAM}}, want: deny},
		{name: "packet socket", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_SOCKET), arguments: [6]uint64{unix.AF_PACKET, unix.SOCK_DGRAM}}, want: deny},
		{name: "raw socket", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_SOCKET), arguments: [6]uint64{unix.AF_INET, unix.SOCK_RAW | unix.SOCK_CLOEXEC}}, want: deny},
		{name: "ordinary clone", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_CLONE), arguments: [6]uint64{uint64(syscall.SIGCHLD)}}, want: allow},
		{name: "namespaced clone", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_CLONE), arguments: [6]uint64{namespace}}, want: deny},
		{name: "clone3", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_CLONE3)}, want: noSys},
		{name: "ordinary unshare", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_UNSHARE)}, want: allow},
		{name: "namespaced unshare", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_UNSHARE), arguments: [6]uint64{namespace}}, want: deny},
		{name: "unspecified setns", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_SETNS)}, want: deny},
		{name: "network setns", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_SETNS), arguments: [6]uint64{0, namespace}}, want: deny},
		{name: "cgroup setns", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_SETNS), arguments: [6]uint64{0, unix.CLONE_NEWCGROUP}}, want: allow},
		{name: "mount", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_MOUNT)}, want: deny},
		{name: "open tree", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_OPEN_TREE)}, want: deny},
		{name: "mount setattr", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_MOUNT_SETATTR)}, want: deny},
		{name: "statmount", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_STATMOUNT)}, want: deny},
		{name: "listmount", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_LISTMOUNT)}, want: deny},
		{name: "open tree attr", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_OPEN_TREE_ATTR)}, want: deny},
		{name: "io uring setup", data: dependencySeccompData{architecture: architecture, number: uint32(unix.SYS_IO_URING_SETUP)}, want: deny},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := evaluateDependencySeccomp(filter, test.data)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("seccomp result = %#x, want %#x", got, test.want)
			}
		})
	}
	if runtime.GOARCH == "amd64" {
		got, err := evaluateDependencySeccomp(filter, dependencySeccompData{
			architecture: architecture,
			number:       uint32(unix.SYS_SOCKET) | dependencyX32SyscallBit,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != kill {
			t.Fatalf("x32 seccomp result = %#x, want %#x", got, kill)
		}
	}
}

func TestDependencyProcessBoundary(t *testing.T) {
	if os.Getenv("HELMR_PRIVILEGED_DEPENDENCY_TEST") != "1" {
		t.Skip("set HELMR_PRIVILEGED_DEPENDENCY_TEST=1 in a disposable privileged Linux guest")
	}
	if os.Geteuid() != 0 {
		t.Fatal("privileged dependency test requires root")
	}
	prepareDependencyTestCgroup(t)

	root := t.TempDir()
	manager := filepath.Join(root, "manager")
	runtimeRoot := filepath.Join(root, "runtime")
	toolchain := filepath.Join(root, "toolchain")
	for _, path := range []string{manager, runtimeRoot, toolchain} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	helper := filepath.Join(manager, "helper")
	source, err := os.ReadFile("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, source, 0o755); err != nil {
		t.Fatal(err)
	}

	plan := deploymentPlanForProcessTest()
	processRoot, err := prepareDependencyProcessRoot(plan)
	if err != nil {
		t.Fatal(err)
	}
	defer removeDependencyProcessRoot(processRoot)
	cgroupPath, cgroup, err := createDependencyCgroup(plan.Limits)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupDependencyCgroup(cgroupPath, cgroup, true)

	config := dependencyProcessConfig{
		Command: deployment.PlanCommand{
			Argv: []string{"/opt/helmr/manager/helper", dependencyProcessTestHelper, "probe"},
			CWD:  "/work",
		},
		Identity:    plan.Identity,
		Manager:     manager,
		ProcessRoot: processRoot,
		Runtime:     runtimeRoot,
		Toolchain:   toolchain,
	}
	probe, interrupted, err := runDependencyCommand(context.Background(), config, cgroup)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted || probe.waitErr != nil || probe.overflow ||
		string(probe.stdout) != "1.2.3\n" {
		t.Fatalf("probe result = %#v, interrupted = %v", probe, interrupted)
	}
	config.Command.Argv[2] = "handshake"
	handshake, interrupted, err := runDependencyCommand(
		context.Background(),
		config,
		cgroup,
	)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted || handshake.waitErr != nil || handshake.overflow ||
		len(handshake.stdout) != 0 || len(handshake.stderr) != 0 {
		t.Fatalf("handshake result = %#v, interrupted = %v", handshake, interrupted)
	}
	config.Command.Argv[2] = "wait"
	timeoutContext, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, interrupted, err = runDependencyCommand(timeoutContext, config, cgroup)
	if err != nil {
		t.Fatal(err)
	}
	if !interrupted {
		t.Fatal("dependency process deadline did not interrupt the complete subtree")
	}
}

func deploymentPlanForProcessTest() deployment.DependencyPlan {
	return deployment.DependencyPlan{
		Identity: deployment.PlanIdentity{UID: 65532, GID: 65532},
		Limits: deployment.PlanLimits{
			CPUPeriodMicros: 100000,
			CPUQuotaMicros:  200000,
			MemoryBytes:     2 << 30,
			PIDs:            512,
		},
	}
}

func prepareDependencyTestCgroup(t *testing.T) {
	t.Helper()
	for _, controller := range []string{"cpu", "memory", "pids"} {
		raw, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
		if err != nil || !bytes.Contains(raw, []byte(controller)) {
			t.Fatalf("cgroup controller %s is unavailable: %v", controller, err)
		}
	}
	bootstrap := "/sys/fs/cgroup/helmr-test-supervisor"
	if err := os.Mkdir(bootstrap, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(bootstrap, "cgroup.procs"),
		[]byte(fmt.Sprintf("%d", os.Getpid())),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		"/sys/fs/cgroup/cgroup.subtree_control",
		[]byte("+cpu +memory +pids"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dependencyCgroupRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dependencyCgroupRoot, "pids.max"),
		[]byte("1024"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dependencyCgroupRoot, "cgroup.subtree_control"),
		[]byte("+cpu +memory +pids"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	supervisor := filepath.Join(dependencyCgroupRoot, "supervisor")
	if err := os.Mkdir(supervisor, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(supervisor, "cgroup.procs"),
		[]byte(fmt.Sprintf("%d", os.Getpid())),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(bootstrap); err != nil {
		t.Fatal(err)
	}
}

type dependencySeccompData struct {
	architecture uint32
	arguments    [6]uint64
	number       uint32
}

func evaluateDependencySeccomp(
	filter []unix.SockFilter,
	data dependencySeccompData,
) (uint32, error) {
	raw := make([]byte, 64)
	byteOrder := binary.LittleEndian
	byteOrder.PutUint32(raw[0:4], data.number)
	byteOrder.PutUint32(raw[4:8], data.architecture)
	for index, argument := range data.arguments {
		byteOrder.PutUint64(raw[16+index*8:24+index*8], argument)
	}

	var accumulator uint32
	for counter := 0; counter < len(filter); counter++ {
		instruction := filter[counter]
		switch instruction.Code {
		case dependencyBPFLdWAbs:
			offset := int(instruction.K)
			if offset < 0 || offset+4 > len(raw) {
				return 0, io.ErrUnexpectedEOF
			}
			accumulator = byteOrder.Uint32(raw[offset : offset+4])
		case dependencyBPFAluAnd:
			accumulator &= instruction.K
		case dependencyBPFJmpJEQ:
			if accumulator == instruction.K {
				counter += int(instruction.Jt)
			} else {
				counter += int(instruction.Jf)
			}
		case dependencyBPFJmpJSet:
			if accumulator&instruction.K != 0 {
				counter += int(instruction.Jt)
			} else {
				counter += int(instruction.Jf)
			}
		case dependencyBPFRetK:
			return instruction.K, nil
		default:
			return 0, errors.New("unsupported test BPF instruction")
		}
	}
	return 0, errors.New("seccomp filter did not return")
}
