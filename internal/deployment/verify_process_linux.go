//go:build linux

package deployment

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	programVerifierMemoryMax    = "2147483648"
	programVerifierSwapMax      = "0"
	programVerifierOOMGroup     = "1"
	programVerifierCPUMax       = "100000 100000"
	programVerifierPIDsMax      = "32"
	programVerifierDrainTimeout = 10 * time.Second
)

var programVerifierCgroupLimits = []struct {
	file  string
	value string
}{
	{"memory.max", programVerifierMemoryMax},
	{"memory.swap.max", programVerifierSwapMax},
	{"memory.oom.group", programVerifierOOMGroup},
	{"cpu.max", programVerifierCPUMax},
	{"pids.max", programVerifierPIDsMax},
}

type programVerifierTerminalRead struct {
	result programVerifierProcessResult
	err    error
}

func runProgramVerifierProcess(
	ctx context.Context,
	config programVerifierProcessConfig,
) (result programVerifierProcessResult, returnErr error) {
	if err := validateProgramVerifierProcessConfig(ctx, config); err != nil {
		return programVerifierProcessResult{}, err
	}

	processContext, cancel := context.WithTimeout(ctx, programVerifierDeadline)
	defer cancel()

	cgroupPath, cgroup, err := createProgramVerifierCgroup(
		config.unitCgroupRoot,
		config.leaseIdentity,
	)
	if err != nil {
		return programVerifierProcessResult{}, err
	}
	killForCleanup := false
	pidFD := -1
	defer func() {
		if pidFD >= 0 {
			if err := unix.Close(pidFD); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close program verifier pidfd: %w", err))
			}
		}
		if err := cgroup.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close program verifier cgroup: %w", err))
		}
		cleanupContext, cleanupCancel := context.WithTimeout(
			context.Background(),
			programVerifierDrainTimeout,
		)
		defer cleanupCancel()
		if err := cleanupProgramVerifierCgroup(cleanupContext, cgroupPath, killForCleanup); err != nil {
			returnErr = errors.Join(returnErr, err)
		}
	}()

	code, err := openProgramVerifierSnapshot(config.code)
	if err != nil {
		return programVerifierProcessResult{}, fmt.Errorf("open program verifier code snapshot: %w", err)
	}
	defer code.Close()
	dependencies, err := openProgramVerifierSnapshot(config.dependencies)
	if err != nil {
		return programVerifierProcessResult{}, fmt.Errorf("open program verifier dependency snapshot: %w", err)
	}
	defer dependencies.Close()

	resultReader, resultWriter, err := os.Pipe()
	if err != nil {
		return programVerifierProcessResult{}, fmt.Errorf("create program verifier result pipe: %w", err)
	}
	defer resultReader.Close()
	defer resultWriter.Close()

	command := newProgramVerifierCommand(
		processContext,
		config,
		int(cgroup.Fd()),
		&pidFD,
		code,
		dependencies,
		resultWriter,
	)
	command.Cancel = func() error {
		return killProgramVerifierCgroup(cgroupPath)
	}
	command.WaitDelay = programVerifierDrainTimeout
	if err := command.Start(); err != nil {
		return programVerifierProcessResult{}, fmt.Errorf("start program verifier: %w", err)
	}
	if err := resultWriter.Close(); err != nil {
		killForCleanup = true
		_ = killProgramVerifierCgroup(cgroupPath)
		_ = command.Wait()
		return programVerifierProcessResult{}, fmt.Errorf("close parent program verifier result writer: %w", err)
	}

	wait := make(chan error, 1)
	go func() {
		wait <- command.Wait()
	}()
	readyDeadline := time.Now().Add(programVerifierBootstrapDeadline)
	if processDeadline, ok := processContext.Deadline(); ok && processDeadline.Before(readyDeadline) {
		readyDeadline = processDeadline
	}
	resultErr := resultReader.SetReadDeadline(readyDeadline)
	if resultErr == nil {
		resultErr = readProgramVerifierReady(resultReader)
	}
	if clearErr := resultReader.SetReadDeadline(time.Time{}); resultErr == nil && clearErr != nil {
		resultErr = fmt.Errorf("clear program verifier readiness deadline: %w", clearErr)
	}
	if resultErr == nil {
		stop := func() {
			killForCleanup = true
			_ = killProgramVerifierCgroup(cgroupPath)
			_ = resultReader.Close()
		}
		result, resultErr, err = awaitProgramVerifierTerminal(
			processContext,
			resultReader,
			wait,
			programVerifierDrainTimeout,
			stop,
		)
	} else {
		killForCleanup = true
		_ = killProgramVerifierCgroup(cgroupPath)
		err = <-wait
	}
	if processContext.Err() != nil {
		killForCleanup = true
		return programVerifierProcessResult{}, fmt.Errorf("program verifier context: %w", processContext.Err())
	}
	if resultErr != nil {
		killForCleanup = true
		if err != nil {
			return programVerifierProcessResult{}, errors.Join(
				resultErr,
				fmt.Errorf("wait for program verifier: %w", err),
			)
		}
		return programVerifierProcessResult{}, resultErr
	}
	if err != nil {
		killForCleanup = true
		return programVerifierProcessResult{}, fmt.Errorf("wait for program verifier: %w", err)
	}
	return result, nil
}

func awaitProgramVerifierTerminal(
	ctx context.Context,
	reader *os.File,
	wait <-chan error,
	drainTimeout time.Duration,
	stop func(),
) (programVerifierProcessResult, error, error) {
	terminal := make(chan programVerifierTerminalRead, 1)
	go func() {
		result, err := readProgramVerifierTerminal(reader)
		terminal <- programVerifierTerminalRead{result: result, err: err}
	}()

	select {
	case outcome := <-terminal:
		select {
		case waitErr := <-wait:
			return outcome.result, outcome.err, waitErr
		case <-ctx.Done():
			stop()
			return outcome.result, outcome.err, <-wait
		}
	case waitErr := <-wait:
		timer := time.NewTimer(drainTimeout)
		defer timer.Stop()
		select {
		case outcome := <-terminal:
			return outcome.result, outcome.err, waitErr
		case <-timer.C:
			stop()
			<-terminal
			return programVerifierProcessResult{},
				errors.New("program verifier result stream remained open after process exit"),
				waitErr
		case <-ctx.Done():
			stop()
			<-terminal
			return programVerifierProcessResult{}, ctx.Err(), waitErr
		}
	case <-ctx.Done():
		stop()
		outcome := <-terminal
		return outcome.result, ctx.Err(), <-wait
	}
}

func validateProgramVerifierProcessConfig(
	ctx context.Context,
	config programVerifierProcessConfig,
) error {
	if ctx == nil {
		return errors.New("program verifier context is nil")
	}
	if config.executable == "" {
		return errors.New("program verifier executable is empty")
	}
	if config.code == nil || config.dependencies == nil {
		return errors.New("program verifier Artifact descriptors are required")
	}
	if !filepath.IsAbs(config.unitCgroupRoot) {
		return errors.New("program verifier unit cgroup root is not absolute")
	}
	if _, err := programVerifierCgroupLeaf(config.leaseIdentity); err != nil {
		return err
	}
	return nil
}

func newProgramVerifierCommand(
	ctx context.Context,
	config programVerifierProcessConfig,
	cgroupFD int,
	pidFD *int,
	code *os.File,
	dependencies *os.File,
	result *os.File,
) *exec.Cmd {
	command := exec.CommandContext(ctx, config.executable, config.arguments...)
	command.Dir = "/"
	command.Env = []string{}
	command.ExtraFiles = []*os.File{code, dependencies, result}
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNET |
			syscall.CLONE_NEWPID |
			syscall.CLONE_NEWIPC,
		Unshareflags: syscall.CLONE_NEWNS,
		UseCgroupFD:  true,
		CgroupFD:     cgroupFD,
		PidFD:        pidFD,
	}
	return command
}

func createProgramVerifierCgroup(unitRoot, leaseIdentity string) (string, *os.File, error) {
	leaf, err := programVerifierCgroupLeaf(leaseIdentity)
	if err != nil {
		return "", nil, err
	}
	root := filepath.Clean(unitRoot)
	if !filepath.IsAbs(root) {
		return "", nil, errors.New("program verifier unit cgroup root is not absolute")
	}
	rootFD, err := unix.Open(
		root,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return "", nil, fmt.Errorf("open program verifier unit cgroup root: %w", err)
	}
	defer unix.Close(rootFD)
	if err := unix.Mkdirat(rootFD, leaf, 0o755); err != nil {
		return "", nil, fmt.Errorf("create program verifier cgroup: %w", err)
	}
	cgroupFD, err := unix.Openat(
		rootFD,
		leaf,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		_ = unix.Unlinkat(rootFD, leaf, unix.AT_REMOVEDIR)
		return "", nil, fmt.Errorf("open program verifier cgroup: %w", err)
	}
	if err := configureProgramVerifierCgroup(cgroupFD); err != nil {
		unix.Close(cgroupFD)
		_ = unix.Unlinkat(rootFD, leaf, unix.AT_REMOVEDIR)
		return "", nil, err
	}
	path := filepath.Join(root, leaf)
	return path, os.NewFile(uintptr(cgroupFD), path), nil
}

func programVerifierCgroupLeaf(leaseIdentity string) (string, error) {
	if len(leaseIdentity) == 0 || len(leaseIdentity) > 128 {
		return "", errors.New("program verifier lease identity is outside [1,128] bytes")
	}
	for _, value := range []byte(leaseIdentity) {
		if (value >= 'a' && value <= 'z') ||
			(value >= 'A' && value <= 'Z') ||
			(value >= '0' && value <= '9') ||
			value == '-' {
			continue
		}
		return "", errors.New("program verifier lease identity is outside the exact ASCII domain")
	}
	return "verifier-" + leaseIdentity, nil
}

func configureProgramVerifierCgroup(cgroupFD int) error {
	for _, limit := range programVerifierCgroupLimits {
		controlFD, err := unix.Openat(
			cgroupFD,
			limit.file,
			unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
		if err != nil {
			return fmt.Errorf("open program verifier %s: %w", limit.file, err)
		}
		control := os.NewFile(uintptr(controlFD), limit.file)
		err = writeAll(control, []byte(limit.value))
		closeErr := control.Close()
		if err != nil {
			return fmt.Errorf("set program verifier %s: %w", limit.file, err)
		}
		if closeErr != nil {
			return fmt.Errorf("close program verifier %s: %w", limit.file, closeErr)
		}
	}
	return nil
}

func openProgramVerifierSnapshot(source *os.File) (*os.File, error) {
	if source == nil {
		return nil, errors.New("Artifact descriptor is nil")
	}
	flags, err := unix.FcntlInt(source.Fd(), unix.F_GETFL, 0)
	if err != nil {
		return nil, fmt.Errorf("inspect Artifact descriptor: %w", err)
	}
	if flags&unix.O_ACCMODE != unix.O_RDONLY {
		return nil, errors.New("Artifact descriptor is not read-only")
	}
	var sourceStat unix.Stat_t
	if err := unix.Fstat(int(source.Fd()), &sourceStat); err != nil {
		return nil, fmt.Errorf("stat Artifact descriptor: %w", err)
	}
	if sourceStat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("Artifact descriptor is not a regular file")
	}
	fd, err := unix.Open(
		"/proc/self/fd/"+strconv.FormatUint(uint64(source.Fd()), 10),
		unix.O_RDONLY|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("reopen Artifact descriptor: %w", err)
	}
	var reopenedStat unix.Stat_t
	if err := unix.Fstat(fd, &reopenedStat); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("stat reopened Artifact descriptor: %w", err)
	}
	if sourceStat.Dev != reopenedStat.Dev || sourceStat.Ino != reopenedStat.Ino {
		unix.Close(fd)
		return nil, errors.New("reopened Artifact descriptor changed inode identity")
	}
	return os.NewFile(uintptr(fd), source.Name()), nil
}

func killProgramVerifierCgroup(path string) error {
	if err := os.WriteFile(filepath.Join(path, "cgroup.kill"), []byte("1"), 0o644); err != nil {
		return fmt.Errorf("kill program verifier cgroup: %w", err)
	}
	return nil
}

func waitProgramVerifierCgroupEmpty(ctx context.Context, path string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		raw, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
		if err != nil {
			return fmt.Errorf("read program verifier cgroup events: %w", err)
		}
		populated, err := parseProgramVerifierCgroupPopulated(raw)
		if err != nil {
			return err
		}
		if !populated {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for program verifier cgroup to empty: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func parseProgramVerifierCgroupPopulated(raw []byte) (bool, error) {
	found := false
	populated := false
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "populated" {
			continue
		}
		if found {
			return false, errors.New("program verifier cgroup events repeats populated")
		}
		found = true
		switch fields[1] {
		case "0":
			populated = false
		case "1":
			populated = true
		default:
			return false, fmt.Errorf("program verifier cgroup populated = %q", fields[1])
		}
	}
	if !found {
		return false, errors.New("program verifier cgroup events omits populated")
	}
	return populated, nil
}

func cleanupProgramVerifierCgroup(ctx context.Context, path string, kill bool) error {
	var cleanupErr error
	if kill {
		cleanupErr = errors.Join(cleanupErr, killProgramVerifierCgroup(path))
	}
	if err := waitProgramVerifierCgroupEmpty(ctx, path); err != nil {
		return errors.Join(cleanupErr, err)
	}
	if err := os.Remove(path); err != nil {
		cleanupErr = errors.Join(
			cleanupErr,
			fmt.Errorf("remove program verifier cgroup: %w", err),
		)
	}
	return cleanupErr
}
