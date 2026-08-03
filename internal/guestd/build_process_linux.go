//go:build linux

package guestd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	buildProcessInitArg = "__helmr-build-init"
	buildCgroupRoot     = "/sys/fs/cgroup/helmr"
	buildCgroupLeaf     = "build"
	buildOutputLimit    = 64 << 10
	buildCleanupTimeout = 10 * time.Second
	buildOutputMarker   = "\n[helmr: build output truncated]\n"
)

type buildProcessConfig struct {
	Aliases     []buildAlias       `json:"aliases"`
	Command     buildCommand       `json:"command"`
	Environment []buildEnvironment `json:"environment"`
	Identity    buildIdentity      `json:"identity"`
	Manager     string             `json:"manager"`
	Output      string             `json:"output"`
	OutputLimit int                `json:"outputLimit"`
	ProcessRoot string             `json:"processRoot"`
	Project     string             `json:"project"`
	Runtime     string             `json:"runtime"`
	Supervisor  bool               `json:"supervisor"`
	Toolchain   string             `json:"toolchain"`
}

type buildAlias struct {
	Path   string `json:"path"`
	Target string `json:"target"`
}

type buildCommand struct {
	Argv []string `json:"argv"`
	CWD  string   `json:"cwd"`
}

type buildEnvironment struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type buildIdentity struct {
	UID int64 `json:"uid"`
	GID int64 `json:"gid"`
}

type buildLimits struct {
	CPUPeriodMicros int64
	CPUQuotaMicros  int64
	MemoryBytes     int64
	PIDs            int64
}

type buildProcessPlan struct {
	Aliases  []buildAlias
	Identity buildIdentity
}

type buildCommandResult struct {
	stderr     []byte
	stdout     []byte
	supervisor []byte
	overflow   bool
	waitErr    error
}

type buildReadyResult struct {
	err error
}

type buildOutput struct {
	limit    int
	mu       sync.Mutex
	overflow bool
	readErr  error
	stderr   bytes.Buffer
	stdout   bytes.Buffer
	total    int
}

func prepareBuildProcessRoot(plan buildProcessPlan) (string, error) {
	root, err := mkdirGuestdTemp("helmr-build-root-*")
	if err != nil {
		return "", fmt.Errorf("create build process root: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(root)
		}
	}()
	if err := os.Chmod(root, 0o755); err != nil {
		return "", fmt.Errorf("set build process root mode: %w", err)
	}

	directories := []struct {
		mode os.FileMode
		path string
	}{
		{0o755, "dev"},
		{0o755, "etc"},
		{0o755, "opt/helmr/manager"},
		{0o755, "opt/helmr/output"},
		{0o755, "opt/helmr/program"},
		{0o755, "opt/helmr/runtime"},
		{0o755, "nix"},
		{0o700, "work"},
		{0o700, "work/home"},
		{0o700, "work/output"},
		{0o700, "work/project"},
		{0o1777, "tmp"},
	}
	for _, alias := range plan.Aliases {
		directories = append(directories, struct {
			mode os.FileMode
			path string
		}{0o755, filepath.Dir(alias.Path)[1:]})
	}
	for _, directory := range directories {
		if directory.path == "." || directory.path == "" {
			continue
		}
		path := filepath.Join(root, directory.path)
		if err := os.MkdirAll(path, directory.mode); err != nil {
			return "", fmt.Errorf("create build process directory %q: %w", directory.path, err)
		}
		if err := os.Chmod(path, directory.mode); err != nil {
			return "", fmt.Errorf("set build process directory %q mode: %w", directory.path, err)
		}
	}
	for _, relative := range []string{"etc/resolv.conf", "etc/nsswitch.conf"} {
		file, err := os.OpenFile(
			filepath.Join(root, relative),
			os.O_CREATE|os.O_EXCL|os.O_WRONLY,
			0o644,
		)
		if err != nil {
			return "", fmt.Errorf("create build process file %q: %w", relative, err)
		}
		if err := file.Close(); err != nil {
			return "", fmt.Errorf("close build process file %q: %w", relative, err)
		}
	}
	for _, relative := range []string{
		"work",
		"work/home",
		"work/output",
		"work/project",
	} {
		if err := os.Chown(
			filepath.Join(root, relative),
			int(plan.Identity.UID),
			int(plan.Identity.GID),
		); err != nil {
			return "", fmt.Errorf("own build process %q: %w", relative, err)
		}
	}
	for _, device := range []runtimeDevice{
		{name: "null", major: 1, minor: 3, mode: 0o666},
		{name: "zero", major: 1, minor: 5, mode: 0o666},
		{name: "random", major: 1, minor: 8, mode: 0o666},
		{name: "urandom", major: 1, minor: 9, mode: 0o666},
	} {
		target := filepath.Join(root, "dev", device.name)
		mode := uint32(syscall.S_IFCHR) | device.mode
		if err := unix.Mknod(
			target,
			mode,
			int(unix.Mkdev(device.major, device.minor)),
		); err != nil {
			return "", fmt.Errorf("create build process /dev/%s: %w", device.name, err)
		}
		if err := os.Chmod(target, os.FileMode(device.mode)); err != nil {
			return "", fmt.Errorf("set build process /dev/%s mode: %w", device.name, err)
		}
	}
	for _, alias := range plan.Aliases {
		target := filepath.Join(root, alias.Path[1:])
		if err := os.Symlink(alias.Target, target); err != nil {
			return "", fmt.Errorf("create build process alias %q: %w", alias.Path, err)
		}
	}
	cleanup = false
	return root, nil
}

func removeBuildProcessRoot(root string) error {
	if root == "" {
		return nil
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove build process root: %w", err)
	}
	return nil
}

func runBuildCommand(
	ctx context.Context,
	config buildProcessConfig,
	cgroup *os.File,
) (result buildCommandResult, interrupted bool, returnErr error) {
	path, processCgroup, err := createBuildProcessCgroup(cgroup)
	if err != nil {
		return buildCommandResult{}, false, err
	}
	defer func() {
		returnErr = errors.Join(
			returnErr,
			cleanupBuildCgroup(path, processCgroup),
		)
	}()
	return runBuildCommandInCgroup(ctx, config, path, processCgroup)
}

func runBuildCommandInCgroup(
	ctx context.Context,
	config buildProcessConfig,
	cgroupPath string,
	cgroup *os.File,
) (buildCommandResult, bool, error) {
	raw, err := json.Marshal(config)
	if err != nil {
		return buildCommandResult{}, false, fmt.Errorf(
			"encode build process config: %w",
			err,
		)
	}
	argument := base64.RawURLEncoding.EncodeToString(raw)
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return buildCommandResult{}, false, fmt.Errorf(
			"create build process readiness pipe: %w",
			err,
		)
	}
	defer readyReader.Close()
	defer readyWriter.Close()
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		return buildCommandResult{}, false, fmt.Errorf(
			"create build process stdout pipe: %w",
			err,
		)
	}
	defer stdoutReader.Close()
	defer stdoutWriter.Close()
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		return buildCommandResult{}, false, fmt.Errorf(
			"create build process stderr pipe: %w",
			err,
		)
	}
	defer stderrReader.Close()
	defer stderrWriter.Close()
	var supervisorReader, supervisorWriter *os.File
	if config.Supervisor {
		supervisorReader, supervisorWriter, err = os.Pipe()
		if err != nil {
			return buildCommandResult{}, false, fmt.Errorf(
				"create build process supervisor pipe: %w",
				err,
			)
		}
		defer supervisorReader.Close()
		defer supervisorWriter.Close()
	}
	stdin, err := os.Open("/dev/null")
	if err != nil {
		return buildCommandResult{}, false, fmt.Errorf(
			"open build process stdin: %w",
			err,
		)
	}
	defer stdin.Close()

	pidFD := -1
	command := exec.Command("/proc/self/exe", buildProcessInitArg, argument)
	command.Dir = "/"
	command.Env = []string{}
	command.Stdin = stdin
	command.Stdout = stdoutWriter
	command.Stderr = stderrWriter
	command.ExtraFiles = []*os.File{readyWriter}
	if supervisorWriter != nil {
		command.ExtraFiles = append(command.ExtraFiles, supervisorWriter)
	}
	cloneFlags := uintptr(
		syscall.CLONE_NEWPID |
			syscall.CLONE_NEWIPC,
	)
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:   cloneFlags,
		Unshareflags: syscall.CLONE_NEWNS,
		UseCgroupFD:  true,
		CgroupFD:     int(cgroup.Fd()),
		PidFD:        &pidFD,
	}
	if err := command.Start(); err != nil {
		return buildCommandResult{}, false, fmt.Errorf(
			"start build process: %w",
			err,
		)
	}
	if pidFD < 0 {
		_ = killCgroup(cgroupPath)
		_ = command.Wait()
		return buildCommandResult{}, false, errors.New(
			"build process did not return a pidfd",
		)
	}
	defer unix.Close(pidFD)
	if err := errors.Join(
		readyWriter.Close(),
		stdoutWriter.Close(),
		stderrWriter.Close(),
		func() error {
			if supervisorWriter != nil {
				return supervisorWriter.Close()
			}
			return nil
		}(),
	); err != nil {
		_ = killCgroup(cgroupPath)
		_ = command.Wait()
		return buildCommandResult{}, false, fmt.Errorf(
			"close build process parent descriptors: %w",
			err,
		)
	}

	outputLimit := config.OutputLimit
	if outputLimit == 0 {
		outputLimit = buildOutputLimit
	}
	output := &buildOutput{limit: outputLimit}
	var outputReaders sync.WaitGroup
	outputReaders.Add(2)
	go func() {
		defer outputReaders.Done()
		output.drain(stdoutReader, true)
	}()
	supervisorResult := make(chan []byte, 1)
	if supervisorReader != nil {
		go func() {
			raw, readErr := io.ReadAll(io.LimitReader(supervisorReader, 70<<20+1))
			if readErr != nil {
				output.mu.Lock()
				output.readErr = errors.Join(output.readErr, readErr)
				output.mu.Unlock()
			}
			supervisorResult <- raw
		}()
	}
	go func() {
		defer outputReaders.Done()
		output.drain(stderrReader, false)
	}()

	ready := make(chan buildReadyResult, 1)
	go func() {
		var marker [1]byte
		_, readErr := io.ReadFull(readyReader, marker[:])
		if readErr == nil && marker[0] != 1 {
			readErr = errors.New("build process readiness marker is invalid")
		}
		if readErr == nil {
			var extra [1]byte
			count, extraErr := readyReader.Read(extra[:])
			if count != 0 || !errors.Is(extraErr, io.EOF) {
				readErr = errors.New("build process readiness stream is not exact")
			}
		}
		ready <- buildReadyResult{err: readErr}
	}()
	exited := make(chan error, 1)
	go func() {
		exited <- waitBuildPidfd(pidFD)
	}()
	waited := make(chan error, 1)
	go func() {
		waited <- command.Wait()
	}()

	readyObserved := false
	exitObserved := false
	var readinessErr error
	cancelled := false
	cancelChannel := ctx.Done()
	for !readyObserved || !exitObserved {
		select {
		case outcome := <-ready:
			readyObserved = true
			readinessErr = outcome.err
			if readinessErr != nil && !exitObserved {
				cancelled = true
				if err := killCgroup(cgroupPath); err != nil {
					return buildCommandResult{}, false, errors.Join(
						fmt.Errorf("build process bootstrap: %w", readinessErr),
						err,
					)
				}
			}
		case pollErr := <-exited:
			if pollErr != nil {
				return buildCommandResult{}, false, pollErr
			}
			exitObserved = true
			cancelChannel = nil
		case <-cancelChannel:
			pidExited, pollErr := buildPidfdReady(pidFD)
			if pollErr != nil {
				return buildCommandResult{}, false, pollErr
			}
			if pidExited {
				exitObserved = true
				cancelChannel = nil
				continue
			}
			cancelled = true
			cancelChannel = nil
			if err := killCgroup(cgroupPath); err != nil {
				return buildCommandResult{}, false, err
			}
		}
	}
	waitErr := <-waited
	if err := killCgroup(cgroupPath); err != nil {
		return buildCommandResult{}, false, err
	}
	if err := waitCgroupEmpty(cgroupPath); err != nil {
		return buildCommandResult{}, false, err
	}
	outputReaders.Wait()
	if output.readErr != nil {
		return buildCommandResult{}, false, output.readErr
	}
	if readinessErr != nil {
		diagnostic := output.result(waitErr).stderr
		if len(diagnostic) > 4096 {
			diagnostic = diagnostic[:4096]
		}
		return buildCommandResult{}, false, fmt.Errorf(
			"build process bootstrap: %w: wait=%v stderr=%q",
			readinessErr,
			waitErr,
			diagnostic,
		)
	}
	result := output.result(waitErr)
	if supervisorReader != nil {
		result.supervisor = <-supervisorResult
	}
	if cancelled {
		return result, true, nil
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			return buildCommandResult{}, false, fmt.Errorf(
				"wait for build process: %w",
				waitErr,
			)
		}
	}
	return result, false, nil
}

func createBuildProcessCgroup(parent *os.File) (string, *os.File, error) {
	const leaf = "process"
	if parent == nil {
		return "", nil, errors.New("build plan cgroup is nil")
	}
	if err := unix.Mkdirat(int(parent.Fd()), leaf, 0o755); err != nil {
		return "", nil, fmt.Errorf("create build process cgroup: %w", err)
	}
	fd, err := unix.Openat(
		int(parent.Fd()),
		leaf,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		_ = unix.Unlinkat(int(parent.Fd()), leaf, unix.AT_REMOVEDIR)
		return "", nil, fmt.Errorf("open build process cgroup: %w", err)
	}
	path := filepath.Join(parent.Name(), leaf)
	return path, os.NewFile(uintptr(fd), path), nil
}

func (output *buildOutput) drain(source io.Reader, stdout bool) {
	buffer := make([]byte, 32<<10)
	for {
		count, err := source.Read(buffer)
		if count > 0 {
			output.append(buffer[:count], stdout)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				output.mu.Lock()
				output.readErr = errors.Join(
					output.readErr,
					fmt.Errorf("read build process output: %w", err),
				)
				output.mu.Unlock()
			}
			return
		}
	}
}

func (output *buildOutput) append(raw []byte, stdout bool) {
	output.mu.Lock()
	defer output.mu.Unlock()
	remaining := output.limit - output.total
	if remaining < len(raw) {
		output.overflow = true
	}
	if remaining <= 0 {
		return
	}
	if len(raw) > remaining {
		raw = raw[:remaining]
	}
	output.total += len(raw)
	if stdout {
		_, _ = output.stdout.Write(raw)
	} else {
		_, _ = output.stderr.Write(raw)
	}
}

func (output *buildOutput) result(waitErr error) buildCommandResult {
	output.mu.Lock()
	defer output.mu.Unlock()
	stdout := bytes.Clone(output.stdout.Bytes())
	stderr := bytes.Clone(output.stderr.Bytes())
	if output.overflow {
		marker := []byte(buildOutputMarker)
		keep := output.limit - len(marker)
		if keep < 0 {
			keep = 0
			marker = marker[:output.limit]
		}
		if len(stdout)+len(stderr) > keep {
			trim := len(stdout) + len(stderr) - keep
			if trim <= len(stderr) {
				stderr = stderr[:len(stderr)-trim]
			} else {
				trim -= len(stderr)
				stderr = nil
				stdout = stdout[:len(stdout)-trim]
			}
		}
		stderr = append(stderr, marker...)
	}
	return buildCommandResult{
		stderr:   stderr,
		stdout:   stdout,
		overflow: output.overflow,
		waitErr:  waitErr,
	}
}

func waitBuildPidfd(pidFD int) error {
	poll := []unix.PollFd{{Fd: int32(pidFD), Events: unix.POLLIN}}
	for {
		count, err := unix.Poll(poll, -1)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("poll build process pidfd: %w", err)
		}
		if count == 1 && poll[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0 {
			return nil
		}
		return fmt.Errorf(
			"build process pidfd events = %#x",
			poll[0].Revents,
		)
	}
}

func buildPidfdReady(pidFD int) (bool, error) {
	poll := []unix.PollFd{{Fd: int32(pidFD), Events: unix.POLLIN}}
	count, err := unix.Poll(poll, 0)
	if err != nil {
		return false, fmt.Errorf("poll build process pidfd: %w", err)
	}
	if count == 0 {
		return false, nil
	}
	if poll[0].Revents&(unix.POLLIN|unix.POLLHUP) == 0 {
		return false, fmt.Errorf(
			"build process pidfd events = %#x",
			poll[0].Revents,
		)
	}
	return true, nil
}

func createBuildCgroup(
	limits buildLimits,
) (string, *os.File, error) {
	rootFD, err := unix.Open(
		buildCgroupRoot,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return "", nil, fmt.Errorf("open build cgroup root: %w", err)
	}
	defer unix.Close(rootFD)
	processes, err := os.ReadFile(filepath.Join(buildCgroupRoot, "cgroup.procs"))
	if err != nil {
		return "", nil, fmt.Errorf("read build cgroup root processes: %w", err)
	}
	if len(bytes.TrimSpace(processes)) != 0 {
		return "", nil, errors.New("build cgroup root is not process-free")
	}
	if err := unix.Mkdirat(rootFD, buildCgroupLeaf, 0o755); err != nil {
		return "", nil, fmt.Errorf("create build cgroup: %w", err)
	}
	cgroupFD, err := unix.Openat(
		rootFD,
		buildCgroupLeaf,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		_ = unix.Unlinkat(rootFD, buildCgroupLeaf, unix.AT_REMOVEDIR)
		return "", nil, fmt.Errorf("open build cgroup: %w", err)
	}
	if err := configureBuildCgroup(cgroupFD, limits); err != nil {
		unix.Close(cgroupFD)
		_ = unix.Unlinkat(rootFD, buildCgroupLeaf, unix.AT_REMOVEDIR)
		return "", nil, err
	}
	path := filepath.Join(buildCgroupRoot, buildCgroupLeaf)
	return path, os.NewFile(uintptr(cgroupFD), path), nil
}

func configureBuildCgroup(cgroupFD int, limits buildLimits) error {
	values := []struct {
		file  string
		value string
	}{
		{"memory.max", strconv.FormatInt(limits.MemoryBytes, 10)},
		{"memory.swap.max", "0"},
		{"memory.oom.group", "1"},
		{
			"cpu.max",
			strconv.FormatInt(limits.CPUQuotaMicros, 10) + " " +
				strconv.FormatInt(limits.CPUPeriodMicros, 10),
		},
		{"pids.max", strconv.FormatInt(limits.PIDs, 10)},
	}
	for _, value := range values {
		controlFD, err := unix.Openat(
			cgroupFD,
			value.file,
			unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
		if err != nil {
			return fmt.Errorf("open build cgroup %s: %w", value.file, err)
		}
		control := os.NewFile(uintptr(controlFD), value.file)
		count, writeErr := control.WriteString(value.value)
		if writeErr == nil && count != len(value.value) {
			writeErr = io.ErrShortWrite
		}
		closeErr := control.Close()
		if writeErr != nil || closeErr != nil {
			return fmt.Errorf(
				"set build cgroup %s: %w",
				value.file,
				errors.Join(writeErr, closeErr),
			)
		}
	}
	return nil
}

func killCgroup(path string) error {
	if err := os.WriteFile(filepath.Join(path, "cgroup.kill"), []byte("1"), 0o644); err != nil {
		return fmt.Errorf("kill cgroup: %w", err)
	}
	return nil
}

func waitCgroupEmpty(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), buildCleanupTimeout)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		raw, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
		if err != nil {
			return fmt.Errorf("read cgroup events: %w", err)
		}
		populated, err := parseCgroupPopulated(raw)
		if err != nil {
			return err
		}
		if !populated {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for cgroup to empty: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func parseCgroupPopulated(raw []byte) (bool, error) {
	found := false
	populated := false
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		fields := bytes.Fields(line)
		if len(fields) != 2 || !bytes.Equal(fields[0], []byte("populated")) {
			continue
		}
		if found {
			return false, errors.New("cgroup events repeats populated")
		}
		found = true
		switch string(fields[1]) {
		case "0":
			populated = false
		case "1":
			populated = true
		default:
			return false, fmt.Errorf(
				"cgroup populated = %q",
				fields[1],
			)
		}
	}
	if !found {
		return false, errors.New("cgroup events omits populated")
	}
	return populated, nil
}

func cleanupBuildCgroup(path string, cgroup *os.File) error {
	var cleanupErr error
	cleanupErr = errors.Join(cleanupErr, killCgroup(path))
	cleanupErr = errors.Join(cleanupErr, waitCgroupEmpty(path))
	if cgroup != nil {
		if err := cgroup.Close(); err != nil && !errors.Is(err, os.ErrInvalid) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if cleanupErr == nil {
		if err := os.Remove(path); err != nil {
			cleanupErr = fmt.Errorf("remove build cgroup: %w", err)
		}
	}
	return cleanupErr
}
