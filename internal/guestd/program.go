package guestd

import (
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

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

const (
	managedProgramSecretRoot       = "/var/lib/helmr/run-secrets"
	managedProgramNode             = "/opt/helmr/runtime/bin/node"
	managedProgramEntry            = "/opt/helmr/program/helmr/entry.mjs"
	managedProgramPreload          = "file:///opt/helmr/runtime/helmr/preload.mjs"
	maxProgramSecretPlacements     = 64
	maxProgramSecretPlaintextBytes = 128 << 20
	maxProgramSecretFrameBytes     = maxProgramSecretPlaintextBytes + 64<<10
	maxProgramControlFrameBytes    = 64 << 10
	maxProgramOutcomeFrameBytes    = maxTaskOutputBytes + 64<<10
	programAdmissionTimeout        = 30 * time.Second
	programOutputDrainTimeout      = 5 * time.Second
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
	waitOnce      sync.Once
	waitDone      chan struct{}
	waitErr       error
	exited        atomic.Bool
}

type programEventStream struct {
	conn         programConnection
	writeTimeout time.Duration
	mu           sync.Mutex
}

type programConnection interface {
	io.ReadWriteCloser
	SetReadDeadline(deadline time.Time) error
	SetWriteDeadline(deadline time.Time) error
}

func handleProgramRunConnection(
	ctx context.Context,
	conn io.ReadWriteCloser,
	logger *slog.Logger,
	registry *workspaceOperationRegistry,
	header wire.StreamHeader,
	bodyLen uint64,
) error {
	programConn, ok := conn.(programConnection)
	if !ok {
		return errors.New("Program connection does not support deadlines")
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
		return fmt.Errorf("Program run header body length %d is invalid", bodyLen)
	}
	runID := strings.TrimSpace(header.RunID)
	if runID == "" {
		return errors.New("Program run run_id is required")
	}
	workspaceMountID := strings.TrimSpace(header.WorkspaceMountID)
	if workspaceMountID == "" {
		return errors.New("Program run workspace_mount_id is required")
	}
	workspaceID := strings.TrimSpace(header.WorkspaceID)
	if workspaceID == "" {
		return errors.New("Program run workspace_id is required")
	}
	var envelope workspacev0.WorkspaceOperationEnvelope
	if err := frameio.ReadProtoFrame(programConn, &envelope); err != nil {
		return fmt.Errorf("read Program run envelope: %w", err)
	}
	if strings.TrimSpace(envelope.GetWorkspaceMountId()) != workspaceMountID {
		return errors.New("Program run envelope workspace_mount_id does not match header")
	}
	if strings.TrimSpace(envelope.GetWorkspaceId()) != workspaceID {
		return errors.New("Program run envelope workspace_id does not match header")
	}
	if strings.TrimSpace(envelope.GetWriteLeaseId()) == "" {
		return errors.New("Program run write_lease_id is required")
	}
	if strings.TrimSpace(envelope.GetFencingToken()) == "" {
		return errors.New("Program run fencing_token is required")
	}
	entry, releaseMount, ok := registry.acquire(
		workspaceMountID,
		workspaceID,
		envelope.GetChannelToken(),
		envelope.GetFencingGeneration(),
	)
	if !ok {
		return errors.New("Program run channel token or fencing generation is invalid")
	}
	defer releaseMount()
	var request runv0.ProgramRunRequest
	if err := frameio.ReadProtoFrame(programConn, &request); err != nil {
		return fmt.Errorf("read Program run request: %w", err)
	}
	if request.GetRunId() != runID {
		return errors.New("Program run request run_id does not match header")
	}
	if err := validateProgramRunRequest(&request); err != nil {
		return err
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
	releaseProgram, ok := registry.claimProgram()
	if !ok {
		return errors.New("Workspace already has an active managed Program")
	}
	defer releaseProgram()
	if err := programConn.SetReadDeadline(deadline); err != nil {
		return err
	}
	process, cleanup, err := newProgramProcess(ctx, entry, secrets)
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
	return superviseProgram(ctx, programConn, &request, process)
}

func readProgramSecrets(
	reader io.Reader,
	request *runv0.ProgramRunRequest,
) ([]*runv0.ProgramSecret, error) {
	if request.GetSecretCount() > maxProgramSecretPlacements {
		return nil, fmt.Errorf(
			"Program secret_count %d exceeds max %d",
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
			return nil, fmt.Errorf("read Program Secret delivery: %w", err)
		}
		secret := command.GetSecretDelivery()
		if secret == nil {
			return nil, errors.New(
				"Program Secret delivery command is required",
			)
		}
		plaintextBytes += uint64(len(secret.GetValue()))
		if plaintextBytes > maxProgramSecretPlaintextBytes {
			return nil, errors.New(
				"Program Secret plaintext exceeds aggregate limit",
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
		return nil, fmt.Errorf("read Program Secret completion: %w", err)
	}
	complete := command.GetSecretsComplete()
	if complete == nil {
		return nil, errors.New(
			"Program Secret delivery requires a completion command",
		)
	}
	if complete.GetRunId() != request.GetRunId() ||
		complete.GetAttemptNumber() != request.GetAttemptNumber() ||
		complete.GetRunLeaseId() != request.GetRunLeaseId() ||
		complete.GetSecretCount() != request.GetSecretCount() {
		return nil, errors.New(
			"Program Secret completion does not match execution fence",
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
		return fmt.Errorf("unmarshal Program supervisor command: %w", err)
	}
	return nil
}

func validateProgramRunRequest(request *runv0.ProgramRunRequest) error {
	if strings.TrimSpace(request.GetRunId()) == "" {
		return errors.New("Program run_id is required")
	}
	if request.GetAttemptNumber() == 0 {
		return errors.New("Program attempt_number is required")
	}
	if strings.TrimSpace(request.GetRunLeaseId()) == "" {
		return errors.New("Program run_lease_id is required")
	}
	if request.GetSecretCount() > maxProgramSecretPlacements {
		return fmt.Errorf(
			"Program secret_count %d exceeds max %d",
			request.GetSecretCount(),
			maxProgramSecretPlacements,
		)
	}
	if request.GetStartDeadlineUnixMs() <= time.Now().UnixMilli() {
		return errors.New("Program start deadline has expired")
	}
	frame := request.GetProgramStartFrame()
	if len(frame) < 4 {
		return errors.New("Program-start frame is truncated")
	}
	bodyLength := binary.BigEndian.Uint32(frame[:4])
	if bodyLength == 0 {
		return errors.New("Program-start frame is empty")
	}
	if bodyLength > frameio.MaxFrameBytes {
		return fmt.Errorf(
			"Program-start frame length %d exceeds max %d",
			bodyLength,
			frameio.MaxFrameBytes,
		)
	}
	if uint64(bodyLength)+4 != uint64(len(frame)) {
		return errors.New("Program-start bytes must contain exactly one frame")
	}
	return nil
}

func validateProgramSecrets(secrets []*runv0.ProgramSecret) error {
	envNames := make(map[string]struct{}, len(secrets))
	previousPlacement := ""
	previousFile := ""
	for _, secret := range secrets {
		if secret == nil {
			return errors.New("Program Secret is required")
		}
		switch placement := secret.GetPlacement().(type) {
		case *runv0.ProgramSecret_Env:
			name := strings.TrimSpace(placement.Env)
			if placement.Env != name || !validEnvironmentName(name) {
				return errors.New("Program Secret environment placement is invalid")
			}
			if isManagedRuntimeEnvKey(name) {
				return fmt.Errorf(
					"Program Secret environment placement %q is reserved",
					name,
				)
			}
			if _, exists := envNames[name]; exists {
				return fmt.Errorf(
					"Program Secret environment placement %q is duplicated",
					name,
				)
			}
			if strings.IndexByte(string(secret.GetValue()), 0) >= 0 {
				return fmt.Errorf(
					"Program Secret environment placement %q contains NUL",
					name,
				)
			}
			envNames[name] = struct{}{}
			key := "env\x00" + name
			if previousPlacement >= key {
				return errors.New("Program Secret deliveries are not in canonical order")
			}
			previousPlacement = key
		case *runv0.ProgramSecret_File:
			if err := validateProgramSecretFilePath(placement.File); err != nil {
				return err
			}
			key := "file\x00" + placement.File
			if previousPlacement >= key {
				return errors.New("Program Secret deliveries are not in canonical order")
			}
			if previousFile != "" &&
				strings.HasPrefix(placement.File, previousFile+"/") {
				return errors.New("Program Secret file placements conflict")
			}
			previousPlacement = key
			previousFile = placement.File
		default:
			return errors.New("Program Secret placement is required")
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
		return errors.New("Program Secret file placement is invalid")
	}
	if value == "/workspace" ||
		strings.HasPrefix(value, "/workspace/") ||
		value == "/var/lib/helmr" ||
		strings.HasPrefix(value, "/var/lib/helmr/") ||
		isReservedRuntimePath(value) {
		return fmt.Errorf(
			"Program Secret file placement %q conflicts with reserved runtime paths",
			value,
		)
	}
	return nil
}

func newProgramProcess(
	ctx context.Context,
	entry *workspaceMountEntry,
	secrets []*runv0.ProgramSecret,
) (*programProcess, func(), error) {
	if entry.runtimeUser == nil {
		return nil, func() {}, errors.New("Workspace runtime user is not resolved")
	}
	if filepath.Clean(entry.workspaceMount) != defaultRuntimeWorkdir {
		return nil, func() {}, errors.New("Workspace durable root must be /workspace")
	}
	if err := prepareLaunchPath(
		entry.imageRoot,
		defaultRuntimeWorkdir,
		entry.runtimeUser,
	); err != nil {
		return nil, func() {}, fmt.Errorf("prepare Program cwd: %w", err)
	}
	if err := entry.prepareWorkspaceOwner(); err != nil {
		return nil, func() {}, err
	}
	env := managedRuntimeEnv(
		entry.imageConfig,
		entry.runtimeUser,
		defaultRuntimeWorkdir,
	)
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
	cmd, err := adapterCommand(
		ctx,
		managedProgramNode,
		[]string{
			"--experimental-transform-types",
			"--import=" + managedProgramPreload,
			managedProgramEntry,
		},
		defaultRuntimeWorkdir,
		sanitizeManagedRuntimeEnv(env),
		entry.imageRoot,
		entry.runtimeUser,
		adapterCommandOptions{
			ImageMode:       true,
			ManagedProgram:  true,
			CgroupNamespace: true,
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
	cgroup, err := createProgramCgroup()
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
		return signalAdapterProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
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
		waitDone:      make(chan struct{}),
	}
	cleanup := func() {
		process.close()
		cleanupRuntime()
		secretCleanup()
	}
	return process, cleanup, nil
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
				"Program Secret target is not a regular file: %s",
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
	stream := &programEventStream{conn: conn}
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
	outputDone.Add(2)
	go forwardProgramOutput(
		process.stdout,
		func(chunk []byte) *runv0.RunEvent {
			return &runv0.RunEvent{
				Event: &runv0.RunEvent_StdoutChunk{StdoutChunk: chunk},
			}
		},
		stream,
		outputErrors,
		&outputDone,
	)
	go forwardProgramOutput(
		process.stderr,
		func(chunk []byte) *runv0.RunEvent {
			return &runv0.RunEvent{
				Event: &runv0.RunEvent_StderrChunk{StderrChunk: chunk},
			}
		},
		stream,
		outputErrors,
		&outputDone,
	)
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
		return fmt.Errorf("read Program-start release: %w", err)
	}
	startRelease := command.GetStartRelease()
	if startRelease == nil {
		return errors.New("Program-start release command is required")
	}
	if startRelease.GetRunId() != request.GetRunId() ||
		startRelease.GetAttemptNumber() != request.GetAttemptNumber() ||
		startRelease.GetRunLeaseId() != request.GetRunLeaseId() {
		return errors.New("Program-start release does not match execution fence")
	}
	if _, err := process.stdin.Write(request.GetProgramStartFrame()); err != nil {
		return fmt.Errorf("write Program-start frame: %w", err)
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
	err := relayProgram(ctx, conn, request, process, stream, outputErrors, &outputDone)
	completed = err == nil
	return err
}

func relayProgram(
	ctx context.Context,
	conn programConnection,
	request *runv0.ProgramRunRequest,
	process *programProcess,
	stream *programEventStream,
	outputErrors <-chan error,
	outputDone *sync.WaitGroup,
) error {
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
	connectionErrors := make(chan error, 1)
	go func() {
		_, err := frameio.ReadMessageFrame(conn)
		if err == nil {
			err = errors.New("unexpected Program control frame")
		}
		connectionErrors <- err
	}()
	wait := make(chan error, 1)
	go func() {
		wait <- process.wait()
	}()
	var processErr error
	processExited := false
	controlClosed := false
	outcomeSeen := false
	quiesced := false
	for !processExited || !controlClosed {
		select {
		case event, ok := <-events:
			if !ok {
				controlClosed = true
				err := <-controlErrors
				events = nil
				if !errors.Is(err, io.EOF) {
					return fmt.Errorf("read Program control event: %w", err)
				}
				continue
			}
			outcome := event.GetTaskOutcome()
			if outcome == nil {
				if outcomeSeen {
					return errors.New("Program emitted a control event after Task outcome")
				}
				return errors.New("Program emitted an unsupported post-entrypoint event")
			}
			if outcomeSeen {
				return errors.New("Program emitted more than one Task outcome")
			}
			if err := validateTaskOutcome(outcome); err != nil {
				return err
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
		case err := <-connectionErrors:
			return fmt.Errorf("Program connection closed: %w", err)
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
		return errors.New("Program exited without a Task outcome")
	}
	if !quiesced {
		return errors.New("Program process tree did not quiesce")
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
		return errors.New("Program output drain deadline exceeded")
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-outputErrors:
		if err != nil {
			return fmt.Errorf("forward Program output: %w", err)
		}
	default:
	}
	if processErr != nil {
		return fmt.Errorf("Program process exited: %w", processErr)
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

func validateTaskOutcome(outcome *runv0.TaskOutcome) error {
	if outcome == nil {
		return errors.New("Task outcome is required")
	}
	switch value := outcome.GetOutcome().(type) {
	case *runv0.TaskOutcome_Succeeded:
		if value.Succeeded == nil {
			return errors.New("Task succeeded outcome is empty")
		}
		raw := []byte(value.Succeeded.GetOutputJson())
		if len(raw) == 0 || len(raw) > maxTaskOutputBytes || !utf8.Valid(raw) {
			return errors.New("Task succeeded output is not bounded UTF-8 JSON")
		}
		if _, err := jsoncanon.Transform(raw); err != nil {
			return errors.New("Task succeeded output is not unambiguous JSON")
		}
	case *runv0.TaskOutcome_Failed:
		if value.Failed == nil {
			return errors.New("Task failed outcome is empty")
		}
		if err := validateTaskFailure(
			value.Failed.GetMessage(),
			value.Failed.DetailsJson,
		); err != nil {
			return fmt.Errorf("invalid Task failure: %w", err)
		}
	case *runv0.TaskOutcome_PayloadInvalid:
		if value.PayloadInvalid == nil {
			return errors.New("Task payload-invalid outcome is empty")
		}
		if err := validateTaskFailure(
			value.PayloadInvalid.GetMessage(),
			value.PayloadInvalid.DetailsJson,
		); err != nil {
			return fmt.Errorf("invalid Task payload failure: %w", err)
		}
	default:
		return errors.New("Task outcome variant is required")
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
		return fmt.Errorf("start Program process: %w", err)
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
		return errors.New("Program process proof deadline exceeded")
	case <-ctx.Done():
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("read Program process proof: %w", err)
	}
	if len(proof) > 64*1024 {
		return errors.New("Program process proof exceeded its bound")
	}
	if len(proof) != 0 {
		return fmt.Errorf("Program process setup failed: %s", proof)
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
		_ = signalAdapterProcessGroup(process.cmd.Process.Pid, syscall.SIGKILL)
	}
}

func (process *programProcess) quiesce() error {
	if process.cgroup == nil {
		return errors.New("Program cgroup is required")
	}
	if err := process.cgroup.kill(); err != nil {
		return fmt.Errorf("kill Program cgroup: %w", err)
	}
	if err := process.cgroup.waitEmpty(); err != nil {
		return fmt.Errorf("wait for Program cgroup: %w", err)
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
	stream.mu.Lock()
	defer stream.mu.Unlock()
	timeout := stream.writeTimeout
	if timeout <= 0 {
		timeout = programOutputDrainTimeout
	}
	if err := stream.conn.SetWriteDeadline(
		time.Now().Add(timeout),
	); err != nil {
		return err
	}
	defer stream.conn.SetWriteDeadline(time.Time{})
	return frameio.WriteProtoFrame(stream.conn, event)
}

func forwardProgramOutput(
	reader io.Reader,
	event func([]byte) *runv0.RunEvent,
	stream *programEventStream,
	errorChannel chan<- error,
	done *sync.WaitGroup,
) {
	defer done.Done()
	buffer := make([]byte, 32*1024)
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			chunk := append([]byte(nil), buffer[:count]...)
			if writeErr := stream.write(event(chunk)); writeErr != nil {
				errorChannel <- writeErr
				return
			}
		}
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			errorChannel <- err
			return
		}
	}
}
