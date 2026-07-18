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
	verifierMemoryMax    = "2147483648"
	verifierSwapMax      = "0"
	verifierOOMGroup     = "1"
	verifierCPUMax       = "100000 100000"
	verifierPIDsMax      = "32"
	verifierDrainTimeout = 10 * time.Second
)

var verifierCgroupLimits = []struct {
	file  string
	value string
}{
	{"memory.max", verifierMemoryMax},
	{"memory.swap.max", verifierSwapMax},
	{"memory.oom.group", verifierOOMGroup},
	{"cpu.max", verifierCPUMax},
	{"pids.max", verifierPIDsMax},
}

type verifierTerminalRead struct {
	result verifierProcessResult
	err    error
}

func runVerifierProcess(
	ctx context.Context,
	config verifierProcessConfig,
) (result verifierProcessResult, returnErr error) {
	if err := validateVerifierProcessConfig(ctx, config); err != nil {
		return verifierProcessResult{}, err
	}

	processContext, cancel := context.WithTimeout(ctx, verifierDeadline)
	defer cancel()

	cgroupPath, cgroup, err := createVerifierCgroup(
		config.job,
		config.unitCgroupRoot,
		config.leaseIdentity,
	)
	if err != nil {
		return verifierProcessResult{}, err
	}
	killForCleanup := false
	pidFD := -1
	defer func() {
		if pidFD >= 0 {
			if err := unix.Close(pidFD); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close artifact verifier pidfd: %w", err))
			}
		}
		if err := cgroup.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close artifact verifier cgroup: %w", err))
		}
		cleanupContext, cleanupCancel := context.WithTimeout(
			context.Background(),
			verifierDrainTimeout,
		)
		defer cleanupCancel()
		if err := cleanupVerifierCgroup(cleanupContext, cgroupPath, killForCleanup); err != nil {
			returnErr = errors.Join(returnErr, err)
		}
	}()

	artifacts := make([]*os.File, 0, len(config.artifacts))
	for index, source := range config.artifacts {
		artifact, err := openVerifierSnapshot(source)
		if err != nil {
			return verifierProcessResult{}, fmt.Errorf(
				"open artifact verifier snapshot %d: %w",
				index,
				err,
			)
		}
		artifacts = append(artifacts, artifact)
		defer artifact.Close()
	}

	resultReader, resultWriter, err := os.Pipe()
	if err != nil {
		return verifierProcessResult{}, fmt.Errorf("create artifact verifier result pipe: %w", err)
	}
	defer resultReader.Close()
	defer resultWriter.Close()

	command := newVerifierCommand(
		processContext,
		config,
		int(cgroup.Fd()),
		&pidFD,
		resultWriter,
		artifacts,
	)
	command.Cancel = func() error {
		return killVerifierCgroup(cgroupPath)
	}
	command.WaitDelay = verifierDrainTimeout
	if err := command.Start(); err != nil {
		return verifierProcessResult{}, fmt.Errorf("start artifact verifier: %w", err)
	}
	if err := resultWriter.Close(); err != nil {
		killForCleanup = true
		_ = killVerifierCgroup(cgroupPath)
		_ = command.Wait()
		return verifierProcessResult{}, fmt.Errorf("close parent artifact verifier result writer: %w", err)
	}

	wait := make(chan error, 1)
	go func() {
		wait <- command.Wait()
	}()
	readyDeadline := time.Now().Add(verifierBootstrapDeadline)
	if processDeadline, ok := processContext.Deadline(); ok && processDeadline.Before(readyDeadline) {
		readyDeadline = processDeadline
	}
	resultErr := resultReader.SetReadDeadline(readyDeadline)
	if resultErr == nil {
		resultErr = readVerifierReady(resultReader)
	}
	if clearErr := resultReader.SetReadDeadline(time.Time{}); resultErr == nil && clearErr != nil {
		resultErr = fmt.Errorf("clear artifact verifier readiness deadline: %w", clearErr)
	}
	if resultErr == nil {
		stop := func() {
			killForCleanup = true
			_ = killVerifierCgroup(cgroupPath)
			_ = resultReader.Close()
		}
		result, resultErr, err = awaitVerifierTerminal(
			processContext,
			resultReader,
			config.job,
			wait,
			verifierDrainTimeout,
			stop,
		)
	} else {
		killForCleanup = true
		_ = killVerifierCgroup(cgroupPath)
		err = <-wait
	}
	if processContext.Err() != nil {
		killForCleanup = true
		return verifierProcessResult{}, fmt.Errorf("artifact verifier context: %w", processContext.Err())
	}
	if resultErr != nil {
		killForCleanup = true
		if err != nil {
			return verifierProcessResult{}, errors.Join(
				resultErr,
				fmt.Errorf("wait for artifact verifier: %w", err),
			)
		}
		return verifierProcessResult{}, resultErr
	}
	if err != nil {
		killForCleanup = true
		return verifierProcessResult{}, fmt.Errorf("wait for artifact verifier: %w", err)
	}
	return result, nil
}

func awaitVerifierTerminal(
	ctx context.Context,
	reader *os.File,
	job verifierJob,
	wait <-chan error,
	drainTimeout time.Duration,
	stop func(),
) (verifierProcessResult, error, error) {
	terminal := make(chan verifierTerminalRead, 1)
	go func() {
		result, err := readVerifierTerminal(reader, job)
		terminal <- verifierTerminalRead{result: result, err: err}
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
			return verifierProcessResult{},
				errors.New("artifact verifier result stream remained open after process exit"),
				waitErr
		case <-ctx.Done():
			stop()
			<-terminal
			return verifierProcessResult{}, ctx.Err(), waitErr
		}
	case <-ctx.Done():
		stop()
		outcome := <-terminal
		return outcome.result, ctx.Err(), <-wait
	}
}

func validateVerifierProcessConfig(
	ctx context.Context,
	config verifierProcessConfig,
) error {
	if ctx == nil {
		return errors.New("artifact verifier context is nil")
	}
	count := config.job.artifactCount()
	if count == 0 {
		return fmt.Errorf("artifact verifier job = %q", config.job)
	}
	if len(config.artifacts) != count {
		return fmt.Errorf(
			"%s verifier artifact descriptor count = %d, want %d",
			config.job,
			len(config.artifacts),
			count,
		)
	}
	for _, artifact := range config.artifacts {
		if artifact == nil {
			return fmt.Errorf("%s verifier Artifact descriptors are required", config.job)
		}
	}
	if !filepath.IsAbs(config.unitCgroupRoot) {
		return errors.New("artifact verifier unit cgroup root is not absolute")
	}
	if _, err := verifierCgroupLeaf(config.job, config.leaseIdentity); err != nil {
		return err
	}
	return nil
}

func newVerifierCommand(
	ctx context.Context,
	config verifierProcessConfig,
	cgroupFD int,
	pidFD *int,
	result *os.File,
	artifacts []*os.File,
) *exec.Cmd {
	command := exec.CommandContext(ctx, verifierExecutable, verifierChildArguments(config.job)...)
	command.Dir = "/"
	command.Env = []string{}
	command.ExtraFiles = append([]*os.File{result}, artifacts...)
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

func createVerifierCgroup(
	job verifierJob,
	unitRoot string,
	leaseIdentity string,
) (string, *os.File, error) {
	leaf, err := verifierCgroupLeaf(job, leaseIdentity)
	if err != nil {
		return "", nil, err
	}
	root := filepath.Clean(unitRoot)
	if !filepath.IsAbs(root) {
		return "", nil, errors.New("artifact verifier unit cgroup root is not absolute")
	}
	rootFD, err := unix.Open(
		root,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return "", nil, fmt.Errorf("open artifact verifier unit cgroup root: %w", err)
	}
	defer unix.Close(rootFD)
	if err := unix.Mkdirat(rootFD, leaf, 0o755); err != nil {
		return "", nil, fmt.Errorf("create artifact verifier cgroup: %w", err)
	}
	cgroupFD, err := unix.Openat(
		rootFD,
		leaf,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		_ = unix.Unlinkat(rootFD, leaf, unix.AT_REMOVEDIR)
		return "", nil, fmt.Errorf("open artifact verifier cgroup: %w", err)
	}
	if err := configureVerifierCgroup(cgroupFD); err != nil {
		unix.Close(cgroupFD)
		_ = unix.Unlinkat(rootFD, leaf, unix.AT_REMOVEDIR)
		return "", nil, err
	}
	path := filepath.Join(root, leaf)
	return path, os.NewFile(uintptr(cgroupFD), path), nil
}

func verifierCgroupLeaf(job verifierJob, leaseIdentity string) (string, error) {
	if job.artifactCount() == 0 {
		return "", fmt.Errorf("artifact verifier job = %q", job)
	}
	if len(leaseIdentity) == 0 || len(leaseIdentity) > 128 {
		return "", errors.New("artifact verifier lease identity is outside [1,128] bytes")
	}
	for _, value := range []byte(leaseIdentity) {
		if (value >= 'a' && value <= 'z') ||
			(value >= 'A' && value <= 'Z') ||
			(value >= '0' && value <= '9') ||
			value == '-' {
			continue
		}
		return "", errors.New("artifact verifier lease identity is outside the exact ASCII domain")
	}
	return "verifier-" + string(job) + "-" + leaseIdentity, nil
}

func configureVerifierCgroup(cgroupFD int) error {
	for _, limit := range verifierCgroupLimits {
		controlFD, err := unix.Openat(
			cgroupFD,
			limit.file,
			unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
		if err != nil {
			return fmt.Errorf("open artifact verifier %s: %w", limit.file, err)
		}
		control := os.NewFile(uintptr(controlFD), limit.file)
		err = writeAll(control, []byte(limit.value))
		closeErr := control.Close()
		if err != nil {
			return fmt.Errorf("set artifact verifier %s: %w", limit.file, err)
		}
		if closeErr != nil {
			return fmt.Errorf("close artifact verifier %s: %w", limit.file, closeErr)
		}
	}
	return nil
}

func openVerifierSnapshot(source *os.File) (*os.File, error) {
	if source == nil {
		return nil, errors.New("artifact descriptor is nil")
	}
	flags, err := unix.FcntlInt(source.Fd(), unix.F_GETFL, 0)
	if err != nil {
		return nil, fmt.Errorf("inspect artifact descriptor: %w", err)
	}
	if flags&unix.O_ACCMODE != unix.O_RDONLY {
		return nil, errors.New("artifact descriptor is not read-only")
	}
	var sourceStat unix.Stat_t
	if err := unix.Fstat(int(source.Fd()), &sourceStat); err != nil {
		return nil, fmt.Errorf("stat artifact descriptor: %w", err)
	}
	if sourceStat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("artifact descriptor is not a regular file")
	}
	fd, err := unix.Open(
		"/proc/self/fd/"+strconv.FormatUint(uint64(source.Fd()), 10),
		unix.O_RDONLY|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("reopen artifact descriptor: %w", err)
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

func killVerifierCgroup(path string) error {
	if err := os.WriteFile(filepath.Join(path, "cgroup.kill"), []byte("1"), 0o644); err != nil {
		return fmt.Errorf("kill artifact verifier cgroup: %w", err)
	}
	return nil
}

func waitVerifierCgroupEmpty(ctx context.Context, path string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		raw, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
		if err != nil {
			return fmt.Errorf("read artifact verifier cgroup events: %w", err)
		}
		populated, err := parseVerifierCgroupPopulated(raw)
		if err != nil {
			return err
		}
		if !populated {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for artifact verifier cgroup to empty: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func parseVerifierCgroupPopulated(raw []byte) (bool, error) {
	found := false
	populated := false
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "populated" {
			continue
		}
		if found {
			return false, errors.New("artifact verifier cgroup events repeats populated")
		}
		found = true
		switch fields[1] {
		case "0":
			populated = false
		case "1":
			populated = true
		default:
			return false, fmt.Errorf("artifact verifier cgroup populated = %q", fields[1])
		}
	}
	if !found {
		return false, errors.New("artifact verifier cgroup events omits populated")
	}
	return populated, nil
}

func cleanupVerifierCgroup(ctx context.Context, path string, kill bool) error {
	var cleanupErr error
	if kill {
		cleanupErr = errors.Join(cleanupErr, killVerifierCgroup(path))
	}
	if err := waitVerifierCgroupEmpty(ctx, path); err != nil {
		return errors.Join(cleanupErr, err)
	}
	if err := os.Remove(path); err != nil {
		cleanupErr = errors.Join(
			cleanupErr,
			fmt.Errorf("remove artifact verifier cgroup: %w", err),
		)
	}
	return cleanupErr
}
