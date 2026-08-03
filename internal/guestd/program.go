package guestd

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

const (
	managedProgramSecretRoot       = "/var/lib/helmr/run-secrets"
	managedProgramNode             = "/opt/helmr/runtime/bin/node"
	managedProgramEntry            = "/opt/helmr/program/helmr/entry.mjs"
	managedRuntimeDescriptor       = "/var/lib/helmr/program/runtime/helmr/descriptor.json"
	maxProgramSecretPlacements     = 64
	maxProgramSecretPlaintextBytes = 128 << 20
	maxProgramSecretFrameBytes     = maxProgramSecretPlaintextBytes + 64<<10
	maxProgramControlFrameBytes    = 64 << 10
	maxProgramOutcomeFrameBytes    = maxTaskOutputBytes + 64<<10
	programAdmissionTimeout        = 30 * time.Second
	programOutputDrainTimeout      = 5 * time.Second
	programOutputPoll              = 10 * time.Millisecond
	maxTaskOutputBytes             = 16 << 20
	maxTaskErrorBytes              = 16 << 10
	maxTaskErrorMessageBytes       = 1024
)

type programProcess struct {
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	stdout        io.ReadCloser
	stderr        io.ReadCloser
	control       io.ReadCloser
	controlWriter *os.File
	proofReader   *os.File
	proofWriter   *os.File
	cgroup        programCgroup
	workspaceRoot string
	secretPaths   []string
	waitOnce      sync.Once
	waitDone      chan struct{}
	waitErr       error
	exited        atomic.Bool
}

type programEventStream struct {
	conn         programConnection
	writeTimeout time.Duration
	mu           sync.Mutex
	changed      chan struct{}
	done         chan struct{}
	rebind       chan programConnection
	replayCount  int
	closed       bool
}

type programConnection interface {
	io.ReadWriteCloser
	SetReadDeadline(deadline time.Time) error
	SetWriteDeadline(deadline time.Time) error
}

type programHostControl struct {
	pause      *runv0.CheckpointPauseRequest
	turnCommit *runv0.ActorTurnCommitPauseRequest
	decision   *runv0.ResumeDecision
	err        error
}

type programOutputPause struct {
	paused chan<- error
	resume <-chan struct{}
}

type programOutputPump struct {
	file   *os.File
	event  func([]byte) *runv0.RunEvent
	stream *programEventStream
	errors chan<- error
	pause  chan programOutputPause
	done   chan struct{}
}

type programOutputCoordinator struct {
	pumps []*programOutputPump
}

func handleProgramRunConnection(
	ctx context.Context,
	conn io.ReadWriteCloser,
	logger *slog.Logger,
	waitingRegistry *waitingRunRegistry,
	registry *workspaceOperationRegistry,
	header wire.StreamHeader,
	bodyLen uint64,
) error {
	programConn, ok := conn.(programConnection)
	if !ok {
		return errors.New("program connection does not support deadlines")
	}
	stopClose := context.AfterFunc(ctx, func() {
		_ = programConn.Close()
	})
	defer stopClose()
	admissionDeadline := time.Now().Add(programAdmissionTimeout)
	if err := programConn.SetReadDeadline(admissionDeadline); err != nil {
		return err
	}
	defer programConn.SetReadDeadline(time.Time{})
	if bodyLen != 0 {
		drainStreamBody(programConn, bodyLen)
		return fmt.Errorf("program run header body length %d is invalid", bodyLen)
	}
	runID := strings.TrimSpace(header.RunID)
	if runID == "" {
		return errors.New("program run run_id is required")
	}
	workspaceMountID := strings.TrimSpace(header.WorkspaceMountID)
	if workspaceMountID == "" {
		return errors.New("program run workspace_mount_id is required")
	}
	workspaceID := strings.TrimSpace(header.WorkspaceID)
	if workspaceID == "" {
		return errors.New("program run workspace_id is required")
	}
	var authority workspacev0.WorkspaceRunAuthority
	if err := frameio.ReadProtoFrame(programConn, &authority); err != nil {
		return fmt.Errorf("read program run authority: %w", err)
	}
	fence := authority.GetFence()
	if fence == nil {
		return errors.New("program run authority fence is required")
	}
	if strings.TrimSpace(fence.GetWorkspaceMountId()) != workspaceMountID {
		return errors.New("program run authority workspace_mount_id does not match header")
	}
	if strings.TrimSpace(fence.GetWorkspaceId()) != workspaceID {
		return errors.New("program run authority workspace_id does not match header")
	}
	entry, releaseMount, ok := registry.acquireAuthorityMount(
		workspaceMountID,
		workspaceID,
		authority.GetChannelToken(),
	)
	if !ok {
		return errors.New("program run authority is not valid for the workspace mount")
	}
	defer releaseMount()
	var request runv0.ProgramRunRequest
	if err := frameio.ReadProtoFrame(programConn, &request); err != nil {
		return fmt.Errorf("read program run request: %w", err)
	}
	if request.GetRunId() != runID {
		return errors.New("program run request run_id does not match header")
	}
	if err := validateProgramRunRequest(&request); err != nil {
		return err
	}
	if fence.GetRunId() != request.GetRunId() ||
		fence.GetAttemptNumber() != request.GetAttemptNumber() ||
		fence.GetRunLeaseId() != request.GetRunLeaseId() {
		return errors.New("program run authority does not match the program request")
	}
	deadline := time.UnixMilli(request.GetStartDeadlineUnixMs())
	deliveryDeadline := admissionDeadline
	if deadline.Before(deliveryDeadline) {
		deliveryDeadline = deadline
	}
	if err := programConn.SetReadDeadline(deliveryDeadline); err != nil {
		return err
	}
	secrets, err := readProgramSecrets(programConn, &request)
	if err != nil {
		return err
	}
	defer clearProgramSecretValues(secrets)
	releaseProgram, err := registry.admitProgram(entry, &authority, time.Now())
	if err != nil {
		return err
	}
	defer releaseProgram()
	if err := programConn.SetReadDeadline(deadline); err != nil {
		return err
	}
	process, cleanup, err := newProgramProcess(ctx, entry, &request, secrets)
	if err != nil {
		return err
	}
	defer cleanup()
	if logger != nil {
		logger.Info(
			"starting Program",
			"run_id", request.GetRunId(),
			"attempt_number", request.GetAttemptNumber(),
			"workspace_id", workspaceID,
		)
	}
	return superviseProgram(ctx, programConn, &request, process, waitingRegistry, entry)
}

func readProgramSecrets(
	reader io.Reader,
	request *runv0.ProgramRunRequest,
) ([]*runv0.ProgramSecret, error) {
	if request.GetSecretCount() > maxProgramSecretPlacements {
		return nil, fmt.Errorf(
			"program secret_count %d exceeds max %d",
			request.GetSecretCount(),
			maxProgramSecretPlacements,
		)
	}
	secrets := make(
		[]*runv0.ProgramSecret,
		0,
		int(request.GetSecretCount()),
	)
	completeSequence := false
	defer func() {
		if !completeSequence {
			clearProgramSecretValues(secrets)
		}
	}()
	var plaintextBytes uint64
	for range request.GetSecretCount() {
		var command runv0.ProgramSupervisorCommand
		if err := readProgramCommand(
			reader,
			maxProgramSecretFrameBytes,
			&command,
		); err != nil {
			return nil, fmt.Errorf("read program secret delivery: %w", err)
		}
		secret := command.GetSecretDelivery()
		if secret == nil {
			return nil, errors.New(
				"program secret delivery command is required",
			)
		}
		plaintextBytes += uint64(len(secret.GetValue()))
		if plaintextBytes > maxProgramSecretPlaintextBytes {
			return nil, errors.New(
				"program secret plaintext exceeds aggregate limit",
			)
		}
		secrets = append(secrets, secret)
	}
	var command runv0.ProgramSupervisorCommand
	if err := readProgramCommand(
		reader,
		maxProgramControlFrameBytes,
		&command,
	); err != nil {
		return nil, fmt.Errorf("read program secret completion: %w", err)
	}
	complete := command.GetSecretsComplete()
	if complete == nil {
		return nil, errors.New(
			"program secret delivery requires a completion command",
		)
	}
	if complete.GetRunId() != request.GetRunId() ||
		complete.GetAttemptNumber() != request.GetAttemptNumber() ||
		complete.GetRunLeaseId() != request.GetRunLeaseId() ||
		complete.GetSecretCount() != request.GetSecretCount() {
		return nil, errors.New(
			"program secret completion does not match execution fence",
		)
	}
	if err := validateProgramSecrets(secrets); err != nil {
		return nil, err
	}
	completeSequence = true
	return secrets, nil
}

func readProgramCommand(
	reader io.Reader,
	maxBytes uint32,
	command *runv0.ProgramSupervisorCommand,
) error {
	body, err := frameio.ReadMessageFrameBounded(reader, maxBytes)
	if err != nil {
		return err
	}
	if err := proto.Unmarshal(body, command); err != nil {
		return fmt.Errorf("unmarshal program supervisor command: %w", err)
	}
	return nil
}

func validateProgramRunRequest(request *runv0.ProgramRunRequest) error {
	if strings.TrimSpace(request.GetRunId()) == "" {
		return errors.New("program run_id is required")
	}
	if request.GetAttemptNumber() == 0 {
		return errors.New("program attempt_number is required")
	}
	if strings.TrimSpace(request.GetRunLeaseId()) == "" {
		return errors.New("program run_lease_id is required")
	}
	if request.GetSecretCount() > maxProgramSecretPlacements {
		return fmt.Errorf(
			"program secret_count %d exceeds max %d",
			request.GetSecretCount(),
			maxProgramSecretPlacements,
		)
	}
	if request.GetStartDeadlineUnixMs() <= time.Now().UnixMilli() {
		return errors.New("program start deadline has expired")
	}
	frame := request.GetProgramStartFrame()
	if len(frame) < 4 {
		return errors.New("program-start frame is truncated")
	}
	bodyLength := binary.BigEndian.Uint32(frame[:4])
	if bodyLength == 0 {
		return errors.New("program-start frame is empty")
	}
	if bodyLength > frameio.MaxFrameBytes {
		return fmt.Errorf(
			"program-start frame length %d exceeds max %d",
			bodyLength,
			frameio.MaxFrameBytes,
		)
	}
	if uint64(bodyLength)+4 != uint64(len(frame)) {
		return errors.New("program-start bytes must contain exactly one frame")
	}
	return nil
}

func validateProgramSecrets(secrets []*runv0.ProgramSecret) error {
	envNames := make(map[string]struct{}, len(secrets))
	previousPlacement := ""
	previousFile := ""
	for _, secret := range secrets {
		if secret == nil {
			return errors.New("program secret is required")
		}
		switch placement := secret.GetPlacement().(type) {
		case *runv0.ProgramSecret_Env:
			name := strings.TrimSpace(placement.Env)
			if placement.Env != name || !validEnvironmentName(name) {
				return errors.New("program secret environment placement is invalid")
			}
			if isManagedRuntimeEnvKey(name) {
				return fmt.Errorf(
					"program secret environment placement %q is reserved",
					name,
				)
			}
			if _, exists := envNames[name]; exists {
				return fmt.Errorf(
					"program secret environment placement %q is duplicated",
					name,
				)
			}
			if strings.IndexByte(string(secret.GetValue()), 0) >= 0 {
				return fmt.Errorf(
					"program secret environment placement %q contains NUL",
					name,
				)
			}
			envNames[name] = struct{}{}
			key := "env\x00" + name
			if previousPlacement >= key {
				return errors.New("program secret deliveries are not in canonical order")
			}
			previousPlacement = key
		case *runv0.ProgramSecret_File:
			if err := validateProgramSecretFilePath(placement.File); err != nil {
				return err
			}
			key := "file\x00" + placement.File
			if previousPlacement >= key {
				return errors.New("program secret deliveries are not in canonical order")
			}
			if previousFile != "" &&
				strings.HasPrefix(placement.File, previousFile+"/") {
				return errors.New("program secret file placements conflict")
			}
			previousPlacement = key
			previousFile = placement.File
		default:
			return errors.New("program secret placement is required")
		}
	}
	return nil
}

func validEnvironmentName(value string) bool {
	if value == "" {
		return false
	}
	for index, char := range value {
		if (char >= 'A' && char <= 'Z') ||
			(char >= 'a' && char <= 'z') ||
			char == '_' ||
			(index > 0 && char >= '0' && char <= '9') {
			continue
		}
		return false
	}
	return true
}

func validateProgramSecretFilePath(value string) error {
	if value == "" ||
		value != strings.TrimSpace(value) ||
		!strings.HasPrefix(value, "/") ||
		strings.ContainsRune(value, 0) ||
		path.Clean(value) != value ||
		value == "/" {
		return errors.New("program secret file placement is invalid")
	}
	if value == "/workspace" ||
		strings.HasPrefix(value, "/workspace/") ||
		value == "/var/lib/helmr" ||
		strings.HasPrefix(value, "/var/lib/helmr/") ||
		isReservedRuntimePath(value) {
		return fmt.Errorf(
			"program secret file placement %q conflicts with reserved runtime paths",
			value,
		)
	}
	return nil
}

func newProgramProcess(
	ctx context.Context,
	entry *workspaceMountEntry,
	request *runv0.ProgramRunRequest,
	secrets []*runv0.ProgramSecret,
) (*programProcess, func(), error) {
	if request == nil {
		return nil, func() {}, errors.New("program run request is required")
	}
	cgroupLeaf, err := programCgroupLeafName(
		request.GetRunId(),
		request.GetAttemptNumber(),
		request.GetRunLeaseId(),
	)
	if err != nil {
		return nil, func() {}, err
	}
	if entry.runtimeUser == nil {
		return nil, func() {}, errors.New("workspace runtime user is not resolved")
	}
	if filepath.Clean(entry.workspaceMount) != defaultRuntimeWorkdir {
		return nil, func() {}, errors.New("workspace durable root must be /workspace")
	}
	if err := prepareLaunchPath(
		entry.imageRoot,
		defaultRuntimeWorkdir,
		entry.runtimeUser,
	); err != nil {
		return nil, func() {}, fmt.Errorf("prepare program cwd: %w", err)
	}
	if err := entry.prepareWorkspaceOwner(); err != nil {
		return nil, func() {}, err
	}
	env := managedRuntimeEnv(
		entry.imageConfig,
		entry.runtimeUser,
		defaultRuntimeWorkdir,
	)
	nodeFlags, err := managedProgramNodeFlags()
	if err != nil {
		return nil, func() {}, err
	}
	secretCleanup, err := stageProgramSecrets(
		entry.imageRoot,
		secrets,
		entry.runtimeUser,
		&env,
	)
	if err != nil {
		return nil, func() {}, err
	}
	cleanupRuntime, err := mountImageRuntimeFilesystems(entry.imageRoot)
	if err != nil {
		secretCleanup()
		return nil, func() {}, err
	}
	cmd, err := imageCommand(
		ctx,
		managedProgramNode,
		append(nodeFlags, []string{
			managedProgramEntry,
		}...),
		defaultRuntimeWorkdir,
		sanitizeManagedRuntimeEnv(env),
		entry.imageRoot,
		entry.runtimeUser,
		imageCommandOptions{
			ManagedProgram:  true,
			CgroupNamespace: true,
			CgroupLeaf:      cgroupLeaf,
			StartProof:      true,
		},
	)
	if err != nil {
		cleanupRuntime()
		secretCleanup()
		return nil, func() {}, err
	}
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		cleanupRuntime()
		secretCleanup()
		return nil, func() {}, err
	}
	cmd.ExtraFiles = []*os.File{controlWriter}
	var proofReader, proofWriter *os.File
	if runtime.GOOS == "linux" {
		proofReader, proofWriter, err = os.Pipe()
		if err != nil {
			_ = controlReader.Close()
			_ = controlWriter.Close()
			cleanupRuntime()
			secretCleanup()
			return nil, func() {}, err
		}
		cmd.ExtraFiles = append(cmd.ExtraFiles, proofWriter)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		closeProgramFiles(controlReader, controlWriter, proofReader, proofWriter)
		cleanupRuntime()
		secretCleanup()
		return nil, func() {}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		closeProgramFiles(stdout, controlReader, controlWriter, proofReader, proofWriter)
		cleanupRuntime()
		secretCleanup()
		return nil, func() {}, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		closeProgramFiles(stdout, stderr, controlReader, controlWriter, proofReader, proofWriter)
		cleanupRuntime()
		secretCleanup()
		return nil, func() {}, err
	}
	cgroup, err := createProgramCgroup(cgroupLeaf)
	if err != nil {
		closeProgramFiles(stdin, stdout, stderr, controlReader, controlWriter, proofReader, proofWriter)
		cleanupRuntime()
		secretCleanup()
		return nil, func() {}, err
	}
	if err := cgroup.attach(cmd); err != nil {
		_ = cgroup.close()
		closeProgramFiles(stdin, stdout, stderr, controlReader, controlWriter, proofReader, proofWriter)
		cleanupRuntime()
		secretCleanup()
		return nil, func() {}, err
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return signalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
	}
	process := &programProcess{
		cmd:           cmd,
		stdin:         stdin,
		stdout:        stdout,
		stderr:        stderr,
		control:       controlReader,
		controlWriter: controlWriter,
		proofReader:   proofReader,
		proofWriter:   proofWriter,
		cgroup:        cgroup,
		workspaceRoot: entry.workspaceRoot,
		secretPaths:   programWorkspaceSecretPaths(entry.workspaceRoot, secrets),
		waitDone:      make(chan struct{}),
	}
	cleanup := func() {
		process.close()
		cleanupRuntime()
		secretCleanup()
	}
	return process, cleanup, nil
}

func managedProgramNodeFlags() ([]string, error) {
	file, err := os.Open(managedRuntimeDescriptor)
	if err != nil {
		return nil, fmt.Errorf("open managed runtime descriptor: %w", err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(
		file,
		(1<<20)+1,
	))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	descriptor, err := deployment.ParseRuntimeArtifactDescriptor(raw)
	if err != nil {
		return nil, fmt.Errorf("parse managed runtime descriptor: %w", err)
	}
	return append([]string(nil), descriptor.ProgramNodeFlags...), nil
}

func programWorkspaceSecretPaths(workspaceRoot string, secrets []*runv0.ProgramSecret) []string {
	paths := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		placement, ok := secret.GetPlacement().(*runv0.ProgramSecret_File)
		if !ok {
			continue
		}
		const workspacePrefix = "/workspace/"
		if !strings.HasPrefix(placement.File, workspacePrefix) {
			continue
		}
		relative := strings.TrimPrefix(placement.File, workspacePrefix)
		if relative == "" || filepath.Clean(relative) != relative {
			continue
		}
		paths = append(paths, filepath.Join(workspaceRoot, relative))
	}
	return paths
}

func stageProgramSecrets(
	imageRoot string,
	secrets []*runv0.ProgramSecret,
	runtimeUser *resolvedRuntimeUser,
	env *[]string,
) (func(), error) {
	defer clearProgramSecretValues(secrets)
	if err := validateProgramSecrets(secrets); err != nil {
		return func() {}, err
	}
	if err := os.RemoveAll(managedProgramSecretRoot); err != nil {
		return func() {}, err
	}
	if err := os.MkdirAll(managedProgramSecretRoot, 0o700); err != nil {
		return func() {}, err
	}
	cleanup := func() {
		_ = os.RemoveAll(managedProgramSecretRoot)
	}
	var targetCleanups []func()
	cleanupAll := func() {
		for index := len(targetCleanups) - 1; index >= 0; index-- {
			targetCleanups[index]()
		}
		cleanup()
	}
	for _, secret := range secrets {
		switch placement := secret.GetPlacement().(type) {
		case *runv0.ProgramSecret_Env:
			*env = setEnvValue(*env, placement.Env, string(secret.GetValue()))
		case *runv0.ProgramSecret_File:
			targetCleanup, err := prepareProgramSecretTarget(
				imageRoot,
				placement.File,
			)
			if err != nil {
				cleanupAll()
				return func() {}, err
			}
			targetCleanups = append(targetCleanups, targetCleanup)
			relative := strings.TrimPrefix(placement.File, "/")
			parent := filepath.Dir(relative)
			if parent != "." {
				if err := mkdirAllNoSymlink(
					managedProgramSecretRoot,
					filepath.ToSlash(parent),
					0o700,
				); err != nil {
					cleanupAll()
					return func() {}, err
				}
			}
			target, err := confinedLayerPath(
				managedProgramSecretRoot,
				relative,
			)
			if err != nil {
				cleanupAll()
				return func() {}, err
			}
			if err := writeFileNoFollow(target, secret.GetValue(), 0o400); err != nil {
				cleanupAll()
				return func() {}, err
			}
			if runtimeUser != nil && os.Geteuid() == 0 {
				if err := os.Chown(
					target,
					int(runtimeUser.UID),
					int(runtimeUser.GID),
				); err != nil {
					cleanupAll()
					return func() {}, err
				}
			}
		}
	}
	return cleanupAll, nil
}

func prepareProgramSecretTarget(
	imageRoot string,
	guestPath string,
) (func(), error) {
	relative := strings.TrimPrefix(guestPath, "/")
	parent := filepath.Dir(relative)
	var createdDirectories []string
	cleanupDirectories := func() {
		for _, directory := range createdDirectories {
			_ = os.Remove(directory)
		}
	}
	for current := parent; current != "." && current != ""; current = filepath.Dir(current) {
		hostPath, err := confinedLayerPath(imageRoot, current)
		if err != nil {
			cleanupDirectories()
			return func() {}, err
		}
		if _, err := os.Lstat(hostPath); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			cleanupDirectories()
			return func() {}, err
		}
		createdDirectories = append(createdDirectories, hostPath)
	}
	if parent != "." {
		if err := mkdirAllNoSymlink(
			imageRoot,
			filepath.ToSlash(parent),
			0o755,
		); err != nil {
			cleanupDirectories()
			return func() {}, err
		}
	}
	target, err := confinedLayerPath(imageRoot, relative)
	if err != nil {
		cleanupDirectories()
		return func() {}, err
	}
	info, err := os.Lstat(target)
	if err == nil {
		if !info.Mode().IsRegular() {
			cleanupDirectories()
			return func() {}, fmt.Errorf(
				"program secret target is not a regular file: %s",
				guestPath,
			)
		}
		return func() {}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		cleanupDirectories()
		return func() {}, err
	}
	file, err := os.OpenFile(
		target,
		os.O_CREATE|os.O_EXCL|os.O_WRONLY,
		0o000,
	)
	if err != nil {
		cleanupDirectories()
		return func() {}, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(target)
		cleanupDirectories()
		return func() {}, err
	}
	return func() {
		_ = os.Remove(target)
		cleanupDirectories()
	}, nil
}

func superviseProgram(
	ctx context.Context,
	conn programConnection,
	request *runv0.ProgramRunRequest,
	process *programProcess,
	registry *waitingRunRegistry,
	workspaceEntry *workspaceMountEntry,
) error {
	deadline := time.UnixMilli(request.GetStartDeadlineUnixMs())
	if err := process.start(ctx, deadline); err != nil {
		return err
	}
	completed := false
	defer func() {
		if !completed {
			process.terminate()
		}
	}()
	stream := &programEventStream{
		conn: conn, changed: make(chan struct{}), done: make(chan struct{}),
		rebind: make(chan programConnection, 1),
	}
	stopStream := context.AfterFunc(ctx, stream.closeCurrentConn)
	defer stopStream()
	if err := stream.write(&runv0.RunEvent{
		Event: &runv0.RunEvent_ProgramProcessStarted{
			ProgramProcessStarted: &runv0.ProgramProcessStarted{
				RunId:         request.GetRunId(),
				AttemptNumber: request.GetAttemptNumber(),
				RunLeaseId:    request.GetRunLeaseId(),
			},
		},
	}); err != nil {
		return err
	}
	outputErrors := make(chan error, 2)
	var outputDone sync.WaitGroup
	outputs, err := newProgramOutputCoordinator(
		stream,
		outputErrors,
		process.stdout,
		func(chunk []byte) *runv0.RunEvent {
			return &runv0.RunEvent{
				Event: &runv0.RunEvent_StdoutChunk{StdoutChunk: chunk},
			}
		},
		process.stderr,
		func(chunk []byte) *runv0.RunEvent {
			return &runv0.RunEvent{
				Event: &runv0.RunEvent_StderrChunk{StderrChunk: chunk},
			}
		},
	)
	if err != nil {
		return err
	}
	outputDone.Add(len(outputs.pumps))
	for _, pump := range outputs.pumps {
		go pump.run(&outputDone)
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	defer conn.SetReadDeadline(time.Time{})
	var command runv0.ProgramSupervisorCommand
	if err := readProgramCommand(
		conn,
		maxProgramControlFrameBytes,
		&command,
	); err != nil {
		return fmt.Errorf("read program-start release: %w", err)
	}
	startRelease := command.GetStartRelease()
	if startRelease == nil {
		return errors.New("program-start release command is required")
	}
	if startRelease.GetRunId() != request.GetRunId() ||
		startRelease.GetAttemptNumber() != request.GetAttemptNumber() ||
		startRelease.GetRunLeaseId() != request.GetRunLeaseId() {
		return errors.New("program-start release does not match execution fence")
	}
	if _, err := process.stdin.Write(request.GetProgramStartFrame()); err != nil {
		return fmt.Errorf("write program-start frame: %w", err)
	}
	request.ProgramStartFrame = nil
	var readyEvent runv0.RunEvent
	if err := readProgramReady(
		ctx,
		deadline,
		process.control,
		&readyEvent,
	); err != nil {
		return fmt.Errorf("read entrypoint-ready event: %w", err)
	}
	ready := readyEvent.GetEntrypointReady()
	if ready == nil ||
		ready.GetRunId() != request.GetRunId() ||
		ready.GetAttemptNumber() != request.GetAttemptNumber() ||
		ready.GetEntrypoint() == nil ||
		strings.TrimSpace(ready.GetEntrypoint().GetDeclaredId()) == "" ||
		ready.GetEntrypoint().GetKind() == nil {
		return errors.New("entrypoint-ready event does not match execution identity")
	}
	if err := stream.write(&readyEvent); err != nil {
		return err
	}
	command.Reset()
	if err := readProgramCommand(
		conn,
		maxProgramControlFrameBytes,
		&command,
	); err != nil {
		return fmt.Errorf("read entrypoint release: %w", err)
	}
	entrypointRelease := command.GetEntrypointRelease()
	if entrypointRelease == nil {
		return errors.New("entrypoint release command is required")
	}
	if entrypointRelease.GetRunId() != request.GetRunId() ||
		entrypointRelease.GetAttemptNumber() != request.GetAttemptNumber() ||
		!proto.Equal(
			entrypointRelease.GetEntrypoint(),
			ready.GetEntrypoint(),
		) {
		return errors.New("entrypoint release does not match ready identity")
	}
	if err := frameio.WriteProtoFrame(process.stdin, entrypointRelease); err != nil {
		return fmt.Errorf("write entrypoint release: %w", err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return err
	}
	err = relayProgram(ctx, conn, request, ready.GetEntrypoint(), process, stream, outputErrors, &outputDone, registry, outputs, workspaceEntry)
	completed = err == nil
	return err
}

func relayProgram(
	ctx context.Context,
	conn programConnection,
	request *runv0.ProgramRunRequest,
	entrypoint *runv0.EntrypointIdentity,
	process *programProcess,
	stream *programEventStream,
	outputErrors <-chan error,
	outputDone *sync.WaitGroup,
	registry *waitingRunRegistry,
	outputs *programOutputCoordinator,
	workspaceEntry *workspaceMountEntry,
) error {
	defer stream.closeCurrentConn()
	events := make(chan *runv0.RunEvent)
	controlErrors := make(chan error, 1)
	readerDone := make(chan struct{})
	defer close(readerDone)
	go func() {
		defer close(events)
		for {
			var event runv0.RunEvent
			if err := frameio.ReadProtoFrameBounded(
				process.control,
				maxProgramOutcomeFrameBytes,
				&event,
			); err != nil {
				controlErrors <- err
				return
			}
			select {
			case events <- &event:
			case <-readerDone:
				return
			}
		}
	}()
	hostControls := readProgramHostControl(conn)
	wait := make(chan error, 1)
	go func() {
		wait <- process.wait()
	}()
	var processErr error
	processExited := false
	controlClosed := false
	outcomeSeen := false
	quiesced := false
	var pendingWait *runv0.RunWaitRequested
	var pendingTurnCommit *runv0.ActorTurnCommitRequested
	var pendingPause *runv0.CheckpointPauseRequest
	pendingRuntimeOperations := make(map[string]string)
	for !processExited || !controlClosed {
		select {
		case event, ok := <-events:
			if !ok {
				controlClosed = true
				err := <-controlErrors
				events = nil
				if !errors.Is(err, io.EOF) {
					return fmt.Errorf("read program control event: %w", err)
				}
				continue
			}
			if waitRequested := event.GetRunWaitRequested(); waitRequested != nil {
				if outcomeSeen {
					return errors.New("program emitted a control event after task outcome")
				}
				if pendingWait != nil || pendingTurnCommit != nil {
					return errors.New("program emitted a concurrent consuming wait")
				}
				if strings.TrimSpace(waitRequested.GetCorrelationId()) == "" || strings.TrimSpace(waitRequested.GetKind()) == "" {
					return errors.New("program wait identity is incomplete")
				}
				if err := stream.write(event); err != nil {
					return err
				}
				pendingWait = waitRequested
				continue
			}
			if turnCommit := event.GetActorTurnCommitRequested(); turnCommit != nil {
				if outcomeSeen || entrypoint.GetActor() == nil {
					return errors.New("program emitted an actor turn commit outside live actor execution")
				}
				if pendingWait != nil || pendingTurnCommit != nil {
					return errors.New("program emitted a concurrent actor turn commit")
				}
				if strings.TrimSpace(turnCommit.GetCorrelationId()) == "" || turnCommit.GetTargetInputSequence() <= 0 {
					return errors.New("actor turn commit identity is incomplete")
				}
				if err := stream.write(event); err != nil {
					return err
				}
				pendingTurnCommit = turnCommit
				continue
			}
			if correlationID, label, ok := runtimeResourceOperationIdentity(event); ok {
				if outcomeSeen {
					return fmt.Errorf("program emitted an %s after outcome", label)
				}
				if correlationID == "" {
					return fmt.Errorf("%s identity is incomplete", label)
				}
				if _, exists := pendingRuntimeOperations[correlationID]; exists {
					return fmt.Errorf("program emitted a duplicate %s correlation", label)
				}
				if len(pendingRuntimeOperations) >= 128 {
					return errors.New("program exceeded the pending runtime operation limit")
				}
				if err := stream.write(event); err != nil {
					return err
				}
				pendingRuntimeOperations[correlationID] = label
				continue
			}
			if send := event.GetActorInputSendRequested(); send != nil {
				if outcomeSeen {
					return errors.New("program emitted an actor input send after outcome")
				}
				correlationID := strings.TrimSpace(send.GetCorrelationId())
				if correlationID == "" || strings.TrimSpace(send.GetDeclaredId()) == "" ||
					strings.TrimSpace(send.GetDataJson()) == "" {
					return errors.New("actor input send identity is incomplete")
				}
				if _, exists := pendingRuntimeOperations[correlationID]; exists {
					return errors.New("program emitted a duplicate actor input send correlation")
				}
				if len(pendingRuntimeOperations) >= 128 {
					return errors.New("program exceeded the pending runtime operation limit")
				}
				if err := stream.write(event); err != nil {
					return err
				}
				pendingRuntimeOperations[correlationID] = "actor input send"
				continue
			}
			if create := event.GetTokenCreateRequested(); create != nil {
				if outcomeSeen {
					return errors.New("program emitted a token create after outcome")
				}
				correlationID := strings.TrimSpace(create.GetCorrelationId())
				if correlationID == "" {
					return errors.New("token create identity is incomplete")
				}
				if _, exists := pendingRuntimeOperations[correlationID]; exists {
					return errors.New("program emitted a duplicate token create correlation")
				}
				if len(pendingRuntimeOperations) >= 128 {
					return errors.New("program exceeded the pending runtime operation limit")
				}
				if err := stream.write(event); err != nil {
					return err
				}
				pendingRuntimeOperations[correlationID] = "token create"
				continue
			}
			if metadata := event.GetMetadataUpdated(); metadata != nil {
				if outcomeSeen {
					return errors.New("program emitted a metadata mutation after outcome")
				}
				correlationID := strings.TrimSpace(metadata.GetCorrelationId())
				if correlationID == "" || strings.TrimSpace(metadata.GetOperation()) == "" {
					return errors.New("metadata mutation identity is incomplete")
				}
				if _, exists := pendingRuntimeOperations[correlationID]; exists {
					return errors.New("program emitted a duplicate metadata mutation correlation")
				}
				if len(pendingRuntimeOperations) >= 128 {
					return errors.New("program exceeded the pending runtime operation limit")
				}
				if err := stream.write(event); err != nil {
					return err
				}
				pendingRuntimeOperations[correlationID] = "metadata mutation"
				continue
			}
			if log := event.GetStructuredLogRequested(); log != nil {
				if outcomeSeen {
					return errors.New("program emitted a structured log after outcome")
				}
				correlationID := strings.TrimSpace(log.GetCorrelationId())
				if correlationID == "" || strings.TrimSpace(log.GetLevel()) == "" ||
					strings.TrimSpace(log.GetAttributesJson()) == "" {
					return errors.New("structured log identity is incomplete")
				}
				if _, exists := pendingRuntimeOperations[correlationID]; exists {
					return errors.New("program emitted a duplicate structured log correlation")
				}
				if len(pendingRuntimeOperations) >= 128 {
					return errors.New("program exceeded the pending runtime operation limit")
				}
				if err := stream.write(event); err != nil {
					return err
				}
				pendingRuntimeOperations[correlationID] = "structured log"
				continue
			}
			var outcomeErr error
			switch entrypoint.GetKind().(type) {
			case *runv0.EntrypointIdentity_Task:
				outcomeErr = validateTaskOutcome(event.GetTaskOutcome())
				if event.GetActorOutcome() != nil {
					outcomeErr = errors.New("task program emitted an actor outcome")
				}
			case *runv0.EntrypointIdentity_Actor:
				outcomeErr = validateActorOutcome(event.GetActorOutcome())
				if event.GetTaskOutcome() != nil {
					outcomeErr = errors.New("actor program emitted a task outcome")
				}
			default:
				outcomeErr = errors.New("program entrypoint kind is unsupported")
			}
			if outcomeSeen {
				return errors.New("program emitted more than one outcome")
			}
			if len(pendingRuntimeOperations) != 0 {
				return errors.New("program emitted an outcome with runtime operations pending")
			}
			if outcomeErr != nil {
				return outcomeErr
			}
			outcomeSeen = true
			if err := stream.write(event); err != nil {
				return err
			}
		case err := <-wait:
			processExited = true
			processErr = err
		case err := <-outputErrors:
			if err != nil {
				return err
			}
		case control := <-hostControls:
			if control.err != nil {
				if stream.hasResumeReplay() {
					hostControls = nil
					continue
				}
				return fmt.Errorf("read program host control: %w", control.err)
			}
			if control.decision != nil {
				correlationID := control.decision.GetCorrelationId()
				if operation, pending := pendingRuntimeOperations[correlationID]; pending {
					if err := validateRuntimeOperationDecision(control.decision); err != nil {
						return err
					}
					if err := frameio.WriteProtoFrame(process.stdin, control.decision); err != nil {
						return fmt.Errorf("write %s decision: %w", operation, err)
					}
					delete(pendingRuntimeOperations, correlationID)
					if pendingPause != nil && len(pendingRuntimeOperations) == 0 {
						resumed, err := pauseAndResumeProgram(
							ctx, request, pendingWait, pendingPause, process, stream,
							registry, outputs, events, controlErrors,
						)
						if err != nil {
							return err
						}
						conn = resumed
						pendingPause = nil
						pendingWait = nil
						hostControls = readProgramHostControl(resumed)
						continue
					}
					hostControls = readProgramHostControl(conn)
					continue
				}
			}
			if pendingPause != nil {
				return errors.New("program host sent non-runtime control while checkpoint pause was deferred")
			}
			if pendingTurnCommit != nil {
				if control.turnCommit == nil {
					return errors.New("program host sent non-commit control during actor turn commit")
				}
				if err := pauseActorTurnCommit(ctx, conn, request, pendingTurnCommit, control.turnCommit, process, stream, outputs, workspaceEntry); err != nil {
					return err
				}
				pendingTurnCommit = nil
				hostControls = readProgramHostControl(conn)
				continue
			}
			if pendingWait == nil {
				return errors.New("program host sent wait control without a pending wait")
			}
			switch {
			case control.decision != nil:
				if control.decision.GetRunWaitId() != pendingWait.GetCorrelationId() {
					return errors.New("immediate resume decision did not match program wait")
				}
				if err := frameio.WriteProtoFrame(process.stdin, control.decision); err != nil {
					return fmt.Errorf("write immediate program resume decision: %w", err)
				}
				pendingWait = nil
				hostControls = readProgramHostControl(conn)
			case control.pause != nil:
				if pendingPause != nil {
					return errors.New("program host sent more than one checkpoint pause request")
				}
				if len(pendingRuntimeOperations) != 0 {
					pendingPause = control.pause
					hostControls = readProgramHostControl(conn)
					continue
				}
				resumed, err := pauseAndResumeProgram(ctx, request, pendingWait, control.pause, process, stream, registry, outputs, events, controlErrors)
				if err != nil {
					return err
				}
				conn = resumed
				pendingWait = nil
				hostControls = readProgramHostControl(resumed)
			default:
				return errors.New("program host control is empty")
			}
		case rebound := <-stream.rebind:
			conn = rebound
			hostControls = readProgramHostControl(rebound)
		case <-ctx.Done():
			return ctx.Err()
		}
		if processExited && !quiesced {
			if err := process.quiesce(); err != nil {
				return err
			}
			quiesced = true
		}
	}
	if !outcomeSeen {
		return errors.New("program exited without an outcome")
	}
	if !quiesced {
		return errors.New("program process tree did not quiesce")
	}
	if err := conn.SetWriteDeadline(
		time.Now().Add(programOutputDrainTimeout),
	); err != nil {
		return err
	}
	drained := make(chan struct{})
	go func() {
		outputDone.Wait()
		close(drained)
	}()
	timer := time.NewTimer(programOutputDrainTimeout)
	defer timer.Stop()
	select {
	case <-drained:
	case <-timer.C:
		return errors.New("program output drain deadline exceeded")
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-outputErrors:
		if err != nil {
			return fmt.Errorf("forward program output: %w", err)
		}
	default:
	}
	if processErr != nil {
		return fmt.Errorf("program process exited: %w", processErr)
	}
	return stream.write(&runv0.RunEvent{
		Event: &runv0.RunEvent_ProgramQuiesced{
			ProgramQuiesced: &runv0.ProgramQuiesced{
				RunId:         request.GetRunId(),
				AttemptNumber: request.GetAttemptNumber(),
				RunLeaseId:    request.GetRunLeaseId(),
			},
		},
	})
}

func runtimeResourceOperationIdentity(event *runv0.RunEvent) (string, string, bool) {
	switch value := event.GetEvent().(type) {
	case *runv0.RunEvent_ActorStartRequested:
		return strings.TrimSpace(value.ActorStartRequested.GetCorrelationId()), "actor start", true
	case *runv0.RunEvent_ActorStatusRequested:
		return strings.TrimSpace(value.ActorStatusRequested.GetCorrelationId()), "actor status read", true
	case *runv0.RunEvent_ActorCloseRequested:
		return strings.TrimSpace(value.ActorCloseRequested.GetCorrelationId()), "actor close", true
	case *runv0.RunEvent_ActorOutputPageRequested:
		return strings.TrimSpace(value.ActorOutputPageRequested.GetCorrelationId()), "actor output page read", true
	case *runv0.RunEvent_WorkspaceCreateRequested:
		return strings.TrimSpace(value.WorkspaceCreateRequested.GetCorrelationId()), "workspace create", true
	case *runv0.RunEvent_WorkspaceRetrieveRequested:
		return strings.TrimSpace(value.WorkspaceRetrieveRequested.GetCorrelationId()), "workspace retrieve", true
	case *runv0.RunEvent_WorkspaceFileReadRequested:
		return strings.TrimSpace(value.WorkspaceFileReadRequested.GetCorrelationId()), "workspace file read", true
	case *runv0.RunEvent_WorkspaceFileStatRequested:
		return strings.TrimSpace(value.WorkspaceFileStatRequested.GetCorrelationId()), "workspace file stat", true
	case *runv0.RunEvent_WorkspaceFileListRequested:
		return strings.TrimSpace(value.WorkspaceFileListRequested.GetCorrelationId()), "workspace file list", true
	case *runv0.RunEvent_WorkspaceExecRequested:
		return strings.TrimSpace(value.WorkspaceExecRequested.GetCorrelationId()), "workspace exec", true
	case *runv0.RunEvent_WorkspaceDeleteRequested:
		return strings.TrimSpace(value.WorkspaceDeleteRequested.GetCorrelationId()), "workspace delete", true
	default:
		return "", "", false
	}
}

func readProgramHostControl(conn programConnection) <-chan programHostControl {
	result := make(chan programHostControl, 1)
	go func() {
		reader := bufio.NewReader(conn)
		prefix, err := reader.Peek(4)
		if err != nil {
			result <- programHostControl{err: err}
			return
		}
		if !frameio.IsStreamFramePrefix(prefix) {
			_, err := frameio.ReadMessageFrame(reader)
			if err == nil {
				err = errors.New("unexpected program protobuf control frame")
			}
			result <- programHostControl{err: err}
			return
		}
		header, bodyLen, err := wire.ReadStreamFrameHeader(reader)
		if err != nil {
			result <- programHostControl{err: err}
			return
		}
		switch header.Type {
		case wire.StreamTypeCheckpointPauseRequest:
			pause, err := wire.ReadCheckpointPauseRequest(header, reader, bodyLen)
			result <- programHostControl{pause: pause, err: err}
		case wire.StreamTypeResumeDecision:
			decision, err := wire.ReadResumeDecision(header, reader, bodyLen)
			result <- programHostControl{decision: decision, err: err}
		case wire.StreamTypeActorTurnCommitPause:
			request, err := wire.ReadActorTurnCommitPauseRequest(header, reader, bodyLen)
			result <- programHostControl{turnCommit: request, err: err}
		default:
			result <- programHostControl{err: fmt.Errorf("unsupported program host control %q", header.Type)}
		}
	}()
	return result
}

func pauseAndResumeProgram(
	ctx context.Context,
	run *runv0.ProgramRunRequest,
	wait *runv0.RunWaitRequested,
	pause *runv0.CheckpointPauseRequest,
	process *programProcess,
	stream *programEventStream,
	registry *waitingRunRegistry,
	outputs *programOutputCoordinator,
	events <-chan *runv0.RunEvent,
	controlErrors <-chan error,
) (programConnection, error) {
	if registry == nil {
		return nil, errors.New("waiting run registry is required")
	}
	if err := validateProgramCheckpointPause(run, wait, pause); err != nil {
		return nil, err
	}
	registration, err := registry.registerProgram(pause)
	if err != nil {
		return nil, err
	}
	retainRegistration := false
	defer func() {
		if !retainRegistration {
			registration.unregister()
		}
	}()
	if err := process.cgroup.freeze(ctx); err != nil {
		return nil, fmt.Errorf("freeze program cgroup: %w", err)
	}
	if outputs == nil {
		return nil, errors.New("program output checkpoint coordinator is required")
	}
	resumeOutputs, err := outputs.pause(ctx)
	if err != nil {
		return nil, fmt.Errorf("pause program output streams: %w", err)
	}
	outputsResumed := false
	defer func() {
		if !outputsResumed {
			resumeOutputs()
		}
	}()
	syscall.Sync()
	if pause.GetCaptureWorkspace() {
		if err := stream.writeWorkspaceArtifact(run.GetRunId(), process.workspaceRoot, process.secretPaths); err != nil {
			return nil, fmt.Errorf("capture checkpoint workspace: %w", err)
		}
	}
	ready := &runv0.CheckpointPauseReady{
		RunId:                    pause.GetRunId(),
		AttemptNumber:            pause.GetAttemptNumber(),
		RunLeaseId:               pause.GetRunLeaseId(),
		RunWaitId:                pause.GetRunWaitId(),
		CheckpointId:             pause.GetCheckpointId(),
		ResumeAttachId:           pause.GetResumeAttachId(),
		CheckpointRequestVersion: pause.GetCheckpointRequestVersion(),
		CorrelationId:            pause.GetCorrelationId(),
	}
	if err := stream.writeCheckpointPauseReady(ready); err != nil {
		return nil, fmt.Errorf("write program checkpoint pause proof: %w", err)
	}
	var resumed programConnection
	var attach *runv0.ResumeAttach
	var decision *runv0.ResumeDecision
	for decision == nil {
		attached, candidateAttach, err := registration.wait(ctx)
		if err != nil {
			return nil, fmt.Errorf("wait for program resume attach: %w", err)
		}
		candidate, ok := attached.(programConnection)
		if !ok {
			if closer, ok := attached.(io.Closer); ok {
				_ = closer.Close()
			}
			continue
		}
		decisionCtx, cancelDecision := context.WithTimeout(ctx, resumeAttachTimeout)
		candidateDecision, readErr := readResumeDecision(decisionCtx, candidate)
		cancelDecision()
		if readErr != nil {
			_ = candidate.Close()
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			continue
		}
		if candidateDecision.GetRunWaitId() != pause.GetRunWaitId() ||
			candidateDecision.GetCorrelationId() != pause.GetCorrelationId() ||
			candidateDecision.GetCheckpointId() != pause.GetCheckpointId() ||
			candidateDecision.GetResumeAttachId() != pause.GetResumeAttachId() ||
			candidateDecision.GetResumeRequestVersion() != candidateAttach.GetResumeRequestVersion() ||
			candidateDecision.GetRunLeaseId() != candidateAttach.GetRunLeaseId() ||
			validateResumeDecisionAuthority(candidateDecision) != nil || !candidateDecision.GetRequireConsumedAck() {
			_ = candidate.Close()
			continue
		}
		resumed = candidate
		attach = candidateAttach
		decision = candidateDecision
	}
	adopted := false
	defer func() {
		if !adopted {
			_ = resumed.Close()
		}
	}()
	if err := frameio.WriteProtoFrame(process.stdin, decision); err != nil {
		return nil, fmt.Errorf("stage program resume decision: %w", err)
	}
	previous, didAdopt := stream.replaceConn(resumed)
	if !didAdopt {
		return nil, errors.New("program event stream closed before resume")
	}
	adopted = true
	if previous != nil && previous != resumed {
		_ = previous.Close()
	}
	if err := process.cgroup.thaw(ctx); err != nil {
		return nil, fmt.Errorf("thaw program cgroup: %w", err)
	}
	consumed, err := awaitProgramResumeConsumed(ctx, events, controlErrors)
	if err != nil {
		return nil, err
	}
	if consumed.GetRunWaitId() != pause.GetRunWaitId() ||
		consumed.GetCheckpointId() != pause.GetCheckpointId() ||
		consumed.GetResumeAttachId() != pause.GetResumeAttachId() ||
		consumed.GetResumeRequestVersion() != attach.GetResumeRequestVersion() ||
		consumed.GetRunLeaseId() != attach.GetRunLeaseId() ||
		consumed.GetCorrelationId() != pause.GetCorrelationId() {
		return nil, errors.New("program resume-consumed proof did not match exact restore authority")
	}
	ack := &runv0.ResumeAck{
		RunWaitId: pause.GetRunWaitId(), CheckpointId: pause.GetCheckpointId(),
		ResumeAttachId: pause.GetResumeAttachId(), ResumeRequestVersion: attach.GetResumeRequestVersion(),
		RunLeaseId: attach.GetRunLeaseId(), CorrelationId: pause.GetCorrelationId(),
	}
	registration.markApplied(decision, ack)
	retainRegistration = true
	stream.retainResumeReplay(ctx, registration, decision, ack)
	if err := stream.writeResumeAck(ack); err != nil {
		_ = resumed.Close()
		select {
		case resumed = <-stream.rebind:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	resumeOutputs()
	outputsResumed = true
	return resumed, nil
}

func pauseActorTurnCommit(
	ctx context.Context,
	conn programConnection,
	run *runv0.ProgramRunRequest,
	requested *runv0.ActorTurnCommitRequested,
	pause *runv0.ActorTurnCommitPauseRequest,
	process *programProcess,
	stream *programEventStream,
	outputs *programOutputCoordinator,
	entry *workspaceMountEntry,
) error {
	if requested == nil || pause == nil ||
		pause.GetCorrelationId() != requested.GetCorrelationId() ||
		pause.GetTargetInputSequence() != requested.GetTargetInputSequence() {
		return errors.New("actor turn commit pause does not match the program request")
	}
	expectedTree := workspace.TreeIdentity{
		Digest: pause.GetExpectedTreeDigest(), SizeBytes: pause.GetExpectedTreeSizeBytes(),
		EntryCount: int(pause.GetExpectedTreeEntryCount()),
	}
	if err := workspace.ValidateTreeIdentity(expectedTree); err != nil {
		return fmt.Errorf("validate actor turn commit expected tree: %w", err)
	}
	if strings.TrimSpace(pause.GetExpectedBaseWorkspaceVersionId()) == "" {
		return errors.New("actor turn commit expected workspace version is required")
	}
	releaseBarrier, authorityExpiresAt, err := entry.acquireActorTurnCommit(run, pause)
	if err != nil {
		return err
	}
	defer releaseBarrier()
	turnCtx, cancelTurn := context.WithDeadline(ctx, authorityExpiresAt)
	defer cancelTurn()
	if err := process.cgroup.freeze(turnCtx); err != nil {
		return fmt.Errorf("freeze actor program cgroup: %w", err)
	}
	frozen := true
	defer func() {
		if frozen {
			_ = process.cgroup.thaw(context.Background())
		}
	}()
	resumeOutputs, err := outputs.pause(turnCtx)
	if err != nil {
		return fmt.Errorf("pause actor program output streams: %w", err)
	}
	outputsPaused := true
	defer func() {
		if outputsPaused {
			resumeOutputs()
		}
	}()
	artifact, cleanup, err := workspace.CreateWorkspaceArtifactFromRootWithExcludesContext(
		turnCtx,
		process.workspaceRoot,
		os.TempDir(),
		process.workspaceRoot,
		workspaceSecretExcludes(process.workspaceRoot, process.secretPaths),
	)
	if err != nil {
		return fmt.Errorf("capture actor turn workspace: %w", err)
	}
	defer cleanup()
	tree, err := workspace.InspectArtifactTreeContext(turnCtx, artifact.Path, artifact.SizeBytes)
	if err != nil {
		return fmt.Errorf("inspect actor turn workspace capture: %w", err)
	}
	changed := tree != expectedTree
	if changed {
		if err := stream.writeWorkspaceArtifactFile(run.GetRunId(), artifact); err != nil {
			return fmt.Errorf("write actor turn workspace capture: %w", err)
		}
	}
	ready := &runv0.ActorTurnCommitPauseReady{
		CorrelationId: pause.GetCorrelationId(), TargetInputSequence: pause.GetTargetInputSequence(),
		RunId: pause.GetRunId(), AttemptNumber: pause.GetAttemptNumber(), RunLeaseId: pause.GetRunLeaseId(),
		TreeDigest: tree.Digest, TreeSizeBytes: tree.SizeBytes,
		TreeEntryCount: uint32(tree.EntryCount), WorkspaceChanged: changed,
	}
	if err := stream.writeActorTurnCommitPauseReady(ready); err != nil {
		return fmt.Errorf("write actor turn commit pause proof: %w", err)
	}
	decision, err := readResumeDecision(turnCtx, conn)
	if err != nil {
		return fmt.Errorf("read actor turn commit decision: %w", err)
	}
	if (decision.GetCorrelationId() != pause.GetCorrelationId() && decision.GetRunWaitId() != pause.GetCorrelationId()) ||
		decision.GetKind() != "committed" {
		return errors.New("actor turn commit decision did not match the paused program")
	}
	var committed struct {
		WorkspaceVersionID string `json:"workspace_version_id"`
	}
	decoder := json.NewDecoder(strings.NewReader(decision.GetDataJson()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&committed); err != nil {
		return fmt.Errorf("decode actor turn commit decision: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("actor turn commit decision has trailing JSON")
	}
	if strings.TrimSpace(committed.WorkspaceVersionID) == "" {
		return errors.New("actor turn commit decision workspace version is required")
	}
	if err := entry.advanceActorTurnWorkspaceFrontierLocked(
		pause.GetExpectedBaseWorkspaceVersionId(), committed.WorkspaceVersionID,
	); err != nil {
		return err
	}
	if err := process.cgroup.thaw(turnCtx); err != nil {
		return fmt.Errorf("thaw actor program cgroup: %w", err)
	}
	frozen = false
	resumeOutputs()
	outputsPaused = false
	if err := frameio.WriteProtoFrame(process.stdin, decision); err != nil {
		return fmt.Errorf("write actor turn commit decision: %w", err)
	}
	return nil
}

func validateResumeDecisionAuthority(decision *runv0.ResumeDecision) error {
	if decision == nil {
		return errors.New("program resume decision is required")
	}
	switch decision.GetKind() {
	case "completed":
		hasResult := decision.GetDataJson() != ""
		if decision.GetNoResult() == hasResult {
			return errors.New("completed program resume decision must contain exactly one result variant")
		}
		if hasResult && !json.Valid([]byte(decision.GetDataJson())) {
			return errors.New("completed program resume result is not valid JSON")
		}
		return nil
	case "failed", "cancelled":
		if decision.GetNoResult() || decision.GetDataJson() == "" {
			return errors.New("terminal program resume failure payload is required")
		}
		var payload struct {
			ReasonCode string          `json:"reason_code"`
			Error      json.RawMessage `json:"error,omitempty"`
		}
		decoder := json.NewDecoder(strings.NewReader(decision.GetDataJson()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&payload); err != nil {
			return fmt.Errorf("decode terminal program resume failure: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return errors.New("terminal program resume failure has trailing JSON")
		}
		if strings.TrimSpace(payload.ReasonCode) == "" {
			return errors.New("terminal program resume failure reason is required")
		}
		return nil
	default:
		return fmt.Errorf("unsupported program resume decision kind %q", decision.GetKind())
	}
}

func validateRuntimeOperationDecision(decision *runv0.ResumeDecision) error {
	if decision == nil ||
		strings.TrimSpace(decision.GetCorrelationId()) == "" ||
		(decision.GetKind() != "completed" && decision.GetKind() != "failed") ||
		decision.GetRunWaitId() != "" ||
		decision.GetRequireConsumedAck() ||
		decision.GetCheckpointId() != "" ||
		decision.GetResumeAttachId() != "" ||
		decision.GetResumeRequestVersion() != 0 ||
		decision.GetRunLeaseId() != "" ||
		decision.GetNoResult() ||
		!json.Valid([]byte(decision.GetDataJson())) {
		return errors.New("runtime operation decision is invalid")
	}
	return nil
}

func awaitProgramResumeConsumed(
	ctx context.Context,
	events <-chan *runv0.RunEvent,
	controlErrors <-chan error,
) (*runv0.ResumeConsumed, error) {
	select {
	case event, ok := <-events:
		if !ok {
			select {
			case err := <-controlErrors:
				return nil, fmt.Errorf("read program resume-consumed proof: %w", err)
			default:
				return nil, errors.New("program control closed before resume-consumed proof")
			}
		}
		consumed := event.GetResumeConsumed()
		if consumed == nil {
			return nil, errors.New("program emitted an event before resume-consumed proof")
		}
		return consumed, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (stream *programEventStream) writeResumeAck(ack *runv0.ResumeAck) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.writeLocked(func(conn programConnection) error {
		return frameio.WriteProtoFrame(conn, ack)
	})
}

func validateProgramCheckpointPause(run *runv0.ProgramRunRequest, wait *runv0.RunWaitRequested, pause *runv0.CheckpointPauseRequest) error {
	if run == nil || wait == nil || pause == nil ||
		pause.GetRunId() != run.GetRunId() ||
		pause.GetAttemptNumber() != run.GetAttemptNumber() ||
		pause.GetRunLeaseId() != run.GetRunLeaseId() ||
		pause.GetCorrelationId() != wait.GetCorrelationId() ||
		strings.TrimSpace(pause.GetRunWaitId()) == "" ||
		strings.TrimSpace(pause.GetCheckpointId()) == "" ||
		strings.TrimSpace(pause.GetResumeAttachId()) == "" ||
		pause.GetCheckpointRequestVersion() <= 0 {
		return errors.New("program checkpoint pause did not match exact execution and wait authority")
	}
	return nil
}

func validateTaskOutcome(outcome *runv0.TaskOutcome) error {
	if outcome == nil {
		return errors.New("task outcome is required")
	}
	switch value := outcome.GetOutcome().(type) {
	case *runv0.TaskOutcome_Succeeded:
		if value.Succeeded == nil {
			return errors.New("task succeeded outcome is empty")
		}
		raw := []byte(value.Succeeded.GetOutputJson())
		if len(raw) == 0 || len(raw) > maxTaskOutputBytes || !utf8.Valid(raw) {
			return errors.New("task succeeded output is not bounded UTF-8 JSON")
		}
		if _, err := jsoncanon.Transform(raw); err != nil {
			return errors.New("task succeeded output is not unambiguous JSON")
		}
	case *runv0.TaskOutcome_Failed:
		if value.Failed == nil {
			return errors.New("task failed outcome is empty")
		}
		if err := validateTaskFailure(
			value.Failed.GetMessage(),
			value.Failed.DetailsJson,
		); err != nil {
			return fmt.Errorf("invalid task failure: %w", err)
		}
	case *runv0.TaskOutcome_PayloadInvalid:
		if value.PayloadInvalid == nil {
			return errors.New("task payload-invalid outcome is empty")
		}
		if err := validateTaskFailure(
			value.PayloadInvalid.GetMessage(),
			value.PayloadInvalid.DetailsJson,
		); err != nil {
			return fmt.Errorf("invalid task payload failure: %w", err)
		}
	default:
		return errors.New("task outcome variant is required")
	}
	return nil
}

func validateActorOutcome(outcome *runv0.ActorOutcome) error {
	if outcome == nil {
		return errors.New("actor outcome is required")
	}
	if outcome.TerminalInputSequence == nil || outcome.GetTerminalInputSequence() < 0 {
		return errors.New("actor terminal input sequence is negative")
	}
	switch value := outcome.GetOutcome().(type) {
	case *runv0.ActorOutcome_Succeeded:
		if value.Succeeded == nil {
			return errors.New("actor succeeded outcome is empty")
		}
	case *runv0.ActorOutcome_Failed:
		if value.Failed == nil {
			return errors.New("actor failed outcome is empty")
		}
		if err := validateTaskFailure(value.Failed.GetMessage(), value.Failed.DetailsJson); err != nil {
			return fmt.Errorf("invalid actor failure: %w", err)
		}
	default:
		return errors.New("actor outcome variant is required")
	}
	return nil
}

func validateTaskFailure(message string, details *string) error {
	if message == "" || !utf8.ValidString(message) || len(message) > maxTaskErrorMessageBytes {
		return errors.New("message is not bounded UTF-8")
	}
	value := map[string]any{"message": message}
	if details != nil {
		raw := []byte(*details)
		if len(raw) == 0 || !utf8.Valid(raw) {
			return errors.New("details_json is not valid UTF-8 JSON")
		}
		canonical, err := jsoncanon.Transform(raw)
		if err != nil {
			return errors.New("details_json is not unambiguous JSON")
		}
		var parsed any
		if err := json.Unmarshal(canonical, &parsed); err != nil {
			return errors.New("details_json is not valid JSON")
		}
		value["details"] = parsed
	}
	normalizedJSON, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal normalized error: %w", err)
	}
	normalized, err := jsoncanon.Transform(normalizedJSON)
	if err != nil {
		return fmt.Errorf("canonicalize normalized error: %w", err)
	}
	if len(normalized) > maxTaskErrorBytes {
		return errors.New("normalized error exceeds its bound")
	}
	return nil
}

func (process *programProcess) start(
	ctx context.Context,
	deadline time.Time,
) error {
	if err := process.cmd.Start(); err != nil {
		return fmt.Errorf("start program process: %w", err)
	}
	process.cmd.Env = nil
	if process.controlWriter != nil {
		_ = process.controlWriter.Close()
		process.controlWriter = nil
	}
	if process.proofWriter != nil {
		_ = process.proofWriter.Close()
		process.proofWriter = nil
	}
	if process.proofReader == nil {
		return nil
	}
	result := make(chan struct {
		body []byte
		err  error
	}, 1)
	go func() {
		proof, err := io.ReadAll(
			io.LimitReader(process.proofReader, 64*1024+1),
		)
		result <- struct {
			body []byte
			err  error
		}{body: proof, err: err}
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	var proof []byte
	var err error
	select {
	case read := <-result:
		proof = read.body
		err = read.err
	case <-timer.C:
		return errors.New("program process proof deadline exceeded")
	case <-ctx.Done():
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("read program process proof: %w", err)
	}
	if len(proof) > 64*1024 {
		return errors.New("program process proof exceeded its bound")
	}
	if len(proof) != 0 {
		return fmt.Errorf("program process setup failed: %s", proof)
	}
	return nil
}

func clearProgramSecretValues(secrets []*runv0.ProgramSecret) {
	for _, secret := range secrets {
		if secret == nil {
			continue
		}
		for index := range secret.Value {
			secret.Value[index] = 0
		}
		secret.Value = nil
	}
}

func signalProcessGroup(pgid int, signal syscall.Signal) error {
	err := syscall.Kill(-pgid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func readProgramReady(
	ctx context.Context,
	deadline time.Time,
	reader io.Reader,
	event *runv0.RunEvent,
) error {
	result := make(chan error, 1)
	go func() {
		result <- frameio.ReadProtoFrameBounded(
			reader,
			maxProgramControlFrameBytes,
			event,
		)
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-timer.C:
		return errors.New("entrypoint-ready deadline exceeded")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (process *programProcess) terminate() {
	_ = process.stdin.Close()
	if process.cgroup != nil {
		_ = process.cgroup.kill()
	}
	if process.cmd.Process != nil && !process.exited.Load() {
		_ = signalProcessGroup(process.cmd.Process.Pid, syscall.SIGKILL)
	}
}

func (process *programProcess) quiesce() error {
	if process.cgroup == nil {
		return errors.New("program cgroup is required")
	}
	if err := process.cgroup.kill(); err != nil {
		return fmt.Errorf("kill program cgroup: %w", err)
	}
	if err := process.cgroup.waitEmpty(); err != nil {
		return fmt.Errorf("wait for program cgroup: %w", err)
	}
	return nil
}

func (process *programProcess) close() {
	process.terminate()
	if process.cmd.Process != nil {
		_ = process.wait()
	}
	closeProgramFiles(
		process.stdout,
		process.stderr,
		process.control,
		process.controlWriter,
		process.proofReader,
		process.proofWriter,
	)
	if process.cgroup != nil {
		_ = process.cgroup.waitEmpty()
		_ = process.cgroup.close()
		process.cgroup = nil
	}
}

func (process *programProcess) wait() error {
	process.waitOnce.Do(func() {
		process.waitErr = process.cmd.Wait()
		process.exited.Store(true)
		if process.waitDone == nil {
			process.waitDone = make(chan struct{})
		}
		close(process.waitDone)
	})
	<-process.waitDone
	return process.waitErr
}

func closeProgramFiles(files ...io.Closer) {
	for _, file := range files {
		if file != nil {
			_ = file.Close()
		}
	}
}

func (stream *programEventStream) write(event *runv0.RunEvent) error {
	for {
		stream.mu.Lock()
		if stream.closed || stream.conn == nil {
			stream.mu.Unlock()
			return errors.New("program event stream is closed")
		}
		conn := stream.conn
		changed := stream.changed
		timeout := stream.writeTimeout
		if timeout <= 0 {
			timeout = programOutputDrainTimeout
		}
		err := conn.SetWriteDeadline(time.Now().Add(timeout))
		if err == nil {
			err = frameio.WriteProtoFrame(conn, event)
			_ = conn.SetWriteDeadline(time.Time{})
		}
		stream.mu.Unlock()
		if err == nil {
			return nil
		}
		if changed == nil || stream.done == nil {
			return err
		}
		select {
		case <-changed:
			continue
		case <-stream.done:
			return err
		}
	}
}

func (stream *programEventStream) replaceConn(conn programConnection) (programConnection, bool) {
	stream.mu.Lock()
	previous := stream.conn
	if stream.closed {
		stream.mu.Unlock()
		_ = conn.Close()
		return previous, false
	}
	stream.conn = conn
	if stream.changed != nil {
		close(stream.changed)
	}
	stream.changed = make(chan struct{})
	stream.mu.Unlock()
	return previous, true
}

func (stream *programEventStream) replaceConnWithResumeAck(conn programConnection, ack *runv0.ResumeAck) (programConnection, error) {
	stream.mu.Lock()
	if stream.closed || stream.conn == nil {
		stream.mu.Unlock()
		_ = conn.Close()
		return nil, errors.New("program event stream is closed")
	}
	previous := stream.conn
	stream.conn = conn
	err := stream.writeLocked(func(current programConnection) error {
		return frameio.WriteProtoFrame(current, ack)
	})
	if err != nil {
		stream.conn = previous
		stream.mu.Unlock()
		_ = conn.Close()
		return previous, err
	}
	if stream.changed != nil {
		close(stream.changed)
	}
	stream.changed = make(chan struct{})
	stream.mu.Unlock()
	return previous, nil
}

func (stream *programEventStream) writeLocked(write func(programConnection) error) error {
	if stream.closed || stream.conn == nil {
		return errors.New("program event stream is closed")
	}
	if err := stream.conn.SetWriteDeadline(time.Now().Add(programAdmissionTimeout)); err != nil {
		return err
	}
	err := write(stream.conn)
	_ = stream.conn.SetWriteDeadline(time.Time{})
	return err
}

func (stream *programEventStream) closeCurrentConn() {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.closed {
		return
	}
	stream.closed = true
	if stream.done != nil {
		close(stream.done)
	}
	if stream.changed != nil {
		close(stream.changed)
	}
	if stream.conn != nil {
		_ = stream.conn.Close()
		stream.conn = nil
	}
}

func (stream *programEventStream) hasResumeReplay() bool {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.replayCount > 0
}

func (stream *programEventStream) retainResumeReplay(
	ctx context.Context,
	registration waitingRunRegistration,
	decision *runv0.ResumeDecision,
	ack *runv0.ResumeAck,
) {
	stream.mu.Lock()
	stream.replayCount++
	stream.mu.Unlock()
	go func() {
		defer registration.unregister()
		defer func() {
			stream.mu.Lock()
			stream.replayCount--
			stream.mu.Unlock()
		}()
		for {
			attached, attach, err := registration.waitStream(ctx, stream.done)
			if err != nil {
				return
			}
			resumed, ok := attached.(programConnection)
			if !ok {
				continue
			}
			replayed, err := readResumeDecisionUntilDone(resumed, stream.done)
			if err != nil || !proto.Equal(replayed, decision) ||
				!proto.Equal(attach, registration.slot.accepted) {
				_ = resumed.Close()
				continue
			}
			previous, err := stream.replaceConnWithResumeAck(resumed, ack)
			if err != nil {
				continue
			}
			if previous != nil && previous != resumed {
				_ = previous.Close()
			}
			select {
			case stream.rebind <- resumed:
			case <-stream.done:
				return
			}
		}
	}()
}

func readResumeDecisionUntilDone(conn programConnection, done <-chan struct{}) (*runv0.ResumeDecision, error) {
	type result struct {
		decision *runv0.ResumeDecision
		err      error
	}
	read := make(chan result, 1)
	if err := conn.SetReadDeadline(time.Now().Add(resumeAttachTimeout)); err != nil {
		return nil, err
	}
	defer conn.SetReadDeadline(time.Time{})
	go func() {
		decision, err := readResumeDecision(context.Background(), conn)
		read <- result{decision: decision, err: err}
	}()
	select {
	case result := <-read:
		return result.decision, result.err
	case <-done:
		_ = conn.Close()
		return nil, errors.New("program event stream closed during resume replay")
	}
}

func readResumeDecision(
	ctx context.Context,
	reader io.Reader,
) (*runv0.ResumeDecision, error) {
	type result struct {
		decision *runv0.ResumeDecision
		err      error
	}
	read := make(chan result, 1)
	go func() {
		var decision runv0.ResumeDecision
		err := frameio.ReadProtoFrame(reader, &decision)
		read <- result{decision: &decision, err: err}
	}()
	select {
	case result := <-read:
		return result.decision, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func workspaceSecretExcludes(
	workspaceRoot string,
	secretPaths []string,
) []string {
	workspaceRoot = filepath.Clean(workspaceRoot)
	patterns := make([]string, 0, len(secretPaths)*2)
	for _, secretPath := range secretPaths {
		relative, err := filepath.Rel(
			workspaceRoot,
			filepath.Clean(secretPath),
		)
		if err != nil ||
			relative == "." ||
			relative == ".." ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		relative = filepath.ToSlash(relative)
		patterns = append(patterns, relative, relative+"/**")
	}
	return patterns
}

func (stream *programEventStream) writeCheckpointPauseReady(ready *runv0.CheckpointPauseReady) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.writeLocked(func(conn programConnection) error {
		return wire.WriteCheckpointPauseReady(conn, ready)
	})
}

func (stream *programEventStream) writeActorTurnCommitPauseReady(ready *runv0.ActorTurnCommitPauseReady) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.writeLocked(func(conn programConnection) error {
		return wire.WriteActorTurnCommitPauseReady(conn, ready)
	})
}

func (stream *programEventStream) writeWorkspaceArtifact(runID, workspaceRoot string, secretPaths []string) error {
	if strings.TrimSpace(workspaceRoot) == "" {
		return errors.New("program workspace root is required")
	}
	artifact, cleanup, err := workspace.CreateWorkspaceArtifactFromRootWithExcludes(
		workspaceRoot,
		os.TempDir(),
		workspaceRoot,
		workspaceSecretExcludes(workspaceRoot, secretPaths),
	)
	if err != nil {
		return err
	}
	defer cleanup()
	return stream.writeWorkspaceArtifactFile(runID, artifact)
}

func (stream *programEventStream) writeWorkspaceArtifactFile(runID string, artifact workspace.WorkspaceArtifact) error {
	entryCount := artifact.EntryCount
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.writeLocked(func(conn programConnection) error {
		return wire.WriteFileFrameWithMetadata(conn, wire.StreamHeader{
			Type:       wire.StreamTypeWorkspaceArtifact,
			RunID:      runID,
			EntryCount: &entryCount,
		}, artifact.Path, artifact.Digest, artifact.SizeBytes)
	})
}

func newProgramOutputCoordinator(
	stream *programEventStream,
	errorChannel chan<- error,
	stdout io.ReadCloser,
	stdoutEvent func([]byte) *runv0.RunEvent,
	stderr io.ReadCloser,
	stderrEvent func([]byte) *runv0.RunEvent,
) (*programOutputCoordinator, error) {
	readers := []struct {
		name   string
		reader io.ReadCloser
		event  func([]byte) *runv0.RunEvent
	}{
		{name: "stdout", reader: stdout, event: stdoutEvent},
		{name: "stderr", reader: stderr, event: stderrEvent},
	}
	coordinator := &programOutputCoordinator{pumps: make([]*programOutputPump, 0, len(readers))}
	for _, output := range readers {
		file, ok := output.reader.(*os.File)
		if !ok || file == nil {
			return nil, fmt.Errorf("program %s pipe is not an OS file", output.name)
		}
		fd, err := unix.Dup(int(file.Fd()))
		if err != nil {
			for _, pump := range coordinator.pumps {
				_ = pump.file.Close()
			}
			return nil, fmt.Errorf("duplicate program %s pipe: %w", output.name, err)
		}
		pumpFile := os.NewFile(uintptr(fd), "helmr-program-"+output.name)
		if err := unix.SetNonblock(fd, true); err != nil {
			_ = pumpFile.Close()
			for _, pump := range coordinator.pumps {
				_ = pump.file.Close()
			}
			return nil, fmt.Errorf("make program %s nonblocking: %w", output.name, err)
		}
		coordinator.pumps = append(coordinator.pumps, &programOutputPump{
			file: pumpFile, event: output.event, stream: stream,
			errors: errorChannel, pause: make(chan programOutputPause), done: make(chan struct{}),
		})
	}
	return coordinator, nil
}

func (coordinator *programOutputCoordinator) pause(ctx context.Context) (func(), error) {
	if coordinator == nil {
		return func() {}, errors.New("program output coordinator is required")
	}
	resumeChannels := make([]chan struct{}, 0, len(coordinator.pumps))
	paused := make(chan error, len(coordinator.pumps))
	pending := 0
	resume := func() {
		for _, channel := range resumeChannels {
			close(channel)
		}
	}
	for _, pump := range coordinator.pumps {
		resumeChannel := make(chan struct{})
		select {
		case pump.pause <- programOutputPause{paused: paused, resume: resumeChannel}:
			resumeChannels = append(resumeChannels, resumeChannel)
			pending++
		case <-pump.done:
		case <-ctx.Done():
			resume()
			return func() {}, ctx.Err()
		}
	}
	for range pending {
		select {
		case err := <-paused:
			if err != nil {
				resume()
				return func() {}, err
			}
		case <-ctx.Done():
			resume()
			return func() {}, ctx.Err()
		}
	}
	var once sync.Once
	return func() { once.Do(resume) }, nil
}

func (pump *programOutputPump) run(done *sync.WaitGroup) {
	defer done.Done()
	defer close(pump.done)
	defer pump.file.Close()
	buffer := make([]byte, 32*1024)
	for {
		select {
		case request := <-pump.pause:
			err := pump.drain(buffer)
			request.paused <- err
			if err != nil {
				pump.errors <- err
				return
			}
			<-request.resume
			continue
		default:
		}
		count, err := unix.Read(int(pump.file.Fd()), buffer)
		if count > 0 {
			if writeErr := pump.write(buffer[:count]); writeErr != nil {
				pump.errors <- writeErr
				return
			}
		}
		if errors.Is(err, io.EOF) || count == 0 && err == nil {
			return
		}
		if err != nil && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EWOULDBLOCK) {
			pump.errors <- err
			return
		}
		if count <= 0 {
			timer := time.NewTimer(programOutputPoll)
			select {
			case request := <-pump.pause:
				timer.Stop()
				err := pump.drain(buffer)
				request.paused <- err
				if err != nil {
					pump.errors <- err
					return
				}
				<-request.resume
			case <-timer.C:
			}
		}
	}
}

func (pump *programOutputPump) drain(buffer []byte) error {
	for {
		count, err := unix.Read(int(pump.file.Fd()), buffer)
		if count > 0 {
			if writeErr := pump.write(buffer[:count]); writeErr != nil {
				return writeErr
			}
		}
		if errors.Is(err, io.EOF) || count == 0 && err == nil {
			return nil
		}
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (pump *programOutputPump) write(body []byte) error {
	chunk := append([]byte(nil), body...)
	return pump.stream.write(pump.event(chunk))
}
