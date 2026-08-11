//go:build linux

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"golang.org/x/sys/unix"
)

const (
	platformAcquisitionOutputBytes = 64 << 10
	platformAcquisitionDrain       = 10 * time.Second
	platformAcquisitionAggregate   = "platform-acquisitions"
	platformAcquisitionProcessLeaf = "process"
)

var platformAcquisitionCgroupLimits = map[string]string{
	"cpu.max":          "100000 100000",
	"memory.max":       "4294967296",
	"memory.oom.group": "1",
	"memory.swap.max":  "0",
	"pids.max":         "64",
}

func runPlatformAcquisitionProcess(
	ctx context.Context,
	process PlatformAcquisitionProcess,
	request workerapi.PlatformAcquisition,
) (_ PlatformAcquisitionProcessResult, returnErr error) {
	if err := validatePlatformAcquisitionProcess(process, request); err != nil {
		return PlatformAcquisitionProcessResult{}, err
	}
	input, err := json.Marshal(request)
	if err != nil {
		return PlatformAcquisitionProcessResult{}, err
	}
	cgroupPath, processCgroup, err := createPlatformAcquisitionCgroup(
		process.UnitCgroupRoot,
		request.DeploymentID,
	)
	if err != nil {
		return PlatformAcquisitionProcessResult{}, err
	}
	defer func() {
		returnErr = errors.Join(
			returnErr,
			processCgroup.Close(),
			cleanupPlatformAcquisitionCgroup(cgroupPath),
		)
	}()

	stdout := &platformAcquisitionOutput{remaining: platformAcquisitionOutputBytes}
	stderr := &platformAcquisitionOutput{remaining: platformAcquisitionOutputBytes}
	command := exec.CommandContext(ctx, process.Executable, "__platform-acquire")
	command.Dir = "/"
	command.Env = platformAcquisitionEnvironment(process, cgroupPath)
	command.Stdin = bytes.NewReader(input)
	command.Stdout = stdout
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{
		UseCgroupFD: true,
		CgroupFD:    int(processCgroup.Fd()),
	}
	command.Cancel = func() error {
		return killPlatformAcquisitionCgroup(cgroupPath)
	}
	command.WaitDelay = platformAcquisitionDrain
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return PlatformAcquisitionProcessResult{}, fmt.Errorf(
				"platform acquisition deadline: %w",
				ctx.Err(),
			)
		}
		return PlatformAcquisitionProcessResult{}, fmt.Errorf(
			"platform acquisition child: %w: %s",
			err,
			strings.TrimSpace(stderr.String()),
		)
	}
	if stdout.exceeded || stderr.exceeded {
		return PlatformAcquisitionProcessResult{}, errors.New("platform acquisition child output is excessive")
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	var result PlatformAcquisitionProcessResult
	if err := decoder.Decode(&result); err != nil {
		return PlatformAcquisitionProcessResult{}, fmt.Errorf("decode platform acquisition child result: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return PlatformAcquisitionProcessResult{}, err
	}
	return result, nil
}

func validatePlatformAcquisitionProcess(
	process PlatformAcquisitionProcess,
	request workerapi.PlatformAcquisition,
) error {
	if _, err := ids.Parse(request.DeploymentID); err != nil {
		return errors.New("platform acquisition deployment ID is invalid")
	}
	for name, value := range map[string]string{
		"build policy":     process.BuildPolicyPath,
		"encoder":          process.Encoder,
		"executable":       process.Executable,
		"GPG verifier":     process.GPGV,
		"unit cgroup root": process.UnitCgroupRoot,
		"work directory":   process.WorkDir,
		"XZ decoder":       process.XZ,
	} {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("platform acquisition %s path is invalid", name)
		}
	}
	if process.PlatformStoreURI == "" {
		return errors.New("platform acquisition store URI is missing")
	}
	return nil
}

func platformAcquisitionEnvironment(
	process PlatformAcquisitionProcess,
	cgroupPath string,
) []string {
	environment := []string{
		"HELMR_PLATFORM_ACQUISITION_BUILD_POLICY=" + process.BuildPolicyPath,
		"HELMR_PLATFORM_ACQUISITION_ENCODER=" + process.Encoder,
		"HELMR_PLATFORM_ACQUISITION_GPGV=" + process.GPGV,
		"HELMR_PLATFORM_ACQUISITION_STORE=" + process.PlatformStoreURI,
		"HELMR_PLATFORM_ACQUISITION_UNIT_CGROUP=" + cgroupPath,
		"HELMR_PLATFORM_ACQUISITION_WORK_DIR=" + process.WorkDir,
		"HELMR_PLATFORM_ACQUISITION_XZ=" + process.XZ,
	}
	for _, name := range []string{
		"AWS_CA_BUNDLE",
		"AWS_DEFAULT_REGION",
		"AWS_EC2_METADATA_SERVICE_ENDPOINT",
		"AWS_REGION",
		"HTTPS_PROXY",
		"NO_PROXY",
		"SSL_CERT_DIR",
		"SSL_CERT_FILE",
	} {
		if value, exists := os.LookupEnv(name); exists {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

type platformAcquisitionOutput struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	remaining int
	exceeded  bool
}

func (output *platformAcquisitionOutput) Write(value []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	original := len(value)
	if len(value) > output.remaining {
		value = value[:output.remaining]
		output.exceeded = true
	}
	_, _ = output.buffer.Write(value)
	output.remaining -= len(value)
	return original, nil
}

func (output *platformAcquisitionOutput) Bytes() []byte {
	output.mu.Lock()
	defer output.mu.Unlock()
	return bytes.Clone(output.buffer.Bytes())
}

func (output *platformAcquisitionOutput) String() string {
	return string(output.Bytes())
}

func createPlatformAcquisitionCgroup(
	unitRoot string,
	deploymentID string,
) (string, *os.File, error) {
	root := filepath.Clean(unitRoot)
	if !filepath.IsAbs(root) {
		return "", nil, errors.New("platform acquisition unit cgroup root is invalid")
	}
	identity, err := ids.Parse(deploymentID)
	if err != nil {
		return "", nil, errors.New("platform acquisition deployment ID is invalid")
	}
	leaf := "platform-acquisition-" + identity.String() + "-" + uuid.Must(uuid.NewV7()).String()
	rootFD, err := unix.Open(
		root,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return "", nil, fmt.Errorf("open platform acquisition cgroup root: %w", err)
	}
	defer unix.Close(rootFD)
	if err := unix.Mkdirat(rootFD, platformAcquisitionAggregate, 0o755); err != nil &&
		!errors.Is(err, unix.EEXIST) {
		return "", nil, fmt.Errorf("create platform acquisition aggregate cgroup: %w", err)
	}
	aggregateFD, err := unix.Openat(
		rootFD,
		platformAcquisitionAggregate,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return "", nil, fmt.Errorf("open platform acquisition aggregate cgroup: %w", err)
	}
	defer unix.Close(aggregateFD)
	for name, value := range platformAcquisitionCgroupLimits {
		if err := writePlatformAcquisitionCgroupControl(aggregateFD, name, value); err != nil {
			return "", nil, err
		}
	}
	if err := writePlatformAcquisitionCgroupControl(
		aggregateFD,
		"cgroup.subtree_control",
		"+cpu +memory +pids",
	); err != nil {
		return "", nil, err
	}

	if err := unix.Mkdirat(aggregateFD, leaf, 0o755); err != nil {
		return "", nil, fmt.Errorf("create platform acquisition cgroup: %w", err)
	}
	cgroupFD, err := unix.Openat(
		aggregateFD,
		leaf,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		_ = unix.Unlinkat(aggregateFD, leaf, unix.AT_REMOVEDIR)
		return "", nil, fmt.Errorf("open platform acquisition cgroup: %w", err)
	}
	cgroupPath := filepath.Join(root, platformAcquisitionAggregate, leaf)
	cleanup := func() {
		unix.Close(cgroupFD)
		_ = os.Remove(filepath.Join(cgroupPath, platformAcquisitionProcessLeaf))
		_ = unix.Unlinkat(aggregateFD, leaf, unix.AT_REMOVEDIR)
	}
	if err := writePlatformAcquisitionCgroupControl(
		cgroupFD,
		"cgroup.subtree_control",
		"+cpu +memory +pids",
	); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := unix.Mkdirat(cgroupFD, platformAcquisitionProcessLeaf, 0o755); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("create platform acquisition process cgroup: %w", err)
	}
	processFD, err := unix.Openat(
		cgroupFD,
		platformAcquisitionProcessLeaf,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("open platform acquisition process cgroup: %w", err)
	}
	if err := unix.Close(cgroupFD); err != nil {
		unix.Close(processFD)
		_ = os.Remove(filepath.Join(cgroupPath, platformAcquisitionProcessLeaf))
		_ = unix.Unlinkat(aggregateFD, leaf, unix.AT_REMOVEDIR)
		return "", nil, err
	}
	return cgroupPath, os.NewFile(uintptr(processFD), cgroupPath), nil
}

func writePlatformAcquisitionCgroupControl(
	cgroupFD int,
	name string,
	value string,
) error {
	controlFD, err := unix.Openat(
		cgroupFD,
		name,
		unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return fmt.Errorf("open platform acquisition %s: %w", name, err)
	}
	control := os.NewFile(uintptr(controlFD), name)
	count, writeErr := io.WriteString(control, value)
	if writeErr == nil && count != len(value) {
		writeErr = io.ErrShortWrite
	}
	return errors.Join(writeErr, control.Close())
}

func killPlatformAcquisitionCgroup(path string) error {
	err := os.WriteFile(filepath.Join(path, "cgroup.kill"), []byte("1"), 0)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func cleanupPlatformAcquisitionCgroup(path string) error {
	killErr := killPlatformAcquisitionCgroup(path)
	ctx, cancel := context.WithTimeout(context.Background(), platformAcquisitionDrain)
	defer cancel()
	for {
		raw, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
		if err != nil {
			if os.IsNotExist(err) {
				return killErr
			}
			return errors.Join(killErr, err)
		}
		if strings.Contains(string(raw), "populated 0\n") {
			entries, readErr := os.ReadDir(path)
			if readErr != nil {
				return errors.Join(killErr, readErr)
			}
			var removeErr error
			for _, entry := range entries {
				if entry.IsDir() {
					removeErr = errors.Join(removeErr, os.Remove(filepath.Join(path, entry.Name())))
				}
			}
			return errors.Join(killErr, removeErr, os.Remove(path))
		}
		select {
		case <-ctx.Done():
			return errors.Join(killErr, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("platform acquisition child result has trailing JSON")
		}
		return fmt.Errorf("decode platform acquisition child trailing data: %w", err)
	}
	return nil
}
