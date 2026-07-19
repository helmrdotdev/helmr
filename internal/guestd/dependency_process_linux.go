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

	"github.com/helmrdotdev/helmr/internal/deployment"
	"golang.org/x/sys/unix"
)

const (
	dependencyProcessInitArg = "__helmr-dependency-process-init"
	dependencyCgroupRoot     = "/sys/fs/cgroup/helmr"
	dependencyCgroupLeaf     = "manager"
	dependencyOutputLimit    = 64 << 10
	dependencyCleanupTimeout = 10 * time.Second
)

type dependencyProcessConfig struct {
	Aliases     []deployment.PlanAlias       `json:"aliases"`
	Command     deployment.PlanCommand       `json:"command"`
	Environment []deployment.PlanEnvironment `json:"environment"`
	Identity    deployment.PlanIdentity      `json:"identity"`
	Manager     string                       `json:"manager"`
	ProcessRoot string                       `json:"processRoot"`
	Runtime     string                       `json:"runtime"`
	Toolchain   string                       `json:"toolchain"`
}

type dependencyProcessFailure struct {
	message string
	reason  deployment.ManagerFailure
}

type dependencyCommandResult struct {
	stderr   []byte
	stdout   []byte
	overflow bool
	waitErr  error
}

type dependencyReadyResult struct {
	err error
}

type dependencyOutput struct {
	limit    int
	mu       sync.Mutex
	overflow bool
	readErr  error
	stderr   bytes.Buffer
	stdout   bytes.Buffer
	total    int
}

func activateDependencyManager(
	ctx context.Context,
	request deployment.ManagerRequest,
	staged stagedDependencyComponents,
) (_ deployment.ManagerMetadata, returnErr error) {
	if request.Operation != deployment.ManagerProbe {
		return deployment.ManagerMetadata{}, fmt.Errorf(
			"dependency manager %q execution is unavailable",
			request.Operation,
		)
	}

	processContext, cancel := context.WithTimeout(
		ctx,
		time.Duration(request.DependencyPlan.Limits.WallTimeSeconds)*time.Second,
	)
	defer cancel()

	root, err := prepareDependencyProcessRoot(request.DependencyPlan)
	if err != nil {
		return deployment.ManagerMetadata{}, err
	}
	rootRemoved := false
	defer func() {
		if !rootRemoved {
			returnErr = errors.Join(returnErr, removeDependencyProcessRoot(root))
		}
	}()

	cgroupPath, cgroup, err := createDependencyCgroup(request.DependencyPlan.Limits)
	if err != nil {
		return deployment.ManagerMetadata{}, err
	}
	cgroupRemoved := false
	defer func() {
		if !cgroupRemoved {
			returnErr = errors.Join(
				returnErr,
				cleanupDependencyCgroup(cgroupPath, cgroup, true),
			)
		}
	}()

	config, err := dependencyProcessConfiguration(
		request.DependencyPlan,
		staged,
		root,
		request.DependencyPlan.Probe,
	)
	if err != nil {
		return deployment.ManagerMetadata{}, err
	}
	probe, interrupted, err := runDependencyCommand(processContext, config, cgroup)
	if err != nil {
		return deployment.ManagerMetadata{}, err
	}
	var failure *dependencyProcessFailure
	if interrupted {
		if ctx.Err() != nil {
			return deployment.ManagerMetadata{}, fmt.Errorf(
				"dependency manager context: %w",
				ctx.Err(),
			)
		}
		failure = &dependencyProcessFailure{
			message: "package manager activation exceeded its wall-time limit",
			reason:  deployment.ManagerProcessFailed,
		}
	} else {
		failure = validateDependencyProbe(
			probe,
			request.DependencyPlan.PackageManager.Version,
		)
	}

	if failure == nil {
		config.Command = request.DependencyPlan.Handshake
		handshake, interrupted, err := runDependencyCommand(
			processContext,
			config,
			cgroup,
		)
		if err != nil {
			return deployment.ManagerMetadata{}, err
		}
		if interrupted {
			if ctx.Err() != nil {
				return deployment.ManagerMetadata{}, fmt.Errorf(
					"dependency manager context: %w",
					ctx.Err(),
				)
			}
			failure = &dependencyProcessFailure{
				message: "package manager activation exceeded its wall-time limit",
				reason:  deployment.ManagerProcessFailed,
			}
		} else {
			failure = validateDependencyHandshake(handshake)
		}
	}

	if err := cleanupDependencyCgroup(cgroupPath, cgroup, true); err != nil {
		return deployment.ManagerMetadata{}, err
	}
	cgroupRemoved = true
	if err := removeDependencyProcessRoot(root); err != nil {
		return deployment.ManagerMetadata{}, err
	}
	rootRemoved = true

	requestDigest, err := deployment.ManagerRequestDigest(request)
	if err != nil {
		return deployment.ManagerMetadata{}, err
	}
	metadata := deployment.ManagerMetadata{
		FormatVersion: deployment.ManagerFormatVersion,
		Operation:     request.Operation,
		RequestDigest: requestDigest,
	}
	if failure != nil {
		metadata.Outcome = deployment.ManagerFailed
		metadata.Reason = &failure.reason
		metadata.Message = &failure.message
		return metadata, nil
	}
	version := request.DependencyPlan.PackageManager.Version
	metadata.Outcome = deployment.ManagerSucceeded
	metadata.ObservedVersion = &version
	return metadata, nil
}

func dependencyProcessConfiguration(
	plan deployment.DependencyPlan,
	staged stagedDependencyComponents,
	root string,
	command deployment.PlanCommand,
) (dependencyProcessConfig, error) {
	manager, err := staged.Path("manager")
	if err != nil {
		return dependencyProcessConfig{}, err
	}
	runtimePath, err := staged.Path("runtime")
	if err != nil {
		return dependencyProcessConfig{}, err
	}
	toolchain, err := staged.Path("toolchain")
	if err != nil {
		return dependencyProcessConfig{}, err
	}
	config := dependencyProcessConfig{
		Aliases:     plan.Aliases,
		Command:     command,
		Environment: plan.Environment,
		Identity:    plan.Identity,
		Manager:     manager,
		ProcessRoot: root,
		Runtime:     runtimePath,
		Toolchain:   toolchain,
	}
	return config, nil
}

func prepareDependencyProcessRoot(plan deployment.DependencyPlan) (string, error) {
	root, err := mkdirGuestdTemp("helmr-manager-root-*")
	if err != nil {
		return "", fmt.Errorf("create dependency process root: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(root)
		}
	}()
	if err := os.Chmod(root, 0o755); err != nil {
		return "", fmt.Errorf("set dependency process root mode: %w", err)
	}

	directories := []struct {
		mode os.FileMode
		path string
	}{
		{0o755, "dev"},
		{0o755, "opt/helmr/manager"},
		{0o755, "opt/helmr/runtime"},
		{0o755, "nix"},
		{0o700, "work"},
		{0o700, "work/home"},
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
			return "", fmt.Errorf("create dependency process directory %q: %w", directory.path, err)
		}
		if err := os.Chmod(path, directory.mode); err != nil {
			return "", fmt.Errorf("set dependency process directory %q mode: %w", directory.path, err)
		}
	}
	for _, relative := range []string{"work", "work/home"} {
		if err := os.Chown(
			filepath.Join(root, relative),
			int(plan.Identity.UID),
			int(plan.Identity.GID),
		); err != nil {
			return "", fmt.Errorf("own dependency process %q: %w", relative, err)
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
			return "", fmt.Errorf("create dependency process /dev/%s: %w", device.name, err)
		}
		if err := os.Chmod(target, os.FileMode(device.mode)); err != nil {
			return "", fmt.Errorf("set dependency process /dev/%s mode: %w", device.name, err)
		}
	}
	for _, alias := range plan.Aliases {
		target := filepath.Join(root, alias.Path[1:])
		if err := os.Symlink(alias.Target, target); err != nil {
			return "", fmt.Errorf("create dependency process alias %q: %w", alias.Path, err)
		}
	}
	cleanup = false
	return root, nil
}

func removeDependencyProcessRoot(root string) error {
	if root == "" {
		return nil
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove dependency process root: %w", err)
	}
	return nil
}

func runDependencyCommand(
	ctx context.Context,
	config dependencyProcessConfig,
	cgroup *os.File,
) (result dependencyCommandResult, interrupted bool, returnErr error) {
	path, processCgroup, err := createDependencyProcessCgroup(cgroup)
	if err != nil {
		return dependencyCommandResult{}, false, err
	}
	defer func() {
		returnErr = errors.Join(
			returnErr,
			cleanupDependencyCgroup(path, processCgroup, true),
		)
	}()
	return runDependencyCommandInCgroup(ctx, config, path, processCgroup)
}

func runDependencyCommandInCgroup(
	ctx context.Context,
	config dependencyProcessConfig,
	cgroupPath string,
	cgroup *os.File,
) (dependencyCommandResult, bool, error) {
	raw, err := json.Marshal(config)
	if err != nil {
		return dependencyCommandResult{}, false, fmt.Errorf(
			"encode dependency process config: %w",
			err,
		)
	}
	argument := base64.RawURLEncoding.EncodeToString(raw)
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return dependencyCommandResult{}, false, fmt.Errorf(
			"create dependency process readiness pipe: %w",
			err,
		)
	}
	defer readyReader.Close()
	defer readyWriter.Close()
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		return dependencyCommandResult{}, false, fmt.Errorf(
			"create dependency process stdout pipe: %w",
			err,
		)
	}
	defer stdoutReader.Close()
	defer stdoutWriter.Close()
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		return dependencyCommandResult{}, false, fmt.Errorf(
			"create dependency process stderr pipe: %w",
			err,
		)
	}
	defer stderrReader.Close()
	defer stderrWriter.Close()
	stdin, err := os.Open("/dev/null")
	if err != nil {
		return dependencyCommandResult{}, false, fmt.Errorf(
			"open dependency process stdin: %w",
			err,
		)
	}
	defer stdin.Close()

	pidFD := -1
	command := exec.Command("/proc/self/exe", dependencyProcessInitArg, argument)
	command.Dir = "/"
	command.Env = []string{}
	command.Stdin = stdin
	command.Stdout = stdoutWriter
	command.Stderr = stderrWriter
	command.ExtraFiles = []*os.File{readyWriter}
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNET |
			syscall.CLONE_NEWPID |
			syscall.CLONE_NEWIPC,
		Unshareflags: syscall.CLONE_NEWNS,
		UseCgroupFD:  true,
		CgroupFD:     int(cgroup.Fd()),
		PidFD:        &pidFD,
	}
	if err := command.Start(); err != nil {
		return dependencyCommandResult{}, false, fmt.Errorf(
			"start dependency process: %w",
			err,
		)
	}
	if pidFD < 0 {
		_ = killDependencyCgroup(cgroupPath)
		_ = command.Wait()
		return dependencyCommandResult{}, false, errors.New(
			"dependency process did not return a pidfd",
		)
	}
	defer unix.Close(pidFD)
	if err := errors.Join(
		readyWriter.Close(),
		stdoutWriter.Close(),
		stderrWriter.Close(),
	); err != nil {
		_ = killDependencyCgroup(cgroupPath)
		_ = command.Wait()
		return dependencyCommandResult{}, false, fmt.Errorf(
			"close dependency process parent descriptors: %w",
			err,
		)
	}

	output := &dependencyOutput{limit: dependencyOutputLimit}
	var outputReaders sync.WaitGroup
	outputReaders.Add(2)
	go func() {
		defer outputReaders.Done()
		output.drain(stdoutReader, true)
	}()
	go func() {
		defer outputReaders.Done()
		output.drain(stderrReader, false)
	}()

	ready := make(chan dependencyReadyResult, 1)
	go func() {
		var marker [1]byte
		_, readErr := io.ReadFull(readyReader, marker[:])
		if readErr == nil && marker[0] != 1 {
			readErr = errors.New("dependency process readiness marker is invalid")
		}
		if readErr == nil {
			var extra [1]byte
			count, extraErr := readyReader.Read(extra[:])
			if count != 0 || !errors.Is(extraErr, io.EOF) {
				readErr = errors.New("dependency process readiness stream is not exact")
			}
		}
		ready <- dependencyReadyResult{err: readErr}
	}()
	exited := make(chan error, 1)
	go func() {
		exited <- waitDependencyPidfd(pidFD)
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
				if err := killDependencyCgroup(cgroupPath); err != nil {
					return dependencyCommandResult{}, false, errors.Join(
						fmt.Errorf("dependency process bootstrap: %w", readinessErr),
						err,
					)
				}
			}
		case pollErr := <-exited:
			if pollErr != nil {
				return dependencyCommandResult{}, false, pollErr
			}
			exitObserved = true
			cancelChannel = nil
		case <-cancelChannel:
			pidExited, pollErr := dependencyPidfdReady(pidFD)
			if pollErr != nil {
				return dependencyCommandResult{}, false, pollErr
			}
			if pidExited {
				exitObserved = true
				cancelChannel = nil
				continue
			}
			cancelled = true
			cancelChannel = nil
			if err := killDependencyCgroup(cgroupPath); err != nil {
				return dependencyCommandResult{}, false, err
			}
		}
	}
	waitErr := <-waited
	if err := killDependencyCgroup(cgroupPath); err != nil {
		return dependencyCommandResult{}, false, err
	}
	if err := waitDependencyCgroupEmpty(cgroupPath); err != nil {
		return dependencyCommandResult{}, false, err
	}
	outputReaders.Wait()
	if output.readErr != nil {
		return dependencyCommandResult{}, false, output.readErr
	}
	if readinessErr != nil {
		diagnostic := output.result(waitErr).stderr
		if len(diagnostic) > 4096 {
			diagnostic = diagnostic[:4096]
		}
		return dependencyCommandResult{}, false, fmt.Errorf(
			"dependency process bootstrap: %w: wait=%v stderr=%q",
			readinessErr,
			waitErr,
			diagnostic,
		)
	}
	result := output.result(waitErr)
	if cancelled {
		return result, true, nil
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			return dependencyCommandResult{}, false, fmt.Errorf(
				"wait for dependency process: %w",
				waitErr,
			)
		}
	}
	return result, false, nil
}

func createDependencyProcessCgroup(parent *os.File) (string, *os.File, error) {
	const leaf = "process"
	if parent == nil {
		return "", nil, errors.New("dependency Plan cgroup is nil")
	}
	if err := unix.Mkdirat(int(parent.Fd()), leaf, 0o755); err != nil {
		return "", nil, fmt.Errorf("create dependency process cgroup: %w", err)
	}
	fd, err := unix.Openat(
		int(parent.Fd()),
		leaf,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		_ = unix.Unlinkat(int(parent.Fd()), leaf, unix.AT_REMOVEDIR)
		return "", nil, fmt.Errorf("open dependency process cgroup: %w", err)
	}
	path := filepath.Join(parent.Name(), leaf)
	return path, os.NewFile(uintptr(fd), path), nil
}

func (output *dependencyOutput) drain(source io.Reader, stdout bool) {
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
					fmt.Errorf("read dependency process output: %w", err),
				)
				output.mu.Unlock()
			}
			return
		}
	}
}

func (output *dependencyOutput) append(raw []byte, stdout bool) {
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

func (output *dependencyOutput) result(waitErr error) dependencyCommandResult {
	output.mu.Lock()
	defer output.mu.Unlock()
	return dependencyCommandResult{
		stderr:   bytes.Clone(output.stderr.Bytes()),
		stdout:   bytes.Clone(output.stdout.Bytes()),
		overflow: output.overflow,
		waitErr:  waitErr,
	}
}

func waitDependencyPidfd(pidFD int) error {
	poll := []unix.PollFd{{Fd: int32(pidFD), Events: unix.POLLIN}}
	for {
		count, err := unix.Poll(poll, -1)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("poll dependency process pidfd: %w", err)
		}
		if count == 1 && poll[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0 {
			return nil
		}
		return fmt.Errorf(
			"dependency process pidfd events = %#x",
			poll[0].Revents,
		)
	}
}

func dependencyPidfdReady(pidFD int) (bool, error) {
	poll := []unix.PollFd{{Fd: int32(pidFD), Events: unix.POLLIN}}
	count, err := unix.Poll(poll, 0)
	if err != nil {
		return false, fmt.Errorf("poll dependency process pidfd: %w", err)
	}
	if count == 0 {
		return false, nil
	}
	if poll[0].Revents&(unix.POLLIN|unix.POLLHUP) == 0 {
		return false, fmt.Errorf(
			"dependency process pidfd events = %#x",
			poll[0].Revents,
		)
	}
	return true, nil
}

func validateDependencyProbe(
	result dependencyCommandResult,
	version string,
) *dependencyProcessFailure {
	if result.overflow {
		return &dependencyProcessFailure{
			message: "package manager probe output exceeded 65536 bytes",
			reason:  deployment.ManagerOutputInvalid,
		}
	}
	if result.waitErr != nil {
		return &dependencyProcessFailure{
			message: "package manager probe exited unsuccessfully",
			reason:  deployment.ManagerProcessFailed,
		}
	}
	expected := []byte(version)
	if !bytes.Equal(result.stdout, expected) &&
		!bytes.Equal(result.stdout, append(bytes.Clone(expected), '\n')) {
		return &dependencyProcessFailure{
			message: "package manager probe returned a different version",
			reason:  deployment.ManagerOutputInvalid,
		}
	}
	return nil
}

func validateDependencyHandshake(
	result dependencyCommandResult,
) *dependencyProcessFailure {
	if result.overflow {
		return &dependencyProcessFailure{
			message: "package manager handshake output exceeded 65536 bytes",
			reason:  deployment.ManagerOutputInvalid,
		}
	}
	if result.waitErr != nil {
		return &dependencyProcessFailure{
			message: "package manager handshake exited unsuccessfully",
			reason:  deployment.ManagerProcessFailed,
		}
	}
	return nil
}

func createDependencyCgroup(
	limits deployment.PlanLimits,
) (string, *os.File, error) {
	rootFD, err := unix.Open(
		dependencyCgroupRoot,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return "", nil, fmt.Errorf("open dependency cgroup root: %w", err)
	}
	defer unix.Close(rootFD)
	processes, err := os.ReadFile(filepath.Join(dependencyCgroupRoot, "cgroup.procs"))
	if err != nil {
		return "", nil, fmt.Errorf("read dependency cgroup root processes: %w", err)
	}
	if len(bytes.TrimSpace(processes)) != 0 {
		return "", nil, errors.New("dependency cgroup root is not process-free")
	}
	if err := unix.Mkdirat(rootFD, dependencyCgroupLeaf, 0o755); err != nil {
		return "", nil, fmt.Errorf("create dependency cgroup: %w", err)
	}
	cgroupFD, err := unix.Openat(
		rootFD,
		dependencyCgroupLeaf,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		_ = unix.Unlinkat(rootFD, dependencyCgroupLeaf, unix.AT_REMOVEDIR)
		return "", nil, fmt.Errorf("open dependency cgroup: %w", err)
	}
	if err := configureDependencyCgroup(cgroupFD, limits); err != nil {
		unix.Close(cgroupFD)
		_ = unix.Unlinkat(rootFD, dependencyCgroupLeaf, unix.AT_REMOVEDIR)
		return "", nil, err
	}
	path := filepath.Join(dependencyCgroupRoot, dependencyCgroupLeaf)
	return path, os.NewFile(uintptr(cgroupFD), path), nil
}

func configureDependencyCgroup(cgroupFD int, limits deployment.PlanLimits) error {
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
			return fmt.Errorf("open dependency cgroup %s: %w", value.file, err)
		}
		control := os.NewFile(uintptr(controlFD), value.file)
		count, writeErr := control.WriteString(value.value)
		if writeErr == nil && count != len(value.value) {
			writeErr = io.ErrShortWrite
		}
		closeErr := control.Close()
		if writeErr != nil || closeErr != nil {
			return fmt.Errorf(
				"set dependency cgroup %s: %w",
				value.file,
				errors.Join(writeErr, closeErr),
			)
		}
	}
	return nil
}

func killDependencyCgroup(path string) error {
	if err := os.WriteFile(filepath.Join(path, "cgroup.kill"), []byte("1"), 0o644); err != nil {
		return fmt.Errorf("kill dependency cgroup: %w", err)
	}
	return nil
}

func waitDependencyCgroupEmpty(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), dependencyCleanupTimeout)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		raw, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
		if err != nil {
			return fmt.Errorf("read dependency cgroup events: %w", err)
		}
		populated, err := parseDependencyCgroupPopulated(raw)
		if err != nil {
			return err
		}
		if !populated {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for dependency cgroup to empty: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func parseDependencyCgroupPopulated(raw []byte) (bool, error) {
	found := false
	populated := false
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		fields := bytes.Fields(line)
		if len(fields) != 2 || !bytes.Equal(fields[0], []byte("populated")) {
			continue
		}
		if found {
			return false, errors.New("dependency cgroup events repeats populated")
		}
		found = true
		switch string(fields[1]) {
		case "0":
			populated = false
		case "1":
			populated = true
		default:
			return false, fmt.Errorf(
				"dependency cgroup populated = %q",
				fields[1],
			)
		}
	}
	if !found {
		return false, errors.New("dependency cgroup events omits populated")
	}
	return populated, nil
}

func cleanupDependencyCgroup(path string, cgroup *os.File, kill bool) error {
	var cleanupErr error
	if kill {
		cleanupErr = errors.Join(cleanupErr, killDependencyCgroup(path))
	}
	cleanupErr = errors.Join(cleanupErr, waitDependencyCgroupEmpty(path))
	if cgroup != nil {
		if err := cgroup.Close(); err != nil && !errors.Is(err, os.ErrInvalid) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if cleanupErr == nil {
		if err := os.Remove(path); err != nil {
			cleanupErr = fmt.Errorf("remove dependency cgroup: %w", err)
		}
	}
	return cleanupErr
}
