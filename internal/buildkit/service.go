package buildkit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	bkclient "github.com/moby/buildkit/client"
)

const (
	buildKitReadyTimeout  = 30 * time.Second
	buildKitHealthTimeout = 3 * time.Second
)

type daemonClient interface {
	buildkitSolver
	Wait(context.Context) error
	Close() error
}

type serviceState struct {
	ActiveState  string
	MainPID      int
	Invocation   string
	ControlGroup string
}

type serviceController interface {
	Inspect(context.Context, string) (serviceState, error)
	Stop(context.Context, string) error
	Start(context.Context, string) error
	CgroupEmpty(string) (bool, error)
	SocketAbsent(string) (bool, error)
	WaitSocket(context.Context, string) error
}

type systemdController struct{}

func (systemdController) Inspect(ctx context.Context, service string) (serviceState, error) {
	output, err := exec.CommandContext(
		ctx,
		"systemctl",
		"show",
		"--no-pager",
		"--property=ActiveState",
		"--property=MainPID",
		"--property=InvocationID",
		"--property=ControlGroup",
		service,
	).CombinedOutput()
	if err != nil {
		return serviceState{}, commandError(err, output)
	}
	state := serviceState{}
	for line := range strings.SplitSeq(string(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "ActiveState":
			state.ActiveState = value
		case "MainPID":
			pid, err := strconv.Atoi(value)
			if err != nil {
				return serviceState{}, fmt.Errorf("parse %s MainPID: %w", service, err)
			}
			state.MainPID = pid
		case "InvocationID":
			state.Invocation = value
		case "ControlGroup":
			state.ControlGroup = value
		}
	}
	return state, nil
}

func (systemdController) Stop(ctx context.Context, service string) error {
	output, err := exec.CommandContext(ctx, "systemctl", "stop", service).CombinedOutput()
	if err != nil {
		return commandError(err, output)
	}
	return nil
}

func (systemdController) Start(ctx context.Context, service string) error {
	output, err := exec.CommandContext(ctx, "systemctl", "start", service).CombinedOutput()
	if err != nil {
		return commandError(err, output)
	}
	return nil
}

func (systemdController) CgroupEmpty(controlGroup string) (bool, error) {
	clean := filepath.Clean("/" + strings.TrimSpace(controlGroup))
	path := filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(clean, "/"), "cgroup.events")
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	for line := range strings.SplitSeq(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "populated" {
			return fields[1] == "0", nil
		}
	}
	return false, errors.New("cgroup.events has no populated field")
}

func (systemdController) SocketAbsent(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return true, nil
	case err != nil:
		return false, err
	default:
		return false, nil
	}
}

func (systemdController) WaitSocket(ctx context.Context, path string) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := os.Lstat(path)
		if err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-ticker.C:
		}
	}
}

func commandError(err error, output []byte) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, detail)
}

type serviceHealth struct {
	controller serviceController
	client     daemonClient
	invocation string
}

func (h serviceHealth) Check(ctx context.Context) error {
	state, err := h.controller.Inspect(ctx, buildKitService)
	if err != nil {
		return err
	}
	if state.ActiveState != "active" || state.MainPID <= 0 || state.Invocation == "" || state.Invocation != h.invocation {
		return errors.New("build service generation changed or is not active")
	}
	return h.client.Wait(ctx)
}

func OpenFresh(ctx context.Context, cfg Config) (*Builder, func() error, error) {
	return openFresh(ctx, cfg, systemdController{}, func(ctx context.Context, addr string) (daemonClient, error) {
		return bkclient.New(ctx, addr)
	})
}

func openFresh(
	ctx context.Context,
	cfg Config,
	controller serviceController,
	open func(context.Context, string) (daemonClient, error),
) (*Builder, func() error, error) {
	addr, err := cfg.endpoint()
	if err != nil {
		return nil, nil, err
	}
	socket, err := buildKitSocketPath(addr)
	if err != nil {
		return nil, nil, err
	}
	old, err := controller.Inspect(ctx, buildKitService)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect prior %s: %w", buildKitService, err)
	}
	if err := controller.Stop(ctx, buildKitService); err != nil {
		return nil, nil, fmt.Errorf("stop %s: %w", buildKitService, err)
	}
	stopped, err := controller.Inspect(ctx, buildKitService)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect stopped %s: %w", buildKitService, err)
	}
	if stopped.ActiveState != "inactive" || stopped.MainPID != 0 {
		return nil, nil, errors.New("BuildKit service did not become inactive")
	}
	controlGroup := old.ControlGroup
	if stopped.ControlGroup != "" {
		controlGroup = stopped.ControlGroup
	}
	if controlGroup == "" {
		return nil, nil, errors.New("BuildKit service control group is unavailable")
	}
	empty, err := controller.CgroupEmpty(controlGroup)
	if err != nil {
		return nil, nil, fmt.Errorf("prove BuildKit cgroup empty: %w", err)
	}
	if !empty {
		return nil, nil, errors.New("BuildKit delegated cgroup remains populated")
	}
	absent, err := controller.SocketAbsent(socket)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect stale BuildKit socket: %w", err)
	}
	if !absent {
		return nil, nil, errors.New("stale BuildKit socket remains after service stop")
	}
	if err := controller.Start(ctx, buildKitService); err != nil {
		return nil, nil, fmt.Errorf("start %s: %w", buildKitService, err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, buildKitReadyTimeout)
	defer cancel()
	if err := controller.WaitSocket(readyCtx, socket); err != nil {
		return nil, nil, fmt.Errorf("wait for fresh BuildKit socket: %w", err)
	}
	started, err := controller.Inspect(readyCtx, buildKitService)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect started %s: %w", buildKitService, err)
	}
	if started.ActiveState != "active" || started.MainPID <= 0 || started.Invocation == "" {
		return nil, nil, errors.New("BuildKit service did not start a valid generation")
	}
	if old.Invocation != "" && started.Invocation == old.Invocation {
		return nil, nil, errors.New("BuildKit service invocation did not change")
	}
	client, err := open(readyCtx, addr)
	if err != nil {
		return nil, nil, err
	}
	if err := client.Wait(readyCtx); err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("wait for fresh BuildKit API: %w", err)
	}
	builder := New(client, cfg.OutputRoot, cfg.CacheNamespace)
	builder.health = serviceHealth{controller: controller, client: client, invocation: started.Invocation}
	return builder, client.Close, nil
}

func buildKitSocketPath(addr string) (string, error) {
	const prefix = "unix://"
	if !strings.HasPrefix(addr, prefix) {
		return "", errors.New("BuildKit service endpoint must be a Unix socket")
	}
	path := strings.TrimPrefix(addr, prefix)
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("BuildKit service socket path must be absolute and clean")
	}
	return path, nil
}
