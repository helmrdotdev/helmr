package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	runv0 "github.com/helmrdotdev/helmr/internal/gen/helmr/run/v0"
	"github.com/helmrdotdev/helmr/internal/guest"
	"github.com/mdlayher/vsock"
	"google.golang.org/protobuf/proto"
)

var resumeAttachTimeout = 30 * time.Second

const (
	defaultRuntimeWorkdir = "/workspace"
	defaultRuntimePath    = "/usr/local/bin:/usr/bin:/bin"
)

type config struct {
	bunPath     string
	adapterPath string
	runtimePath string
	vsockPort   uint
	healthPort  uint
}

type connectionStart struct {
	streamHeader guest.StreamHeader
	bodyLen      uint64
	attach       *runv0.ResumeAttach
}

type waitingRunRegistry struct {
	mu    sync.Mutex
	slots map[string]*waitingRunSlot
}

type waitingRunSlot struct {
	checkpointID string
	attached     chan io.ReadWriter
}

type waitingRunRegistration struct {
	registry    *waitingRunRegistry
	waitpointID string
	slot        *waitingRunSlot
}

func newWaitingRunRegistry() *waitingRunRegistry {
	return &waitingRunRegistry{slots: map[string]*waitingRunSlot{}}
}

func (r *waitingRunRegistry) register(waitpointID string, checkpointID string) waitingRunRegistration {
	slot := &waitingRunSlot{
		checkpointID: checkpointID,
		attached:     make(chan io.ReadWriter, 1),
	}
	r.mu.Lock()
	r.slots[waitpointID] = slot
	r.mu.Unlock()
	return waitingRunRegistration{registry: r, waitpointID: waitpointID, slot: slot}
}

func (r *waitingRunRegistry) attach(waitpointID string, checkpointID string, stream io.ReadWriter) error {
	r.mu.Lock()
	slot := r.slots[waitpointID]
	r.mu.Unlock()
	if slot == nil {
		return fmt.Errorf("no waiting run slot matched waitpoint %s checkpoint %s", waitpointID, checkpointID)
	}
	if slot.checkpointID != checkpointID {
		return fmt.Errorf("resume attach checkpoint %s did not match expected %s", checkpointID, slot.checkpointID)
	}
	select {
	case slot.attached <- stream:
		return nil
	default:
		return fmt.Errorf("waitpoint %s already has an attached resume stream", waitpointID)
	}
}

func (r waitingRunRegistration) wait(ctx context.Context) (io.ReadWriter, error) {
	select {
	case stream := <-r.slot.attached:
		return stream, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r waitingRunRegistration) unregister() {
	r.registry.mu.Lock()
	if r.registry.slots[r.waitpointID] == r.slot {
		delete(r.registry.slots, r.waitpointID)
	}
	r.registry.mu.Unlock()
}

func main() {
	cfg := parseFlags()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(context.Background(), cfg, logger); err != nil {
		logger.Error("guestd failed", "error", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.bunPath, "bun-path", "/usr/bin/bun", "Bun executable path")
	flag.StringVar(&cfg.adapterPath, "adapter-path", "/opt/helmr/adapter/main.js", "adapter entrypoint path")
	flag.StringVar(&cfg.runtimePath, "runtime-path", "/opt/helmr-runtime", "runtime bundle path")
	flag.UintVar(&cfg.vsockPort, "vsock-port", 5000, "guest task vsock port")
	flag.UintVar(&cfg.healthPort, "health-port", 5001, "health check vsock port")
	flag.Parse()
	return cfg
}

func run(ctx context.Context, cfg config, logger *slog.Logger) error {
	if strings.TrimSpace(cfg.bunPath) == "" {
		return errors.New("bun path is required")
	}
	if strings.TrimSpace(cfg.adapterPath) == "" {
		return errors.New("adapter path is required")
	}
	healthListener, err := vsock.Listen(uint32(cfg.healthPort), nil)
	if err != nil {
		return fmt.Errorf("listen health vsock: %w", err)
	}
	defer healthListener.Close()
	var ready atomic.Bool
	go serveHealth(healthListener, ready.Load)

	runListener, err := vsock.Listen(uint32(cfg.vsockPort), nil)
	if err != nil {
		return fmt.Errorf("listen guest task vsock: %w", err)
	}
	defer runListener.Close()
	ready.Store(true)
	logger.Info("guestd ready", "vsock_port", cfg.vsockPort, "health_port", cfg.healthPort)

	registry := newWaitingRunRegistry()
	for {
		conn, err := runListener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("accept guest task connection: %w", err)
		}
		go func() {
			closeConn := true
			defer func() {
				if closeConn {
					_ = conn.Close()
				}
			}()
			keepOpen, err := handleConnection(ctx, conn, cfg, logger, registry)
			if keepOpen {
				closeConn = false
			}
			if err != nil {
				logger.Error("run failed", "error", err)
			}
		}()
	}
}

func serveHealth(listener net.Listener, ready func() bool) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !ready() {
			_, _ = io.WriteString(w, `{"status":"starting","component":"guestd"}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok","component":"guestd"}`)
	})
	_ = http.Serve(listener, mux)
}

func handleConnection(ctx context.Context, conn io.ReadWriter, cfg config, logger *slog.Logger, registry *waitingRunRegistry) (bool, error) {
	start, err := readConnectionStart(conn)
	if err != nil {
		return false, err
	}
	if start.attach != nil {
		if err := registry.attach(start.attach.WaitpointId, start.attach.CheckpointId, conn); err != nil {
			return false, err
		}
		return true, nil
	}
	switch start.streamHeader.Type {
	case guest.StreamTypeParseSource:
		return false, handleParseSource(ctx, conn, cfg, start.streamHeader, start.bodyLen)
	case guest.StreamTypeRunImage:
		return false, handleRunConnection(ctx, conn, cfg, logger, registry, start.streamHeader, start.bodyLen)
	default:
		return false, fmt.Errorf("unsupported input stream type %q", start.streamHeader.Type)
	}
}

func readConnectionStart(conn io.Reader) (connectionStart, error) {
	var prefix [8]byte
	if _, err := io.ReadFull(conn, prefix[:]); err != nil {
		return connectionStart{}, fmt.Errorf("read initial connection frame: %w", err)
	}
	frameLen := binary.BigEndian.Uint32(prefix[:4])
	if frameLen < 4 {
		return connectionStart{}, fmt.Errorf("initial connection frame length %d is invalid", frameLen)
	}
	second := binary.BigEndian.Uint32(prefix[4:])
	if second <= frameLen && second <= guest.MaxFrameBytes {
		headerBytes := make([]byte, second)
		if _, err := io.ReadFull(conn, headerBytes); err != nil {
			return connectionStart{}, fmt.Errorf("read stream header: %w", err)
		}
		var header guest.StreamHeader
		if err := json.Unmarshal(headerBytes, &header); err == nil && strings.TrimSpace(string(header.Type)) != "" {
			return connectionStart{streamHeader: header, bodyLen: uint64(frameLen) - uint64(second)}, nil
		}
		if second > frameLen-4 {
			return connectionStart{}, errors.New("decode initial stream header")
		}
		remaining := int(frameLen) - 4 - len(headerBytes)
		body := append([]byte{}, prefix[4:]...)
		body = append(body, headerBytes...)
		if remaining > 0 {
			tail := make([]byte, remaining)
			if _, err := io.ReadFull(conn, tail); err != nil {
				return connectionStart{}, fmt.Errorf("read resume attach frame: %w", err)
			}
			body = append(body, tail...)
		}
		var attach runv0.ResumeAttach
		if err := proto.Unmarshal(body, &attach); err != nil {
			return connectionStart{}, fmt.Errorf("decode initial connection frame: %w", err)
		}
		return validateResumeAttach(&attach)
	}
	if frameLen > guest.MaxFrameBytes {
		return connectionStart{}, fmt.Errorf("resume attach frame length %d exceeds max %d", frameLen, guest.MaxFrameBytes)
	}
	body := make([]byte, int(frameLen))
	copy(body, prefix[4:])
	if _, err := io.ReadFull(conn, body[4:]); err != nil {
		return connectionStart{}, fmt.Errorf("read resume attach frame: %w", err)
	}
	var attach runv0.ResumeAttach
	if err := proto.Unmarshal(body, &attach); err != nil {
		return connectionStart{}, fmt.Errorf("decode resume attach: %w", err)
	}
	return validateResumeAttach(&attach)
}

func validateResumeAttach(attach *runv0.ResumeAttach) (connectionStart, error) {
	if strings.TrimSpace(attach.CheckpointId) == "" || strings.TrimSpace(attach.WaitpointId) == "" || strings.TrimSpace(attach.SessionId) == "" {
		return connectionStart{}, errors.New("resume attach is missing required fields")
	}
	return connectionStart{attach: attach}, nil
}

func handleParseSource(ctx context.Context, conn io.ReadWriter, cfg config, header guest.StreamHeader, bodyLen uint64) error {
	runRoot, err := os.MkdirTemp("", "helmr-run-*")
	if err != nil {
		return fmt.Errorf("create parse temp dir: %w", err)
	}
	defer os.RemoveAll(runRoot)
	sourceRoot := filepath.Join(runRoot, "source")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		return fmt.Errorf("create parse source dir: %w", err)
	}
	runID := strings.TrimSpace(header.RunID)
	if runID == "" {
		return errors.New("parse source run_id is required")
	}
	taskID := strings.TrimSpace(header.TaskID)
	if taskID == "" {
		return errors.New("parse source task_id is required")
	}
	body := &io.LimitedReader{R: conn, N: int64(bodyLen)}
	if err := extractTar(body, sourceRoot); err != nil {
		if _, drainErr := io.Copy(io.Discard, body); drainErr != nil {
			return errors.Join(fmt.Errorf("extract parse source: %w", err), fmt.Errorf("drain parse source: %w", drainErr))
		}
		return guest.WriteParseErrorFrame(conn, "bad_request", fmt.Sprintf("extract parse source: %s", err))
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		return guest.WriteParseErrorFrame(conn, "bad_request", fmt.Sprintf("drain parse source: %s", err))
	}
	bundle, err := parseAdapter(ctx, cfg, sourceRoot, taskID)
	if err != nil {
		var parseErr adapterParseError
		if errors.As(err, &parseErr) {
			return guest.WriteParseErrorFrame(conn, parseErr.Kind, parseErr.Message)
		}
		return err
	}
	return guest.WriteMessageFrame(conn, bundle)
}

func handleRunConnection(ctx context.Context, conn io.ReadWriter, cfg config, logger *slog.Logger, registry *waitingRunRegistry, header guest.StreamHeader, bodyLen uint64) error {
	if err := handleRunStream(ctx, conn, cfg, logger, registry, header, bodyLen); err != nil {
		if reportErr := writeRunSetupFailure(conn, err); reportErr != nil {
			return errors.Join(err, fmt.Errorf("write run setup failure: %w", reportErr))
		}
	}
	return nil
}

func handleRunStream(ctx context.Context, conn io.ReadWriter, cfg config, logger *slog.Logger, registry *waitingRunRegistry, header guest.StreamHeader, bodyLen uint64) error {
	runRoot, err := os.MkdirTemp("", "helmr-run-*")
	if err != nil {
		return fmt.Errorf("create run temp dir: %w", err)
	}
	defer os.RemoveAll(runRoot)
	taskSourceRoot := filepath.Join(runRoot, "task-source")
	if err := os.MkdirAll(taskSourceRoot, 0o755); err != nil {
		return fmt.Errorf("create task source dir: %w", err)
	}
	workspaceSourceRoot := filepath.Join(runRoot, "workspace-source")
	if err := os.MkdirAll(workspaceSourceRoot, 0o755); err != nil {
		return fmt.Errorf("create workspace source dir: %w", err)
	}
	imageRoot := filepath.Join(runRoot, "image")
	var image ociImage
	runID := header.RunID
	if strings.TrimSpace(runID) == "" {
		return errors.New("input stream run_id is required")
	}
	if header.Type != guest.StreamTypeRunImage {
		return fmt.Errorf("unsupported input stream type %q", header.Type)
	}
	body := &io.LimitedReader{R: conn, N: int64(bodyLen)}
	image, err = unpackOCIImage(body, imageRoot)
	if err != nil {
		if _, drainErr := io.Copy(io.Discard, body); drainErr != nil {
			return errors.Join(fmt.Errorf("unpack run image: %w", err), fmt.Errorf("drain run image: %w", drainErr))
		}
		return fmt.Errorf("unpack run image: %w", err)
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		return fmt.Errorf("drain run image: %w", err)
	}
	header, bodyLen, err = guest.ReadStreamFrameHeader(conn)
	if err != nil {
		return fmt.Errorf("read task source stream header: %w", err)
	}
	if header.RunID != runID {
		return fmt.Errorf("task source run_id %q does not match run image run_id %q", header.RunID, runID)
	}
	if header.Type != guest.StreamTypeTaskSource {
		return fmt.Errorf("unsupported input stream type %q", header.Type)
	}
	body = &io.LimitedReader{R: conn, N: int64(bodyLen)}
	if err := extractTar(body, taskSourceRoot); err != nil {
		if _, drainErr := io.Copy(io.Discard, body); drainErr != nil {
			return errors.Join(fmt.Errorf("extract task source: %w", err), fmt.Errorf("drain task source: %w", drainErr))
		}
		drainRemainingRunInput(conn, runID, true)
		return fmt.Errorf("extract task source: %w", err)
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		return fmt.Errorf("drain task source: %w", err)
	}
	header, bodyLen, err = guest.ReadStreamFrameHeader(conn)
	if err != nil {
		return fmt.Errorf("read workspace source stream header: %w", err)
	}
	if header.RunID != runID {
		return fmt.Errorf("workspace source run_id %q does not match run image run_id %q", header.RunID, runID)
	}
	if header.Type != guest.StreamTypeWorkspaceSource {
		return fmt.Errorf("unsupported input stream type %q", header.Type)
	}
	body = &io.LimitedReader{R: conn, N: int64(bodyLen)}
	if err := extractTar(body, workspaceSourceRoot); err != nil {
		if _, drainErr := io.Copy(io.Discard, body); drainErr != nil {
			return errors.Join(fmt.Errorf("extract workspace source: %w", err), fmt.Errorf("drain workspace source: %w", drainErr))
		}
		drainRunRequest(conn)
		return fmt.Errorf("extract workspace source: %w", err)
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		return fmt.Errorf("drain workspace source: %w", err)
	}
	var request runv0.RunTaskRequest
	if err := guest.ReadProtoFrame(conn, &request); err != nil {
		return fmt.Errorf("read run request: %w", err)
	}
	if request.RunId != runID {
		return fmt.Errorf("run request run_id %q does not match input stream run_id %q", request.RunId, runID)
	}
	mountPath, err := workspaceMountPath(&request)
	if err != nil {
		return err
	}
	workspaceRoot, err := workspaceRootForImage(image.RootfsDir, mountPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		return fmt.Errorf("create workspace root: %w", err)
	}
	if err := copyTree(workspaceSourceRoot, workspaceRoot); err != nil {
		return fmt.Errorf("materialize workspace: %w", err)
	}
	runCwd := request.Cwd
	if strings.TrimSpace(runCwd) == "" {
		runCwd = mountPath
	}
	logger.Info("running task", "run_id", request.RunId, "task_id", request.TaskId)
	return runAdapter(ctx, conn, cfg, image.RootfsDir, taskSourceRoot, workspaceRoot, runCwd, image.Config, true, &request, registry)
}

func drainRunRequest(conn io.Reader) {
	_, _ = guest.ReadMessageFrame(conn)
}

func drainRemainingRunInput(conn io.Reader, runID string, includeWorkspaceSource bool) {
	if includeWorkspaceSource {
		header, bodyLen, err := guest.ReadStreamFrameHeader(conn)
		if err != nil {
			return
		}
		if header.RunID != runID || header.Type != guest.StreamTypeWorkspaceSource {
			return
		}
		if _, err := io.Copy(io.Discard, &io.LimitedReader{R: conn, N: int64(bodyLen)}); err != nil {
			return
		}
	}
	drainRunRequest(conn)
}

func parseAdapter(ctx context.Context, cfg config, sourceRoot string, taskID string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, cfg.bunPath, cfg.adapterPath,
		"parse",
		"--cwd", sourceRoot,
		"--task", taskID,
		"--output", "binary",
	)
	cmd.Dir = sourceRoot
	cmd.Env = mergeEnv(os.Environ(), nil, []string{"HELMR_ADAPTER_SDK_PATH=/opt/helmr/adapter/sdk.js"})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, classifyAdapterParseError(taskID, stderr.String(), err)
	}
	return stdout.Bytes(), nil
}

type adapterParseError struct {
	Kind    string
	Message string
}

func (e adapterParseError) Error() string {
	if e.Message == "" {
		return "parse task bundle"
	}
	return "parse task bundle: " + e.Message
}

type adapterErrorPayload struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func classifyAdapterParseError(taskID string, stderr string, fallback error) adapterParseError {
	message := strings.TrimSpace(stderr)
	if message == "" && fallback != nil {
		message = fallback.Error()
	}
	for _, line := range reverseLines(stderr) {
		var payload adapterErrorPayload
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			continue
		}
		kind := strings.TrimSpace(payload.Kind)
		if kind == "" {
			continue
		}
		payloadMessage := strings.TrimSpace(payload.Message)
		if payloadMessage == "" {
			payloadMessage = defaultAdapterParseMessage(kind, taskID, message)
		}
		return adapterParseError{Kind: kind, Message: payloadMessage}
	}
	return adapterParseError{Kind: "bad_request", Message: message}
}

func reverseLines(value string) []string {
	lines := strings.Split(strings.TrimSpace(value), "\n")
	for left, right := 0, len(lines)-1; left < right; left, right = left+1, right-1 {
		lines[left], lines[right] = lines[right], lines[left]
	}
	return lines
}

func defaultAdapterParseMessage(kind string, taskID string, fallback string) string {
	switch kind {
	case "task_not_found":
		if strings.TrimSpace(taskID) != "" {
			return "task not found: " + taskID
		}
	case "duplicate_task_id":
		if strings.TrimSpace(taskID) != "" {
			return "duplicate task id: " + taskID
		}
	case "missing_config":
		return "no helmr.config.ts found"
	}
	if strings.TrimSpace(fallback) != "" {
		return fallback
	}
	return kind
}

func extractTar(r io.Reader, dst string) error {
	reader := tar.NewReader(r)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if tarEntryIsRootDir(header) {
			continue
		}
		relative, err := tarEntryPath(header.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(relative))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := mkdirAllNoSymlink(dst, relative, os.FileMode(header.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg:
			parent := filepath.ToSlash(filepath.Dir(relative))
			if parent == "." {
				parent = ""
			}
			if err := mkdirAllNoSymlink(dst, parent, 0o755); err != nil {
				return err
			}
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, reader)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			if err := validateSymlinkTarget(relative, header.Linkname); err != nil {
				return err
			}
			parent := filepath.ToSlash(filepath.Dir(relative))
			if parent == "." {
				parent = ""
			}
			if err := mkdirAllNoSymlink(dst, parent, 0o755); err != nil {
				return err
			}
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			return fmt.Errorf("unsupported tar hardlink entry %q", header.Name)
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return fmt.Errorf("unsupported tar device entry %q type %d", header.Name, header.Typeflag)
		default:
			return fmt.Errorf("unsupported tar entry %q type %d", header.Name, header.Typeflag)
		}
	}
}

func tarEntryIsRootDir(header *tar.Header) bool {
	if header == nil || header.Typeflag != tar.TypeDir {
		return false
	}
	name := strings.TrimSpace(header.Name)
	if name == "" || filepath.IsAbs(name) || strings.HasPrefix(filepath.FromSlash(name), string(filepath.Separator)) {
		return false
	}
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(name))) == "."
}

func tarEntryPath(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("tar path is empty")
	}
	if filepath.IsAbs(name) || strings.HasPrefix(filepath.FromSlash(name), string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe tar path %q", name)
	}
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if part == ".." {
			return "", fmt.Errorf("unsafe tar path %q", name)
		}
	}
	relative := filepath.ToSlash(filepath.Clean(filepath.FromSlash(name)))
	if relative == "." || relative == ".." || strings.HasPrefix(relative, "../") {
		return "", fmt.Errorf("unsafe tar path %q", name)
	}
	return relative, nil
}

func mkdirAllNoSymlink(root, relative string, mode os.FileMode) error {
	if relative == "" || relative == "." {
		return nil
	}
	clean, err := tarEntryPath(relative)
	if err != nil {
		return err
	}
	current := root
	for _, part := range strings.Split(clean, "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, mode); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("unsafe tar parent %q", current)
		}
	}
	return nil
}

func safeJoin(root, name string) (string, error) {
	clean := filepath.Clean("/" + filepath.FromSlash(name))
	if clean == "/" {
		return root, nil
	}
	target := filepath.Join(root, strings.TrimPrefix(clean, string(filepath.Separator)))
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("tar path escapes destination: %s", name)
	}
	return target, nil
}

func workspaceMountPath(request *runv0.RunTaskRequest) (string, error) {
	mountPath := "/workspace"
	if request.WorkspaceOverlay != nil && strings.TrimSpace(request.WorkspaceOverlay.MountPath) != "" {
		mountPath = request.WorkspaceOverlay.MountPath
	}
	if !strings.HasPrefix(mountPath, "/") {
		return "", fmt.Errorf("workspace mount path must be absolute: %q", mountPath)
	}
	for _, part := range strings.Split(mountPath, "/") {
		if part == ".." {
			return "", fmt.Errorf("workspace mount path must not contain parent components: %q", mountPath)
		}
	}
	clean := pathpkg.Clean(mountPath)
	if clean == "/" {
		return "", errors.New("workspace mount path must not be root")
	}
	if isReservedRuntimePath(clean) {
		return "", fmt.Errorf("workspace mount path %q conflicts with reserved runtime paths", clean)
	}
	return clean, nil
}

func workspaceRootForImage(imageRoot, mountPath string) (string, error) {
	root, err := confinedLayerPath(imageRoot, strings.TrimPrefix(mountPath, "/"))
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return root, nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("workspace mount path is not a directory: %s", mountPath)
	}
	return root, nil
}

func runAdapter(ctx context.Context, conn io.ReadWriter, cfg config, imageRoot string, taskSourceRoot string, workspaceRoot string, runCwd string, imageConfig ociRuntimeConfig, imageMode bool, request *runv0.RunTaskRequest, registry *waitingRunRegistry) error {
	stdoutWriter := eventWriter{conn: conn}
	bunPath := cfg.bunPath
	var bunPrefixArgs []string
	adapterPath := cfg.adapterPath
	taskAdapterCwd := taskSourceRoot
	if imageMode {
		if err := installRuntimeBundle(cfg.runtimePath, imageRoot); err != nil {
			return writeRunSetupFailure(conn, err)
		}
		adapterPath = "/opt/helmr/adapter/main.js"
		var err error
		bunPath, bunPrefixArgs, err = bundledRuntimeCommand(imageRoot)
		if err != nil {
			return writeRunSetupFailure(conn, err)
		}
	}
	launchCwd := runCwd
	var runtimeUser *resolvedRuntimeUser
	if imageMode {
		var err error
		runtimeUser, err = resolveRuntimeUser(imageRoot, imageConfig.User)
		if err != nil {
			return writeRunSetupFailure(conn, err)
		}
		launchCwd, err = resolveLaunchCwd(runCwd, defaultRuntimeWorkdir)
		if err != nil {
			return writeRunSetupFailure(conn, err)
		}
		taskAdapterCwd, err = materializeTaskSourceForRuntime(imageRoot, taskSourceRoot, launchCwd, runtimeUser)
		if err != nil {
			return writeRunSetupFailure(conn, err)
		}
		if err := prepareLaunchPath(imageRoot, launchCwd, runtimeUser); err != nil {
			return writeRunSetupFailure(conn, err)
		}
		if err := chownTree(workspaceRoot, runtimeUser.UID, runtimeUser.GID); err != nil {
			return writeRunSetupFailure(conn, fmt.Errorf("prepare workspace owner: %w", err))
		}
	}
	var cmdEnv []string
	if imageMode && runtimeUser != nil {
		cmdEnv = imageRuntimeEnv(imageConfig, runtimeUser, launchCwd)
	} else {
		cmdEnv = mergeEnv(os.Environ(), imageConfig.Env, []string{"HELMR_ADAPTER_SDK_PATH=/opt/helmr/adapter/sdk.js"})
	}
	cmdArgs := append(append([]string{}, bunPrefixArgs...), adapterPath,
		"run",
		"--cwd", runCwd,
		"--task-cwd", taskAdapterCwd,
		"--task", request.TaskId,
		"--run-id", request.RunId,
		"--payload-json", request.PayloadJson,
	)
	cmd, err := adapterCommand(ctx, bunPath, cmdArgs, launchCwd, cmdEnv, imageRoot, runtimeUser, imageMode)
	if err != nil {
		return writeRunSetupFailure(conn, err)
	}
	if imageMode {
		cleanupRuntimeMounts, err := mountImageRuntimeFilesystems(imageRoot)
		if err != nil {
			return writeRunSetupFailure(conn, err)
		}
		defer cleanupRuntimeMounts()
	}
	if err := applySecrets(imageRoot, workspaceRoot, request, runtimeUser, &cmd.Env); err != nil {
		return writeRunSetupFailure(conn, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return writeRunSetupFailure(conn, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return writeRunSetupFailure(conn, err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return writeRunSetupFailure(conn, err)
	}
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		return writeRunSetupFailure(conn, err)
	}
	defer controlReader.Close()
	cmd.ExtraFiles = []*os.File{controlWriter}
	if err := cmd.Start(); err != nil {
		_ = controlWriter.Close()
		_ = stdin.Close()
		return writeRunSetupFailure(conn, err)
	}
	_ = controlWriter.Close()

	var wg sync.WaitGroup
	controlErrCh := make(chan error, 1)
	recordControlErr := func(err error) {
		if err == nil {
			return
		}
		select {
		case controlErrCh <- err:
		default:
		}
	}
	wg.Add(3)
	go func() {
		defer wg.Done()
		_ = forwardChunks(stdout, func(chunk []byte) *runv0.RunEvent {
			return &runv0.RunEvent{Event: &runv0.RunEvent_StdoutChunk{StdoutChunk: chunk}}
		}, &stdoutWriter)
	}()
	go func() {
		defer wg.Done()
		_ = forwardChunks(stderr, func(chunk []byte) *runv0.RunEvent {
			return &runv0.RunEvent{Event: &runv0.RunEvent_StderrChunk{StderrChunk: chunk}}
		}, &stdoutWriter)
	}()
	go func() {
		defer wg.Done()
		defer stdin.Close()
		for {
			event, err := guest.ReadRunEvent(controlReader)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					recordControlErr(fmt.Errorf("read adapter control event: %w", err))
				}
				return
			}
			if wait := event.GetWaitRequested(); wait != nil {
				if err := stdoutWriter.write(event); err != nil {
					recordControlErr(fmt.Errorf("write wait request event: %w", err))
					return
				}
				var suspend runv0.SuspendForCheckpoint
				if err := guest.ReadProtoFrame(conn, &suspend); err != nil {
					recordControlErr(fmt.Errorf("read checkpoint suspend request: %w", err))
					return
				}
				registration := registry.register(suspend.WaitpointId, suspend.CheckpointId)
				if err := stdoutWriter.writeProto(&runv0.PauseReady{
					WaitpointId:  suspend.WaitpointId,
					CheckpointId: suspend.CheckpointId,
				}); err != nil {
					registration.unregister()
					recordControlErr(fmt.Errorf("write checkpoint pause ready: %w", err))
					return
				}
				attachCtx, cancelAttach := context.WithTimeout(ctx, resumeAttachTimeout)
				attached, err := registration.wait(attachCtx)
				cancelAttach()
				registration.unregister()
				if err != nil {
					recordControlErr(fmt.Errorf("wait for resume attach: %w", err))
					return
				}
				decisionCtx, cancelDecision := context.WithTimeout(ctx, resumeAttachTimeout)
				decision, err := readResumeDecision(decisionCtx, attached)
				cancelDecision()
				if err != nil {
					recordControlErr(fmt.Errorf("read resume decision: %w", err))
					return
				}
				if err := stdoutWriter.resumeOn(attached, stdin, decision); err != nil {
					recordControlErr(fmt.Errorf("resume adapter stream: %w", err))
					return
				}
				continue
			}
			if err := stdoutWriter.write(event); err != nil {
				recordControlErr(fmt.Errorf("write adapter control event: %w", err))
				return
			}
		}
	}()

	waitErr := cmd.Wait()
	_ = stdin.Close()
	wg.Wait()
	var controlErr error
	select {
	case controlErr = <-controlErrCh:
	default:
	}
	exitCode := int32(0)
	var message string
	if controlErr != nil {
		exitCode = 1
		message = controlErr.Error()
	} else if waitErr != nil {
		exitCode = 1
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = int32(exitErr.ExitCode())
		}
	}
	return stdoutWriter.writeComplete(exitCode, message)
}

func writeRunSetupFailure(conn io.Writer, err error) error {
	message := "guest runtime setup failed"
	if err != nil && strings.TrimSpace(err.Error()) != "" {
		message = err.Error()
	}
	writer := eventWriter{conn: conn}
	if writeErr := writer.write(&runv0.RunEvent{Event: &runv0.RunEvent_StderrChunk{StderrChunk: []byte(message + "\n")}}); writeErr != nil {
		return writeErr
	}
	return writer.writeComplete(1, message)
}

func installRuntimeBundle(runtimePath, imageRoot string) error {
	if strings.TrimSpace(runtimePath) == "" {
		return errors.New("runtime path is required")
	}
	if err := mkdirAllNoSymlink(imageRoot, "opt", 0o755); err != nil {
		return err
	}
	target, err := safeJoin(imageRoot, "opt/helmr")
	if err != nil {
		return err
	}
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		return err
	}
	return copyTree(runtimePath, target)
}

func materializeTaskSourceForRuntime(imageRoot string, sourceRoot string, launchCwd string, runtimeUser *resolvedRuntimeUser) (string, error) {
	runtimePath := pathpkg.Join(launchCwd, ".helmr", "task-source")
	if isReservedRuntimePath(runtimePath) {
		return "", fmt.Errorf("task source path %s conflicts with reserved runtime paths", runtimePath)
	}
	parent := pathpkg.Join(strings.TrimPrefix(runtimePath, "/"), "..")
	if err := mkdirAllNoSymlink(imageRoot, parent, 0o755); err != nil {
		return "", err
	}
	target, err := safeJoin(imageRoot, strings.TrimPrefix(runtimePath, "/"))
	if err != nil {
		return "", err
	}
	if err := os.RemoveAll(target); err != nil {
		return "", err
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		return "", err
	}
	if err := copyTree(sourceRoot, target); err != nil {
		return "", fmt.Errorf("materialize task source: %w", err)
	}
	if runtimeUser != nil {
		if err := chownTree(target, runtimeUser.UID, runtimeUser.GID); err != nil {
			return "", fmt.Errorf("prepare task source owner: %w", err)
		}
	}
	return runtimePath, nil
}

func bundledRuntimeCommand(imageRoot string) (string, []string, error) {
	bunHostPath, err := safeJoin(imageRoot, "opt/helmr/bin/bun")
	if err != nil {
		return "", nil, err
	}
	if !isExecutableFile(bunHostPath) {
		return "", nil, errors.New("runtime bundle must provide executable /opt/helmr/bin/bun")
	}
	libHostPath, err := safeJoin(imageRoot, "opt/helmr/lib")
	if err != nil {
		return "", nil, err
	}
	loaderName, err := findBundledRuntimeLoader(libHostPath)
	if err != nil {
		return "", nil, err
	}
	loaderPath := pathpkg.Join("/opt/helmr/lib", loaderName)
	return loaderPath, []string{"--library-path", "/opt/helmr/lib", "/opt/helmr/bin/bun"}, nil
}

func findBundledRuntimeLoader(libHostPath string) (string, error) {
	for _, name := range []string{"ld-linux-x86-64.so.2", "ld-linux-aarch64.so.1"} {
		if isExecutableFile(filepath.Join(libHostPath, name)) {
			return name, nil
		}
	}
	entries, err := os.ReadDir(libHostPath)
	if err != nil {
		return "", fmt.Errorf("read runtime bundle lib directory: %w", err)
	}
	var muslLoaders []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "ld-musl-") && strings.HasSuffix(name, ".so.1") {
			muslLoaders = append(muslLoaders, name)
		}
	}
	sort.Strings(muslLoaders)
	for _, name := range muslLoaders {
		if isExecutableFile(filepath.Join(libHostPath, name)) {
			return name, nil
		}
	}
	return "", errors.New("runtime bundle must provide an executable dynamic loader in /opt/helmr/lib")
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

type eventWriter struct {
	mu   sync.Mutex
	conn io.Writer
}

func (w *eventWriter) write(event *runv0.RunEvent) error {
	return w.writeProto(event)
}

func (w *eventWriter) writeProto(message proto.Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return guest.WriteProtoFrame(w.conn, message)
}

func (w *eventWriter) resumeOn(conn io.Writer, stdin io.Writer, decision *runv0.ResumeDecision) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.conn = conn
	if err := guest.WriteProtoFrame(stdin, decision); err != nil {
		return err
	}
	return guest.WriteProtoFrame(w.conn, &runv0.ResumeAck{WaitpointId: decision.WaitpointId})
}

func (w *eventWriter) writeComplete(exitCode int32, message string) error {
	complete := &runv0.TaskComplete{ExitCode: exitCode}
	if message != "" {
		complete.ErrorMessage = &message
	}
	return w.write(&runv0.RunEvent{Event: &runv0.RunEvent_TaskComplete{TaskComplete: complete}})
}

func forwardChunks(r io.Reader, event func([]byte) *runv0.RunEvent, writer *eventWriter) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if writeErr := writer.write(event(chunk)); writeErr != nil {
				return writeErr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

type resumeDecisionResult struct {
	decision *runv0.ResumeDecision
	err      error
}

func readResumeDecision(ctx context.Context, reader io.Reader) (*runv0.ResumeDecision, error) {
	result := make(chan resumeDecisionResult, 1)
	go func() {
		var decision runv0.ResumeDecision
		err := guest.ReadProtoFrame(reader, &decision)
		result <- resumeDecisionResult{decision: &decision, err: err}
	}()
	select {
	case value := <-result:
		return value.decision, value.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func mergeEnv(groups ...[]string) []string {
	values := make(map[string]string)
	order := []string{}
	for _, group := range groups {
		for _, entry := range group {
			key, value, ok := strings.Cut(entry, "=")
			if !ok {
				continue
			}
			if _, exists := values[key]; !exists {
				order = append(order, key)
			}
			values[key] = value
		}
	}
	env := make([]string, 0, len(order))
	for _, key := range order {
		env = append(env, key+"="+values[key])
	}
	return env
}

func imageRuntimeEnv(imageConfig ociRuntimeConfig, runtimeUser *resolvedRuntimeUser, launchCwd string) []string {
	env := mergeEnv(sanitizeDynamicLoaderEnv(imageConfig.Env), []string{"HELMR_ADAPTER_SDK_PATH=/opt/helmr/adapter/sdk.js"})
	env = setEnvDefault(env, "PATH", defaultRuntimePath)
	env = setEnvDefault(env, "HOME", runtimeUser.Home)
	env = setEnvDefault(env, "USER", runtimeUser.Name)
	env = setEnvDefault(env, "LOGNAME", runtimeUser.Name)
	env = setEnvValue(env, "PWD", launchCwd)
	return env
}

func sanitizeDynamicLoaderEnv(env []string) []string {
	sanitized := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || isDynamicLoaderEnvKey(key) {
			continue
		}
		sanitized = append(sanitized, entry)
	}
	return sanitized
}

func isDynamicLoaderEnvKey(key string) bool {
	return strings.HasPrefix(key, "LD_")
}

func setEnvDefault(env []string, key string, value string) []string {
	if envHasKey(env, key) {
		return env
	}
	return append(env, key+"="+value)
}

func setEnvValue(env []string, key string, value string) []string {
	for i, entry := range env {
		entryKey, _, ok := strings.Cut(entry, "=")
		if ok && entryKey == key {
			env[i] = key + "=" + value
			return env
		}
	}
	return append(env, key+"="+value)
}

func envHasKey(env []string, key string) bool {
	for _, entry := range env {
		entryKey, _, ok := strings.Cut(entry, "=")
		if ok && entryKey == key {
			return true
		}
	}
	return false
}

func applySecrets(imageRoot, workspaceRoot string, request *runv0.RunTaskRequest, runtimeUser *resolvedRuntimeUser, env *[]string) error {
	for _, secret := range request.Secrets {
		if secret == nil {
			return errors.New("secret injection is required")
		}
		if secret.Placement == nil {
			return fmt.Errorf("secret %s placement is required", secret.Name)
		}
		switch placement := secret.Placement.Kind.(type) {
		case *runv0.Placement_Env:
			if placement.Env == nil || strings.TrimSpace(placement.Env.Name) == "" {
				return fmt.Errorf("secret %s env placement name is required", secret.Name)
			}
			envName := strings.TrimSpace(placement.Env.Name)
			if isDynamicLoaderEnvKey(envName) {
				return fmt.Errorf("secret %s env placement %q conflicts with reserved runtime environment", secret.Name, envName)
			}
			*env = setEnvValue(*env, envName, string(secret.ValueBytes))
		case *runv0.Placement_File:
			if placement.File == nil || strings.TrimSpace(placement.File.Path) == "" {
				return fmt.Errorf("secret %s file placement path is required", secret.Name)
			}
			path, err := materializedPath(imageRoot, workspaceRoot, placement.File.Path)
			if err != nil {
				return err
			}
			uid, gid, err := secretOwner(imageRoot, runtimeUser, placement.File.Owner)
			if err != nil {
				return err
			}
			ownSecret := shouldChownSecret(runtimeUser, placement.File.Owner)
			parentUID, parentGID := uid, gid
			if runtimeUser != nil {
				parentUID, parentGID = runtimeUser.UID, runtimeUser.GID
			}
			if err := mkdirAllOwned(filepath.Dir(path), 0o700, parentUID, parentGID, ownSecret); err != nil {
				return err
			}
			mode := os.FileMode(0o600)
			if placement.File.Mode != nil {
				parsed, err := parseSecretMode(*placement.File.Mode)
				if err != nil {
					return fmt.Errorf("invalid secret file mode for %s: %w", placement.File.Path, err)
				}
				mode = parsed
			}
			if err := writeFileNoFollow(path, secret.ValueBytes, mode); err != nil {
				return err
			}
			if err := os.Chmod(path, mode); err != nil {
				return err
			}
			if ownSecret {
				if err := os.Chown(path, int(uid), int(gid)); err != nil {
					return fmt.Errorf("chown secret file %s: %w", path, err)
				}
			}
			if err := ensureRuntimeCanReadSecretFile(imageRoot, workspaceRoot, path, runtimeUser); err != nil {
				return err
			}
		case *runv0.Placement_Dir:
			if placement.Dir == nil || strings.TrimSpace(placement.Dir.Path) == "" {
				return fmt.Errorf("secret %s dir placement path is required", secret.Name)
			}
			path, err := materializedPath(imageRoot, workspaceRoot, placement.Dir.Path)
			if err != nil {
				return err
			}
			uid, gid, err := secretOwner(imageRoot, runtimeUser, placement.Dir.Owner)
			if err != nil {
				return err
			}
			mode := os.FileMode(0o700)
			if placement.Dir.Mode != nil {
				parsed, err := parseSecretMode(*placement.Dir.Mode)
				if err != nil {
					return fmt.Errorf("invalid secret dir mode for %s: %w", placement.Dir.Path, err)
				}
				mode = parsed
			}
			ownSecret := shouldChownSecret(runtimeUser, placement.Dir.Owner)
			if err := mkdirAllOwned(path, mode, uid, gid, ownSecret); err != nil {
				return err
			}
			if err := os.Chmod(path, mode); err != nil {
				return err
			}
			if ownSecret {
				if err := os.Chown(path, int(uid), int(gid)); err != nil {
					return fmt.Errorf("chown secret dir %s: %w", path, err)
				}
			}
			if err := ensureRuntimeCanTraverseSecretDir(imageRoot, workspaceRoot, path, runtimeUser); err != nil {
				return err
			}
		default:
			return fmt.Errorf("secret %s placement is required", secret.Name)
		}
	}
	return nil
}

func shouldChownSecret(runtimeUser *resolvedRuntimeUser, owner *string) bool {
	return runtimeUser != nil || (owner != nil && strings.TrimSpace(*owner) != "")
}

func mkdirAllOwned(path string, mode os.FileMode, uid uint32, gid uint32, own bool) error {
	var missing []string
	for current := path; current != "" && current != string(filepath.Separator); current = filepath.Dir(current) {
		_, err := os.Lstat(current)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	if !own {
		return nil
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Chmod(missing[i], mode); err != nil {
			return err
		}
		if err := os.Chown(missing[i], int(uid), int(gid)); err != nil {
			return err
		}
	}
	return nil
}

func secretOwner(imageRoot string, runtimeUser *resolvedRuntimeUser, owner *string) (uint32, uint32, error) {
	raw := ""
	if owner != nil {
		raw = strings.TrimSpace(*owner)
	}
	if raw == "" {
		if runtimeUser == nil {
			return 0, 0, nil
		}
		return runtimeUser.UID, runtimeUser.GID, nil
	}
	identity, err := resolveUserSpec(imageRoot, raw)
	if err != nil {
		if isRootUserSpec(raw) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("resolve secret owner %q: %w", raw, err)
	}
	return identity.UID, identity.GID, nil
}

func materializedPath(imageRoot, workspaceRoot, path string) (string, error) {
	if err := validateSecretPath(path); err != nil {
		return "", err
	}
	if filepath.IsAbs(path) {
		return confinedMaterializedPath(imageRoot, strings.TrimPrefix(path, "/"))
	}
	return confinedMaterializedPath(workspaceRoot, path)
}

func validateSecretPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("secret path is required")
	}
	if path != strings.TrimSpace(path) {
		return fmt.Errorf("secret path must not contain leading or trailing whitespace: %q", path)
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." || clean == string(filepath.Separator) {
		return fmt.Errorf("secret path must target a file or directory: %q", path)
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return fmt.Errorf("secret path must not contain parent components: %q", path)
		}
	}
	if strings.HasPrefix(path, "/") && isReservedRuntimePath(pathpkg.Clean(filepath.ToSlash(path))) {
		return fmt.Errorf("secret path %q conflicts with reserved runtime paths", path)
	}
	return nil
}

func ensureRuntimeCanReadSecretFile(imageRoot, workspaceRoot, path string, runtimeUser *resolvedRuntimeUser) error {
	if runtimeUser == nil {
		return nil
	}
	if err := ensureRuntimeCanTraversePath(imageRoot, workspaceRoot, filepath.Dir(path), runtimeUser); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("secret file is not a regular file: %s", path)
	}
	if !runtimeCanRead(info, runtimeUser) {
		return fmt.Errorf("secret file is not readable by runtime user %s: %s", runtimeUser.Name, path)
	}
	return nil
}

func ensureRuntimeCanTraverseSecretDir(imageRoot, workspaceRoot, path string, runtimeUser *resolvedRuntimeUser) error {
	if runtimeUser == nil {
		return nil
	}
	return ensureRuntimeCanTraversePath(imageRoot, workspaceRoot, path, runtimeUser)
}

func ensureRuntimeCanTraversePath(imageRoot, workspaceRoot, path string, runtimeUser *resolvedRuntimeUser) error {
	root, err := materializedRoot(imageRoot, workspaceRoot, path)
	if err != nil {
		return err
	}
	current := filepath.Clean(path)
	root = filepath.Clean(root)
	for current != root {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("secret ancestor is not a directory: %s", current)
		}
		if !runtimeCanTraverse(info, runtimeUser) {
			return fmt.Errorf("secret path is not traversable by runtime user %s: %s", runtimeUser.Name, current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return nil
}

func materializedRoot(imageRoot, workspaceRoot, path string) (string, error) {
	imageRoot = filepath.Clean(imageRoot)
	workspaceRoot = filepath.Clean(workspaceRoot)
	path = filepath.Clean(path)
	if path == imageRoot || strings.HasPrefix(path, imageRoot+string(filepath.Separator)) {
		if path == workspaceRoot || strings.HasPrefix(path, workspaceRoot+string(filepath.Separator)) {
			return workspaceRoot, nil
		}
		return imageRoot, nil
	}
	return "", fmt.Errorf("secret path is outside materialized roots: %s", path)
}

func runtimeCanRead(info os.FileInfo, runtimeUser *resolvedRuntimeUser) bool {
	mode := info.Mode().Perm()
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return mode&0o004 != 0
	}
	return permissionApplies(mode, stat.Uid, stat.Gid, runtimeUser, 0o400, 0o040, 0o004)
}

func runtimeCanTraverse(info os.FileInfo, runtimeUser *resolvedRuntimeUser) bool {
	mode := info.Mode().Perm()
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return mode&0o001 != 0
	}
	return permissionApplies(mode, stat.Uid, stat.Gid, runtimeUser, 0o100, 0o010, 0o001)
}

func permissionApplies(mode os.FileMode, uid uint32, gid uint32, runtimeUser *resolvedRuntimeUser, ownerBit os.FileMode, groupBit os.FileMode, otherBit os.FileMode) bool {
	if runtimeUser.UID == 0 {
		return true
	}
	if uid == runtimeUser.UID {
		return mode&ownerBit != 0
	}
	if gid == runtimeUser.GID {
		return mode&groupBit != 0
	}
	return mode&otherBit != 0
}

func confinedMaterializedPath(root, relative string) (string, error) {
	path, err := confinedLayerPath(root, relative)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("secret path must not be a symlink: %s", path)
	}
	return path, nil
}

func writeFileNoFollow(path string, body []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(body)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func parseSecretMode(raw string) (os.FileMode, error) {
	original := raw
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "0o")
	raw = strings.TrimPrefix(raw, "0O")
	if raw == "" {
		return 0, fmt.Errorf("invalid file mode %q", original)
	}
	mode, err := strconv.ParseUint(raw, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid file mode %q", original)
	}
	if mode > 0o777 {
		return 0, fmt.Errorf("file mode %q must only contain permission bits", original)
	}
	return os.FileMode(mode), nil
}

func copyTree(sourceRoot, destinationRoot string) error {
	return filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target, err := safeJoin(destinationRoot, filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			parent := filepath.ToSlash(filepath.Dir(rel))
			if parent == "." {
				parent = ""
			}
			if err := mkdirAllNoSymlink(destinationRoot, parent, 0o755); err != nil {
				return err
			}
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			return os.Symlink(link, target)
		case entry.IsDir():
			return mkdirAllNoSymlink(destinationRoot, filepath.ToSlash(rel), info.Mode()&0o777)
		case info.Mode().IsRegular():
			parent := filepath.ToSlash(filepath.Dir(rel))
			if parent == "." {
				parent = ""
			}
			if err := mkdirAllNoSymlink(destinationRoot, parent, 0o755); err != nil {
				return err
			}
			source, err := os.Open(path)
			if err != nil {
				return err
			}
			defer source.Close()
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			destination, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, info.Mode()&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(destination, source)
			closeErr := destination.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		default:
			return nil
		}
	})
}
