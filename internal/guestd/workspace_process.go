package guestd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/transport"
)

const workspaceProcessChunkBytes = 64 * 1024
const workspacePtyCloseKillAfter = 2 * time.Second
const workspaceProcessStopWait = 5 * time.Second

type workspaceProcess struct {
	resourceKind string
	resourceID   string
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	pty          *os.File
	outputDone   sync.WaitGroup
	inputMu      sync.Mutex
	inputCursor  uint64
	inputChunks  map[uint64][]byte
	inputClosed  bool
	done         chan struct{}
}

type workspaceExecStartRequest struct {
	ExecID         string            `json:"exec_id"`
	Command        []string          `json:"command"`
	Cwd            string            `json:"cwd"`
	EnvShape       map[string]string `json:"env_shape"`
	FilesystemMode string            `json:"filesystem_mode"`
	Detached       bool              `json:"detached"`
}

type workspacePtyCreateRequest struct {
	PtyID          string `json:"pty_id"`
	Cwd            string `json:"cwd"`
	Cols           int32  `json:"cols"`
	Rows           int32  `json:"rows"`
	FilesystemMode string `json:"filesystem_mode"`
}

type workspacePtyResizeRequest struct {
	PtyID string `json:"pty_id"`
	Cols  int32  `json:"cols"`
	Rows  int32  `json:"rows"`
}

type workspacePtyCloseRequest struct {
	PtyID string `json:"pty_id"`
}

func (entry *workspaceMaterializationEntry) startWorkspaceExec(envelope *workspacev0.WorkspaceOperationEnvelope, requestJSON string) error {
	var request workspaceExecStartRequest
	if err := json.Unmarshal([]byte(requestJSON), &request); err != nil {
		return fmt.Errorf("decode StartExec request: %w", err)
	}
	execID := strings.TrimSpace(request.ExecID)
	if execID == "" {
		return errors.New("StartExec exec_id is required")
	}
	if entry.getWorkspaceProcess("exec", execID) != nil {
		return nil
	}
	if request.FilesystemMode == "read" {
		return errors.New("workspace_read_only_unsupported")
	}
	if len(request.Command) == 0 {
		return errors.New("StartExec command is required")
	}
	for _, arg := range request.Command {
		if strings.Contains(arg, "\x00") {
			return errors.New("StartExec command contains NUL")
		}
	}
	launchCwd, err := entry.workspaceLaunchCwd(request.Cwd)
	if err != nil {
		return err
	}
	env, err := entry.workspaceProcessEnv(launchCwd, request.EnvShape)
	if err != nil {
		return err
	}
	runtimePath, err := entry.workspaceRuntimePath(request.Command[0], launchCwd, env)
	if err != nil {
		return err
	}
	if err := prepareLaunchPath(entry.imageRoot, launchCwd, entry.runtimeUser); err != nil {
		return fmt.Errorf("prepare exec cwd: %w", err)
	}
	if err := entry.prepareWorkspaceOwner(); err != nil {
		return err
	}
	cmd, err := adapterCommand(context.Background(), runtimePath, request.Command[1:], launchCwd, env, entry.imageRoot, entry.runtimeUser, adapterCommandOptions{ImageMode: true})
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open exec stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("open exec stderr: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open exec stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		entry.emitExecError(envelope, execID, err)
		return fmt.Errorf("start exec: %w", err)
	}
	process := &workspaceProcess{resourceKind: "exec", resourceID: execID, cmd: cmd, stdin: stdin, done: make(chan struct{})}
	if err := entry.registerWorkspaceProcess(process); err != nil {
		_ = signalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
		_ = stdin.Close()
		_ = closeReadCloser(stdout)
		_ = closeReadCloser(stderr)
		_ = cmd.Wait()
		return err
	}
	entry.emit(&workspacev0.WorkspaceOperationEvent{
		Envelope: envelope,
		Event:    &workspacev0.WorkspaceOperationEvent_ExecStarted{ExecStarted: &workspacev0.WorkspaceExecStarted{ExecId: execID, ProcessId: fmt.Sprintf("%d", cmd.Process.Pid)}},
	})
	process.outputDone.Add(2)
	go entry.copyExecOutput(envelope, process, stdout, true)
	go entry.copyExecOutput(envelope, process, stderr, false)
	go entry.waitExec(envelope, process)
	return nil
}

func (entry *workspaceMaterializationEntry) createWorkspacePty(envelope *workspacev0.WorkspaceOperationEnvelope, requestJSON string) error {
	var request workspacePtyCreateRequest
	if err := json.Unmarshal([]byte(requestJSON), &request); err != nil {
		return fmt.Errorf("decode CreatePty request: %w", err)
	}
	ptyID := strings.TrimSpace(request.PtyID)
	if ptyID == "" {
		return errors.New("CreatePty pty_id is required")
	}
	if entry.getWorkspaceProcess("pty", ptyID) != nil {
		return nil
	}
	if request.FilesystemMode == "read" {
		return errors.New("workspace_read_only_unsupported")
	}
	cols, rows := normalizeWorkspacePtySize(request.Cols, request.Rows)
	launchCwd, err := entry.workspaceLaunchCwd(request.Cwd)
	if err != nil {
		return err
	}
	env, err := entry.workspaceProcessEnv(launchCwd, nil)
	if err != nil {
		return err
	}
	shell, err := entry.workspaceRuntimePath("sh", launchCwd, env)
	if err != nil {
		return err
	}
	if err := prepareLaunchPath(entry.imageRoot, launchCwd, entry.runtimeUser); err != nil {
		return fmt.Errorf("prepare pty cwd: %w", err)
	}
	if err := entry.prepareWorkspaceOwner(); err != nil {
		return err
	}
	cmd, err := adapterCommand(context.Background(), shell, []string{"-l"}, launchCwd, env, entry.imageRoot, entry.runtimeUser, adapterCommandOptions{ImageMode: true, Pty: true})
	if err != nil {
		return err
	}
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		entry.emitPtyError(envelope, ptyID, err)
		return fmt.Errorf("start pty: %w", err)
	}
	process := &workspaceProcess{resourceKind: "pty", resourceID: ptyID, cmd: cmd, pty: tty, stdin: tty, done: make(chan struct{})}
	if err := entry.registerWorkspaceProcess(process); err != nil {
		_ = signalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
		_ = tty.Close()
		return err
	}
	entry.emit(&workspacev0.WorkspaceOperationEvent{
		Envelope: envelope,
		Event:    &workspacev0.WorkspaceOperationEvent_PtyOpened{PtyOpened: &workspacev0.WorkspacePtyOpened{PtyId: ptyID, ProcessId: fmt.Sprintf("%d", cmd.Process.Pid), Cols: uint32(cols), Rows: uint32(rows)}},
	})
	process.outputDone.Add(1)
	go entry.copyPtyOutput(envelope, process, tty)
	go entry.waitPty(envelope, process)
	return nil
}

func (entry *workspaceMaterializationEntry) resizeWorkspacePty(requestJSON string) error {
	var request workspacePtyResizeRequest
	if err := json.Unmarshal([]byte(requestJSON), &request); err != nil {
		return fmt.Errorf("decode ResizePty request: %w", err)
	}
	process := entry.getWorkspaceProcess("pty", request.PtyID)
	if process == nil || process.pty == nil {
		return fmt.Errorf("workspace pty %q is not open", strings.TrimSpace(request.PtyID))
	}
	cols, rows := normalizeWorkspacePtySize(request.Cols, request.Rows)
	if err := pty.Setsize(process.pty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}); err != nil {
		return err
	}
	entry.emit(&workspacev0.WorkspaceOperationEvent{
		Event: &workspacev0.WorkspaceOperationEvent_PtyResizeApplied{PtyResizeApplied: &workspacev0.WorkspacePtyResizeApplied{PtyId: strings.TrimSpace(request.PtyID), Cols: uint32(cols), Rows: uint32(rows)}},
	})
	return nil
}

func (entry *workspaceMaterializationEntry) closeWorkspacePty(requestJSON string) error {
	var request workspacePtyCloseRequest
	if err := json.Unmarshal([]byte(requestJSON), &request); err != nil {
		return fmt.Errorf("decode ClosePty request: %w", err)
	}
	process := entry.getWorkspaceProcess("pty", request.PtyID)
	if process == nil {
		return fmt.Errorf("workspace pty %q is not open", strings.TrimSpace(request.PtyID))
	}
	if err := signalProcessGroup(process.cmd.Process.Pid, syscall.SIGTERM); err != nil {
		return err
	}
	go forceKillWorkspacePtyAfter(process.done, process.cmd.Process.Pid, workspacePtyCloseKillAfter)
	return nil
}

func forceKillWorkspacePtyAfter(done <-chan struct{}, pgid int, delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
		select {
		case <-done:
			return
		default:
			_ = signalProcessGroup(pgid, syscall.SIGKILL)
		}
	}
}

func handleWorkspaceInputConnection(ctx context.Context, conn io.ReadWriter, registry *workspaceOperationRegistry) error {
	var envelope workspacev0.WorkspaceOperationEnvelope
	if err := transport.ReadProtoFrame(conn, &envelope); err != nil {
		return fmt.Errorf("read workspace input envelope: %w", err)
	}
	entry, release, ok := registry.acquire(envelope.MaterializationId, envelope.WorkspaceId, envelope.ChannelToken, envelope.FencingGeneration)
	if !ok {
		return errors.New("workspace input channel token or fencing generation is invalid")
	}
	defer release()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		var frame workspacev0.WorkspaceInputFrame
		if err := transport.ReadProtoFrame(conn, &frame); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read workspace input frame: %w", err)
		}
		switch payload := frame.GetFrame().(type) {
		case *workspacev0.WorkspaceInputFrame_Chunk:
			input := payload.Chunk
			if input == nil {
				return errors.New("workspace input chunk frame is empty")
			}
			if len(input.GetData()) == 0 {
				return errors.New("workspace input chunk data is required")
			}
			ackOffset, err := entry.writeWorkspaceInput(input.ResourceKind, input.ResourceId, input.GetOffsetStart(), input.Data)
			if err != nil {
				return err
			}
			if err := transport.WriteProtoFrame(conn, &workspacev0.WorkspaceStreamAck{
				Envelope:      &envelope,
				ResourceKind:  input.ResourceKind,
				ResourceId:    input.ResourceId,
				Stream:        input.Stream,
				DurableOffset: ackOffset,
			}); err != nil {
				return fmt.Errorf("write workspace input ack: %w", err)
			}
		case *workspacev0.WorkspaceInputFrame_Close:
			input := payload.Close
			if input == nil {
				return errors.New("workspace input close frame is empty")
			}
			if err := entry.closeWorkspaceInput(input.ResourceKind, input.ResourceId, input.Offset); err != nil {
				return err
			}
			if err := transport.WriteProtoFrame(conn, &workspacev0.WorkspaceStreamAck{
				Envelope:      &envelope,
				ResourceKind:  input.ResourceKind,
				ResourceId:    input.ResourceId,
				Stream:        input.Stream,
				DurableOffset: input.Offset,
			}); err != nil {
				return fmt.Errorf("write workspace input close ack: %w", err)
			}
		default:
			return errors.New("workspace input frame is required")
		}
	}
}

func (entry *workspaceMaterializationEntry) writeWorkspaceInput(kind string, id string, offset uint64, data []byte) (uint64, error) {
	process := entry.getWorkspaceProcess(workspaceInputProcessKind(kind), id)
	if process == nil || process.stdin == nil {
		return offset, errors.New("workspace input target is not available")
	}
	return process.writeInput(offset, data)
}

func (entry *workspaceMaterializationEntry) closeWorkspaceInput(kind string, id string, offset uint64) error {
	if workspaceInputProcessKind(kind) == "pty" {
		return errors.New("workspace pty input close is unsupported")
	}
	process := entry.getWorkspaceProcess(workspaceInputProcessKind(kind), id)
	if process == nil || process.stdin == nil {
		return errors.New("workspace input target is not available")
	}
	return process.closeInput(offset)
}

func (process *workspaceProcess) writeInput(offset uint64, data []byte) (uint64, error) {
	process.inputMu.Lock()
	defer process.inputMu.Unlock()
	if process.inputClosed {
		return process.inputCursor, errors.New("workspace input is closed")
	}
	if process.inputChunks == nil {
		process.inputChunks = map[uint64][]byte{}
	}
	if existing, ok := process.inputChunks[offset]; ok {
		if bytes.Equal(existing, data) {
			return offset + uint64(len(data)), nil
		}
		return process.inputCursor, errors.New("workspace input offset conflict")
	}
	if offset != process.inputCursor {
		return process.inputCursor, fmt.Errorf("workspace input offset conflict: got %d want %d", offset, process.inputCursor)
	}
	if _, err := process.stdin.Write(data); err != nil {
		return process.inputCursor, err
	}
	process.inputChunks[offset] = append([]byte(nil), data...)
	process.inputCursor += uint64(len(data))
	process.trimInputChunksLocked()
	return process.inputCursor, nil
}

func (process *workspaceProcess) closeInput(offset uint64) error {
	process.inputMu.Lock()
	defer process.inputMu.Unlock()
	if process.inputClosed {
		return nil
	}
	if offset != process.inputCursor {
		return fmt.Errorf("workspace input close offset conflict: got %d want %d", offset, process.inputCursor)
	}
	process.inputClosed = true
	return process.stdin.Close()
}

func (process *workspaceProcess) trimInputChunksLocked() {
	const retainedInputChunks = 1024
	if len(process.inputChunks) <= retainedInputChunks {
		return
	}
	offsets := make([]uint64, 0, len(process.inputChunks))
	for offset := range process.inputChunks {
		offsets = append(offsets, offset)
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	for _, offset := range offsets {
		delete(process.inputChunks, offset)
		if len(process.inputChunks) <= retainedInputChunks {
			return
		}
	}
}

func workspaceInputProcessKind(kind string) string {
	switch strings.TrimSpace(kind) {
	case "workspace_exec", "exec":
		return "exec"
	case "workspace_pty", "pty":
		return "pty"
	default:
		return strings.TrimSpace(kind)
	}
}

func (entry *workspaceMaterializationEntry) copyExecOutput(envelope *workspacev0.WorkspaceOperationEnvelope, process *workspaceProcess, reader io.Reader, stdout bool) {
	defer process.outputDone.Done()
	buf := make([]byte, workspaceProcessChunkBytes)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			if stdout {
				entry.emit(&workspacev0.WorkspaceOperationEvent{Envelope: envelope, Event: &workspacev0.WorkspaceOperationEvent_ExecStdoutChunk{ExecStdoutChunk: &workspacev0.WorkspaceExecOutputChunk{ExecId: process.resourceID, Data: data}}})
			} else {
				entry.emit(&workspacev0.WorkspaceOperationEvent{Envelope: envelope, Event: &workspacev0.WorkspaceOperationEvent_ExecStderrChunk{ExecStderrChunk: &workspacev0.WorkspaceExecOutputChunk{ExecId: process.resourceID, Data: data}}})
			}
		}
		if err != nil {
			return
		}
	}
}

func (entry *workspaceMaterializationEntry) copyPtyOutput(envelope *workspacev0.WorkspaceOperationEnvelope, process *workspaceProcess, reader io.Reader) {
	defer process.outputDone.Done()
	buf := make([]byte, workspaceProcessChunkBytes)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			entry.emit(&workspacev0.WorkspaceOperationEvent{
				Envelope: envelope,
				Event:    &workspacev0.WorkspaceOperationEvent_PtyOutputChunk{PtyOutputChunk: &workspacev0.WorkspacePtyOutputChunk{PtyId: process.resourceID, Data: append([]byte(nil), buf[:n]...)}},
			})
		}
		if err != nil {
			return
		}
	}
}

func (entry *workspaceMaterializationEntry) waitExec(envelope *workspacev0.WorkspaceOperationEnvelope, process *workspaceProcess) {
	process.outputDone.Wait()
	err := process.cmd.Wait()
	entry.unregisterWorkspaceProcess(process)
	exitCode, signal, errJSON := workspaceProcessExit(err)
	if errJSON != "" {
		entry.emit(&workspacev0.WorkspaceOperationEvent{Envelope: envelope, Event: &workspacev0.WorkspaceOperationEvent_ExecError{ExecError: &workspacev0.WorkspaceExecError{ExecId: process.resourceID, ErrorJson: errJSON}}})
		close(process.done)
		return
	}
	entry.emit(&workspacev0.WorkspaceOperationEvent{Envelope: envelope, Event: &workspacev0.WorkspaceOperationEvent_ExecExited{ExecExited: &workspacev0.WorkspaceExecExited{ExecId: process.resourceID, ExitCode: exitCode, Signal: signal}}})
	close(process.done)
}

func (entry *workspaceMaterializationEntry) waitPty(envelope *workspacev0.WorkspaceOperationEnvelope, process *workspaceProcess) {
	process.outputDone.Wait()
	err := process.cmd.Wait()
	if process.pty != nil {
		_ = process.pty.Close()
	}
	entry.unregisterWorkspaceProcess(process)
	_, signal, errJSON := workspaceProcessExit(err)
	if errJSON != "" {
		entry.emit(&workspacev0.WorkspaceOperationEvent{Envelope: envelope, Event: &workspacev0.WorkspaceOperationEvent_PtyError{PtyError: &workspacev0.WorkspacePtyError{PtyId: process.resourceID, ErrorJson: errJSON}}})
		close(process.done)
		return
	}
	reason := "exit"
	if signal != "" {
		reason = "signal:" + signal
	}
	entry.emit(&workspacev0.WorkspaceOperationEvent{Envelope: envelope, Event: &workspacev0.WorkspaceOperationEvent_PtyClosed{PtyClosed: &workspacev0.WorkspacePtyClosed{PtyId: process.resourceID, Reason: reason}}})
	close(process.done)
}

func (entry *workspaceMaterializationEntry) registerWorkspaceProcess(process *workspaceProcess) error {
	entry.processesMu.Lock()
	defer entry.processesMu.Unlock()
	if entry.processes == nil {
		entry.processes = map[string]*workspaceProcess{}
	}
	key := workspaceProcessKey(process.resourceKind, process.resourceID)
	if _, exists := entry.processes[key]; exists {
		return fmt.Errorf("%s %s is already running", process.resourceKind, process.resourceID)
	}
	entry.processes[key] = process
	return nil
}

func (entry *workspaceMaterializationEntry) getWorkspaceProcess(kind string, id string) *workspaceProcess {
	entry.processesMu.Lock()
	defer entry.processesMu.Unlock()
	return entry.processes[workspaceProcessKey(kind, id)]
}

func (entry *workspaceMaterializationEntry) unregisterWorkspaceProcess(process *workspaceProcess) {
	entry.processesMu.Lock()
	defer entry.processesMu.Unlock()
	delete(entry.processes, workspaceProcessKey(process.resourceKind, process.resourceID))
}

func (entry *workspaceMaterializationEntry) stopWorkspaceProcesses() {
	entry.processesMu.Lock()
	processes := make([]*workspaceProcess, 0, len(entry.processes))
	for _, process := range entry.processes {
		processes = append(processes, process)
	}
	entry.processesMu.Unlock()
	for _, process := range processes {
		if process.cmd != nil && process.cmd.Process != nil {
			_ = signalProcessGroup(process.cmd.Process.Pid, syscall.SIGKILL)
		}
		if process.stdin != nil {
			_ = process.stdin.Close()
		}
		if process.pty != nil {
			_ = process.pty.Close()
		}
	}
	timer := time.NewTimer(workspaceProcessStopWait)
	defer timer.Stop()
	for _, process := range processes {
		select {
		case <-process.done:
		case <-timer.C:
			return
		}
	}
}

func workspaceProcessKey(kind string, id string) string {
	return strings.TrimSpace(kind) + ":" + strings.TrimSpace(id)
}

func closeReadCloser(reader io.Reader) error {
	closer, ok := reader.(io.Closer)
	if !ok || closer == nil {
		return nil
	}
	return closer.Close()
}

func (entry *workspaceMaterializationEntry) workspaceLaunchCwd(raw string) (string, error) {
	return resolveLaunchCwd(raw, entry.workspaceMount)
}

func (entry *workspaceMaterializationEntry) workspaceProcessEnv(launchCwd string, userEnv map[string]string) ([]string, error) {
	if entry.runtimeUser == nil {
		return nil, errors.New("workspace runtime user is not resolved")
	}
	env := imageRuntimeEnv(entry.imageConfig, entry.runtimeUser, launchCwd)
	for key, value := range userEnv {
		if strings.Contains(key, "\x00") || strings.Contains(value, "\x00") {
			return nil, fmt.Errorf("env %q contains NUL", key)
		}
		env = setEnvValue(env, key, value)
	}
	return env, nil
}

func (entry *workspaceMaterializationEntry) prepareWorkspaceOwner() error {
	if entry.runtimeUser == nil || os.Geteuid() != 0 {
		return nil
	}
	if err := chownTree(entry.workspaceRoot, entry.runtimeUser.UID, entry.runtimeUser.GID); err != nil {
		return fmt.Errorf("prepare workspace owner: %w", err)
	}
	return nil
}

func (entry *workspaceMaterializationEntry) workspaceRuntimePath(command string, launchCwd string, env []string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", errors.New("command is required")
	}
	if strings.Contains(command, "\x00") {
		return "", errors.New("command contains NUL")
	}
	if strings.Contains(command, "/") {
		if strings.HasPrefix(command, "/") {
			return path.Clean(command), nil
		}
		return path.Clean(path.Join(launchCwd, command)), nil
	}
	searchPath := workspaceEnvValue(env, "PATH")
	if strings.TrimSpace(searchPath) == "" {
		searchPath = defaultRuntimePath
	}
	for _, dir := range strings.Split(searchPath, ":") {
		if dir == "" {
			dir = "."
		}
		candidate := path.Clean(path.Join(dir, command))
		if !strings.HasPrefix(candidate, "/") {
			candidate = path.Clean(path.Join(launchCwd, candidate))
		}
		hostPath, err := confinedLayerPath(entry.imageRoot, strings.TrimPrefix(candidate, "/"))
		if err != nil {
			continue
		}
		if isExecutableFile(hostPath) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("command %q not found in image PATH", command)
}

func workspaceEnvValue(env []string, key string) string {
	for _, entry := range env {
		entryKey, value, ok := strings.Cut(entry, "=")
		if ok && entryKey == key {
			return value
		}
	}
	return ""
}

func normalizeWorkspacePtySize(cols int32, rows int32) (int32, int32) {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	return cols, rows
}

func signalProcessGroup(pgid int, signal syscall.Signal) error {
	if pgid <= 0 {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-pgid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func workspaceProcessExit(err error) (int32, string, string) {
	if err == nil {
		return 0, "", ""
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		status, ok := exitErr.Sys().(syscall.WaitStatus)
		if !ok {
			return int32(exitErr.ExitCode()), "", ""
		}
		if status.Signaled() {
			return int32(exitErr.ExitCode()), status.Signal().String(), ""
		}
		return int32(status.ExitStatus()), "", ""
	}
	return 0, "", workspaceOperationErrorJSON(err)
}

func workspaceOperationErrorJSON(err error) string {
	body, marshalErr := json.Marshal(struct {
		Message string `json:"message"`
	}{Message: err.Error()})
	if marshalErr != nil {
		return `{"message":"workspace operation failed"}`
	}
	return string(body)
}

func (entry *workspaceMaterializationEntry) emitExecError(envelope *workspacev0.WorkspaceOperationEnvelope, execID string, err error) {
	entry.emit(&workspacev0.WorkspaceOperationEvent{Envelope: envelope, Event: &workspacev0.WorkspaceOperationEvent_ExecError{ExecError: &workspacev0.WorkspaceExecError{ExecId: execID, ErrorJson: workspaceOperationErrorJSON(err)}}})
}

func (entry *workspaceMaterializationEntry) emitPtyError(envelope *workspacev0.WorkspaceOperationEnvelope, ptyID string, err error) {
	entry.emit(&workspacev0.WorkspaceOperationEvent{Envelope: envelope, Event: &workspacev0.WorkspaceOperationEvent_PtyError{PtyError: &workspacev0.WorkspacePtyError{PtyId: ptyID, ErrorJson: workspaceOperationErrorJSON(err)}}})
}

func (entry *workspaceMaterializationEntry) emit(event *workspacev0.WorkspaceOperationEvent) {
	if entry.events == nil {
		return
	}
	select {
	case entry.events <- event:
	case <-entry.eventsDone:
	}
}

func (entry *workspaceMaterializationEntry) closeEvents() {
	if entry.eventsDone == nil {
		return
	}
	entry.eventsDoneOnce.Do(func() {
		close(entry.eventsDone)
	})
}
