package executor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/localcache"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/runtime"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"golang.org/x/sync/errgroup"
)

type WorkspaceMaterializer struct {
	Connector             vm.Connector
	CAS                   cas.Store
	Sessions              WorkspaceMountSessionRegistry
	TempDir               string
	Heartbeat             time.Duration
	StartupTimeout        time.Duration
	FailureTimeout        time.Duration
	PollEvery             time.Duration
	ClaimErrorBackoff     time.Duration
	CompleteErrorBackoff  time.Duration
	Network               compute.NetworkPolicy
	Log                   *slog.Logger
	ArtifactCacheDir      string
	ArtifactCacheMaxBytes int64
	Substrates            RuntimeSubstrateResolver
	RuntimePool           *PreparedRuntimePool
	BackgroundGate        *BackgroundWorkGate
}

func (m WorkspaceMaterializer) RunWorkspaceMount(ctx context.Context, mount api.WorkerWorkspaceMount, client api.WorkerWorkspaceMaterializerControlClient) error {
	if m.Connector == nil {
		return errors.New("workspace materializer connector is required")
	}
	endForeground := m.beginForegroundWorkspaceMount()
	foregroundActive := true
	defer func() {
		if foregroundActive {
			endForeground()
		}
	}()
	totalStarted := time.Now()
	m.logWorkspaceMountPhase(mount, "workspace mount started", "state", "starting")
	renewEvery := m.Heartbeat
	if renewEvery <= 0 {
		renewEvery = 15 * time.Second
	}
	startupCtx, cancelStartup := context.WithTimeout(ctx, m.startupTimeout())
	defer cancelStartup()
	phaseStarted := time.Now()
	rawSession, sandboxImagePath, workspaceArtifactPath, cleanup, preparedRuntimeKey, usePreparedRuntime, err := m.materializeSession(startupCtx, &mount)
	m.logWorkspaceMountPhase(mount, "workspace mount session materialized", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", errorString(err))
	if err != nil {
		cleanup()
		_ = m.failWorkspaceMount(client, mount, err)
		return fmt.Errorf("connect workspace mount guest: %w", err)
	}
	renewal := m.startRenewalLoop(ctx, api.WorkerWorkspaceMountRenewRequest{
		OrgID:                mount.OrgID,
		WorkspaceMountID:     mount.ID,
		RuntimeInstanceToken: mount.RuntimeInstanceToken,
	}, client, renewEvery)
	defer renewal.stopAndWait()
	session := newManagedWorkspaceMountSession(rawSession)
	defer cleanup()
	defer func() { _ = m.closeSession(session) }()
	phaseStarted = time.Now()
	if err := m.registerWorkspaceMountContext(startupCtx, session, mount, sandboxImagePath, workspaceArtifactPath, preparedRuntimeKey, usePreparedRuntime); err != nil {
		m.logWorkspaceMountPhase(mount, "workspace mount guest registered", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", err.Error())
		if renewalErr := renewal.stopAndWait(); renewalErr != nil {
			err = renewalErr
		}
		_ = m.failWorkspaceMount(client, mount, err)
		return err
	}
	m.logWorkspaceMountPhase(mount, "workspace mount guest registered", "duration_ms", time.Since(phaseStarted).Milliseconds())
	phaseStarted = time.Now()
	mounted, err := client.MarkWorkspaceMountMounted(renewal.ctx, api.WorkerWorkspaceMountMountedRequest{
		OrgID:                mount.OrgID,
		WorkspaceMountID:     mount.ID,
		RuntimeInstanceToken: mount.RuntimeInstanceToken,
	})
	m.logWorkspaceMountPhase(mount, "workspace mount marked mounted", "duration_ms", time.Since(phaseStarted).Milliseconds(), "state", strings.TrimSpace(mounted.State), "error", errorString(err))
	if err != nil {
		if renewalErr := renewal.stopAndWait(); renewalErr != nil {
			err = renewalErr
		}
		_ = m.failWorkspaceMount(client, mount, err)
		return fmt.Errorf("mark workspace mount mounted: %w", err)
	}
	switch strings.TrimSpace(mounted.State) {
	case "unmounting":
		if err := m.stopControlledWorkspaceMount(renewal.ctx, session, mount, mounted, client); err != nil {
			return err
		}
		_ = renewal.stopAndWait()
		return nil
	}
	unregisterSession := func() {}
	if m.Sessions != nil {
		unregisterSession = m.Sessions.RegisterWorkspaceMountSession(mount, session, m.channelToken(mount))
	}
	defer unregisterSession()
	endForeground()
	foregroundActive = false
	m.logWorkspaceMountPhase(mount, "workspace mount ready", "duration_ms", time.Since(totalStarted).Milliseconds())
	sessionExited := make(chan error, 1)
	go func() {
		sessionExited <- session.Wait(renewal.ctx)
	}()
	eventStream, err := m.openWorkspaceEventStream(renewal.ctx, session, mount)
	if err != nil {
		if renewalErr := renewal.stopAndWait(); renewalErr != nil {
			err = renewalErr
		}
		_ = m.failWorkspaceMount(client, mount, err)
		return fmt.Errorf("open workspace event stream: %w", err)
	}
	eventLoopExited := make(chan error, 1)
	go func() {
		eventLoopExited <- m.runWorkspaceEventLoop(renewal.ctx, session, eventStream, mount, client)
	}()
	inputRelays := newWorkspaceInputRelayRegistry()
	inputRelayExited := make(chan error, 1)
	failAndReturn := func(cause error) error {
		if ctx.Err() == nil {
			_ = m.failWorkspaceMount(client, mount, cause)
		}
		return cause
	}
	stopAndReturn := func() error {
		_ = renewal.stopAndWait()
		return ctx.Err()
	}
	pollEvery := m.PollEvery
	if pollEvery <= 0 {
		pollEvery = 500 * time.Millisecond
	}
	claimErrorBackoff := m.ClaimErrorBackoff
	if claimErrorBackoff <= 0 {
		claimErrorBackoff = 2 * time.Second
	}
	completeErrorBackoff := m.CompleteErrorBackoff
	if completeErrorBackoff <= 0 {
		completeErrorBackoff = 250 * time.Millisecond
	}
	poll := time.NewTimer(0)
	defer poll.Stop()
	renewDone := renewal.done
	renewUpdates := renewal.updates
	for {
		select {
		case <-ctx.Done():
			return stopAndReturn()
		case update := <-renewUpdates:
			switch strings.TrimSpace(update.State) {
			case "unmounting":
				if err := m.stopControlledWorkspaceMount(renewal.ctx, session, mount, update, client); err != nil {
					return err
				}
				_ = renewal.stopAndWait()
				return nil
			}
		case err := <-renewDone:
			renewDone = nil
			renewal.once.Do(func() { renewal.err = err })
			if err != nil {
				return failAndReturn(err)
			}
		case err := <-sessionExited:
			sessionExited = nil
			if released, releaseErr := session.CheckpointReleaseResult(context.Background()); released {
				_ = renewal.stopAndWait()
				if releaseErr != nil {
					return failAndReturn(workspaceMountFailure{
						code: "workspace_mount_checkpoint_release_failed",
						err:  fmt.Errorf("release checkpoint source: %w", releaseErr),
					})
				}
				return nil
			}
			if renewal.ctx.Err() != nil {
				continue
			}
			if ctx.Err() != nil {
				return stopAndReturn()
			}
			if err == nil {
				err = errors.New("workspace mount session exited")
			}
			return failAndReturn(workspaceMountFailure{
				code: "workspace_mount_vm_exited",
				err:  fmt.Errorf("workspace mount VM exited: %w", err),
			})
		case err := <-eventLoopExited:
			eventLoopExited = nil
			if released, releaseErr := session.CheckpointReleaseResult(context.Background()); released {
				_ = renewal.stopAndWait()
				if releaseErr != nil {
					return failAndReturn(workspaceMountFailure{
						code: "workspace_mount_checkpoint_release_failed",
						err:  fmt.Errorf("release checkpoint source: %w", releaseErr),
					})
				}
				return nil
			}
			if renewal.ctx.Err() != nil {
				continue
			}
			if ctx.Err() != nil {
				return stopAndReturn()
			}
			if err == nil {
				err = errors.New("workspace mount event stream exited")
			}
			return failAndReturn(workspaceMountFailure{
				code: "workspace_mount_event_stream_lost",
				err:  fmt.Errorf("workspace mount event stream exited: %w", err),
			})
		case err := <-inputRelayExited:
			if released, releaseErr := session.CheckpointReleaseResult(context.Background()); released {
				_ = renewal.stopAndWait()
				if releaseErr != nil {
					return failAndReturn(workspaceMountFailure{
						code: "workspace_mount_checkpoint_release_failed",
						err:  fmt.Errorf("release checkpoint source: %w", releaseErr),
					})
				}
				return nil
			}
			if renewal.ctx.Err() != nil {
				continue
			}
			if ctx.Err() != nil {
				return stopAndReturn()
			}
			if err != nil {
				return failAndReturn(err)
			}
		case <-poll.C:
			claimed, err := client.ClaimWorkspaceOperation(renewal.ctx, api.WorkerWorkspaceOperationClaimRequest{
				OrgID:                mount.OrgID,
				WorkspaceMountID:     mount.ID,
				RuntimeInstanceToken: mount.RuntimeInstanceToken,
			})
			if err != nil {
				poll.Reset(claimErrorBackoff)
				continue
			}
			if claimed.Operation == nil {
				poll.Reset(pollEvery)
				continue
			}
			if err := startWorkspaceOperation(renewal.ctx, client, api.WorkerWorkspaceOperationStartRequest{
				OrgID:       claimed.Operation.OrgID,
				OperationID: claimed.Operation.ID,
				ClaimToken:  claimed.Operation.ClaimToken,
			}, completeErrorBackoff); err != nil {
				poll.Reset(completeErrorBackoff)
				continue
			}
			complete, err := m.dispatchOperation(renewal.ctx, session, mount, *claimed.Operation)
			if err != nil {
				complete = api.WorkerWorkspaceOperationCompleteRequest{
					OrgID:       claimed.Operation.OrgID,
					OperationID: claimed.Operation.ID,
					ClaimToken:  claimed.Operation.ClaimToken,
					Error:       workspaceOperationDispatchError(err),
				}
			}
			if err := completeWorkspaceOperation(renewal.ctx, client, complete, completeErrorBackoff); err != nil {
				return failAndReturn(fmt.Errorf("complete workspace operation: %w", err))
			}
			if err == nil && len(complete.Error) == 0 {
				m.startWorkspaceInputRelay(renewal.ctx, session, mount, *claimed.Operation, client, inputRelayExited, inputRelays)
			}
			poll.Reset(pollEvery)
		}
	}
}

func (m WorkspaceMaterializer) logWorkspaceMountPhase(mount api.WorkerWorkspaceMount, message string, attrs ...any) {
	log := m.Log
	if log == nil {
		log = slog.Default()
	}
	base := []any{
		"workspace_id", strings.TrimSpace(mount.WorkspaceID),
		"workspace_mount_id", strings.TrimSpace(mount.ID),
		"org_id", strings.TrimSpace(mount.OrgID),
		"project_id", strings.TrimSpace(mount.ProjectID),
		"environment_id", strings.TrimSpace(mount.EnvironmentID),
	}
	base = append(base, attrs...)
	log.Info(message, base...)
}

func (m WorkspaceMaterializer) beginForegroundWorkspaceMount() func() {
	if m.BackgroundGate == nil {
		return func() {}
	}
	return m.BackgroundGate.BeginForeground()
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type workspaceMountRenewal struct {
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan error
	updates chan api.WorkspaceMountResponse
	once    sync.Once
	err     error
}

func (r *workspaceMountRenewal) stopAndWait() error {
	r.once.Do(func() {
		r.cancel()
		r.err = <-r.done
	})
	return r.err
}

func (m WorkspaceMaterializer) startRenewalLoop(ctx context.Context, request api.WorkerWorkspaceMountRenewRequest, client api.WorkerWorkspaceMaterializerControlClient, every time.Duration) *workspaceMountRenewal {
	renewCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	updates := make(chan api.WorkspaceMountResponse, 1)
	go func() {
		var err error
		defer func() { done <- err }()
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				response, renewErr := client.RenewWorkspaceMount(renewCtx, request)
				if renewErr != nil {
					err = fmt.Errorf("renew workspace mount: %w", renewErr)
					cancel()
					return
				}
				select {
				case updates <- response:
				default:
				}
			}
		}
	}()
	return &workspaceMountRenewal{ctx: renewCtx, cancel: cancel, done: done, updates: updates}
}

type workspaceMountFailure struct {
	code string
	err  error
}

func (e workspaceMountFailure) Error() string {
	if e.err == nil {
		return e.code
	}
	return e.err.Error()
}

func (e workspaceMountFailure) Unwrap() error {
	return e.err
}

func (m WorkspaceMaterializer) materializeSession(ctx context.Context, mount *api.WorkerWorkspaceMount) (vm.Session, string, string, func(), string, bool, error) {
	if mount == nil {
		return nil, "", "", func() {}, "", false, workspaceMountFailure{code: "workspace_mount_missing", err: errors.New("workspace mount is required")}
	}
	if m.CAS == nil {
		return nil, "", "", func() {}, "", false, workspaceMountFailure{code: "workspace_mount_cas_unconfigured", err: errors.New("workspace materializer CAS is required")}
	}
	connector, ok := m.Connector.(vm.MaterializingConnector)
	if !ok {
		return nil, "", "", func() {}, "", false, workspaceMountFailure{code: "workspace_mount_connector_unsupported", err: errors.New("workspace materializer connector does not support artifact mount")}
	}
	tempDir := strings.TrimSpace(m.TempDir)
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return nil, "", "", func() {}, "", false, workspaceMountFailure{code: "workspace_mount_temp_unavailable", err: fmt.Errorf("create mount temp dir: %w", err)}
	}
	if m.RuntimePool != nil {
		if session, key, runtimeInstanceToken, ok := m.RuntimePool.Checkout(ctx, *mount); ok {
			mount.RuntimeInstanceToken = strings.TrimSpace(runtimeInstanceToken)
			if mount.RuntimeInstanceToken == "" {
				_ = session.Close(context.Background())
				return nil, "", "", func() {}, key, true, workspaceMountFailure{code: "runtime_instance_token_unavailable", err: errors.New("prepared runtime checkout returned empty instance token")}
			}
			workspaceArtifact := api.CASObject{
				Digest:    strings.TrimSpace(mount.WorkspaceArtifact.Digest),
				SizeBytes: mount.WorkspaceArtifact.SizeBytes,
				MediaType: strings.TrimSpace(mount.WorkspaceArtifact.MediaType),
			}
			phaseStarted := time.Now()
			workspacePath, cleanupWorkspace, err := m.restoreCASObject(ctx, tempDir, "workspace-version", workspaceArtifact)
			m.logWorkspaceMountPhase(*mount, "workspace mount workspace artifact restored", "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", workspaceArtifact.SizeBytes, "error", errorString(err), "prepared_runtime_hit", true)
			if err != nil {
				_ = session.Close(context.Background())
				return nil, "", "", cleanupWorkspace, key, true, err
			}
			m.logWorkspaceMountPhase(*mount, "workspace mount prepared runtime checked out", "runtime_key_id", runtime.ID(key))
			return session, "", workspacePath, cleanupWorkspace, key, true, nil
		}
	}
	mount.RuntimeInstanceID = strings.TrimSpace(mount.RuntimeInstanceID)
	mount.RuntimeInstanceToken = strings.TrimSpace(mount.RuntimeInstanceToken)
	if mount.RuntimeInstanceID == "" {
		return nil, "", "", func() {}, "", false, workspaceMountFailure{code: "runtime_instance_missing", err: errors.New("workspace mount claim must include a runtime instance id")}
	}
	if mount.RuntimeInstanceToken == "" {
		return nil, "", "", func() {}, "", false, workspaceMountFailure{code: "runtime_instance_token_unavailable", err: errors.New("workspace mount claim must include a runtime instance token")}
	}
	runtimeKey := preparedRuntimeKeyFromWorkspaceMount(*mount, m.Network)
	m.logWorkspaceMountPhase(*mount, "workspace mount runtime instance claimed", "runtime_instance_id", mount.RuntimeInstanceID, "runtime_key_id", runtime.ID(runtimeKey))
	workspaceArtifact := api.CASObject{
		Digest:    strings.TrimSpace(mount.WorkspaceArtifact.Digest),
		SizeBytes: mount.WorkspaceArtifact.SizeBytes,
		MediaType: strings.TrimSpace(mount.WorkspaceArtifact.MediaType),
	}
	if strings.TrimSpace(mount.BaseVersionID) == "" {
		return nil, "", "", func() {}, "", false, workspaceMountFailure{code: "workspace_version_missing", err: errors.New("workspace mount base_version_id is required")}
	}
	if strings.TrimSpace(mount.WorkspaceMountPath) == "" {
		return nil, "", "", func() {}, "", false, workspaceMountFailure{code: "workspace_mount_path_missing", err: errors.New("workspace mount mount path is required")}
	}
	if strings.TrimSpace(mount.WorkspaceArtifact.Encoding) != workspace.ArtifactEncoding {
		return nil, "", "", func() {}, "", false, workspaceMountFailure{code: "workspace_version_artifact_incompatible", err: fmt.Errorf("workspace artifact encoding %q is not supported", mount.WorkspaceArtifact.Encoding)}
	}
	var (
		sandboxImagePath    string
		workspacePath       string
		session             vm.Session
		cleanupSandboxImage = func() {}
		cleanupWorkspace    = func() {}
	)
	cleanup := func() {
		cleanupWorkspace()
		cleanupSandboxImage()
	}
	var group errgroup.Group
	group.Go(func() error {
		phaseStarted := time.Now()
		path, cleanupFn, err := m.restoreCASObject(ctx, tempDir, "sandbox-image", mount.SandboxImageArtifact)
		cleanupSandboxImage = cleanupFn
		sandboxImagePath = path
		m.logWorkspaceMountPhase(*mount, "workspace mount sandbox image restored", "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", mount.SandboxImageArtifact.SizeBytes, "error", errorString(err))
		return err
	})
	group.Go(func() error {
		phaseStarted := time.Now()
		path, cleanupFn, err := m.restoreCASObject(ctx, tempDir, "workspace-version", workspaceArtifact)
		cleanupWorkspace = cleanupFn
		workspacePath = path
		m.logWorkspaceMountPhase(*mount, "workspace mount workspace artifact restored", "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", workspaceArtifact.SizeBytes, "error", errorString(err))
		return err
	})
	if m.Substrates == nil {
		group.Go(func() error {
			phaseStarted := time.Now()
			materialized, err := connector.Materialize(ctx, vm.MaterializeRequest{
				ID:                 mount.ID,
				RootfsDigest:       mount.RootfsDigest,
				ImageDigest:        mount.ImageDigest,
				ImageFormat:        mount.ImageFormat,
				WorkspaceMountPath: mount.WorkspaceMountPath,
				BaseVersionID:      mount.BaseVersionID,
				Resources: compute.ResourceVector{
					MilliCPU:  mount.RequestedMilliCPU,
					MemoryMiB: mount.RequestedMemoryMiB,
					DiskMiB:   mount.RequestedDiskMiB,
					Slots:     mount.RequestedExecutionSlots,
				},
				Network: m.networkPolicy(),
			})
			session = materialized
			m.logWorkspaceMountPhase(*mount, "workspace mount connector materialized", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", errorString(err))
			if err != nil {
				return workspaceMountFailure{code: "workspace_sandbox_abi_incompatible", err: err}
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		if session != nil {
			_ = session.Close(context.Background())
		}
		cleanup()
		return nil, "", "", func() {}, "", false, err
	}
	if m.Substrates != nil {
		phaseStarted := time.Now()
		topology, err := runtimeSubstrateTopology(ctx, m.Substrates, sandboxImagePath, *mount)
		m.logWorkspaceMountPhase(*mount, "workspace mount substrate resolved", "duration_ms", time.Since(phaseStarted).Milliseconds(), "substrate_digest", runtimeSubstrateDigest(topology), "error", errorString(err))
		if err != nil {
			cleanup()
			return nil, "", "", func() {}, "", false, workspaceMountFailure{code: "workspace_sandbox_substrate_unavailable", err: err}
		}
		phaseStarted = time.Now()
		materialized, err := connector.Materialize(ctx, vm.MaterializeRequest{
			ID:                 mount.ID,
			RootfsDigest:       mount.RootfsDigest,
			ImageDigest:        mount.ImageDigest,
			ImageFormat:        mount.ImageFormat,
			WorkspaceMountPath: mount.WorkspaceMountPath,
			BaseVersionID:      mount.BaseVersionID,
			Resources: compute.ResourceVector{
				MilliCPU:  mount.RequestedMilliCPU,
				MemoryMiB: mount.RequestedMemoryMiB,
				DiskMiB:   mount.RequestedDiskMiB,
				Slots:     mount.RequestedExecutionSlots,
			},
			Network:  m.networkPolicy(),
			Topology: topology,
		})
		session = materialized
		m.logWorkspaceMountPhase(*mount, "workspace mount connector materialized", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", errorString(err))
		if err != nil {
			cleanup()
			return nil, "", "", func() {}, "", false, workspaceMountFailure{code: "workspace_sandbox_abi_incompatible", err: err}
		}
	}
	if session == nil {
		cleanup()
		return nil, "", "", func() {}, "", false, workspaceMountFailure{code: "workspace_sandbox_abi_incompatible", err: errors.New("workspace mount connector returned nil session")}
	}
	return session, sandboxImagePath, workspacePath, cleanup, "", false, nil
}

func (m WorkspaceMaterializer) restoreCASObject(ctx context.Context, tempDir string, label string, artifact api.CASObject) (string, func(), error) {
	cleanup := func() {}
	codeLabel := strings.ReplaceAll(label, "-", "_")
	digest := strings.TrimSpace(artifact.Digest)
	if digest == "" {
		return "", cleanup, workspaceMountFailure{code: codeLabel + "_artifact_missing", err: errors.New(label + " artifact digest is required")}
	}
	if artifact.SizeBytes <= 0 {
		return "", cleanup, workspaceMountFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("%s artifact size_bytes must be positive", label)}
	}
	mediaType := strings.TrimSpace(artifact.MediaType)
	if mediaType == "" {
		return "", cleanup, workspaceMountFailure{code: codeLabel + "_artifact_missing", err: fmt.Errorf("%s artifact media_type is required", label)}
	}
	stat, err := m.CAS.Stat(ctx, digest)
	if err != nil {
		return "", cleanup, workspaceMountFailure{code: codeLabel + "_artifact_missing", err: fmt.Errorf("stat %s artifact: %w", label, err)}
	}
	if stat.SizeBytes != artifact.SizeBytes || strings.TrimSpace(stat.MediaType) != mediaType {
		return "", cleanup, workspaceMountFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("%s artifact metadata mismatch", label)}
	}
	if cacheDir := strings.TrimSpace(m.ArtifactCacheDir); cacheDir != "" {
		return m.restoreCASObjectWithCache(ctx, tempDir, cacheDir, label, codeLabel, artifact)
	}
	return m.restoreCASObjectUncached(ctx, tempDir, label, codeLabel, artifact)
}

func (m WorkspaceMaterializer) restoreCASObjectUncached(ctx context.Context, tempDir string, label string, codeLabel string, artifact api.CASObject) (string, func(), error) {
	cleanup := func() {}
	digest := strings.TrimSpace(artifact.Digest)
	reader, err := m.CAS.Get(ctx, digest)
	if err != nil {
		return "", cleanup, workspaceMountFailure{code: codeLabel + "_artifact_missing", err: fmt.Errorf("get %s artifact: %w", label, err)}
	}
	defer reader.Close()
	file, err := os.CreateTemp(tempDir, label+"-*")
	if err != nil {
		return "", cleanup, workspaceMountFailure{code: "workspace_mount_temp_unavailable", err: fmt.Errorf("create %s artifact temp file: %w", label, err)}
	}
	path := file.Name()
	cleanup = func() { _ = os.Remove(path) }
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), reader)
	closeErr := file.Close()
	if copyErr != nil {
		cleanup()
		return "", func() {}, workspaceMountFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("copy %s artifact: %w", label, copyErr)}
	}
	if closeErr != nil {
		cleanup()
		return "", func() {}, workspaceMountFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("close %s artifact: %w", label, closeErr)}
	}
	if written != artifact.SizeBytes {
		cleanup()
		return "", func() {}, workspaceMountFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("%s artifact size mismatch", label)}
	}
	if sha256sum.DigestHash(hash) != digest {
		cleanup()
		return "", func() {}, workspaceMountFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("%s artifact digest mismatch", label)}
	}
	return path, cleanup, nil
}

func (m WorkspaceMaterializer) restoreCASObjectWithCache(ctx context.Context, tempDir string, cacheDir string, label string, codeLabel string, artifact api.CASObject) (string, func(), error) {
	cachePath, err := artifactCachePath(cacheDir, artifact.Digest)
	if err != nil {
		return "", func() {}, workspaceMountFailure{code: codeLabel + "_artifact_missing", err: err}
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return "", func() {}, workspaceMountFailure{code: "workspace_mount_cache_unavailable", err: fmt.Errorf("create %s artifact cache dir: %w", label, err)}
	}
	cacheRoot := filepath.Join(cacheDir, "sha256")
	var linkedPath string
	var linkedCleanup func()
	err = localcache.WithRootLock(cacheRoot, func(lock localcache.RootLock) error {
		if err := validateCachedArtifact(cachePath, artifact); err == nil {
			if touchErr := localcache.Touch(cachePath); touchErr == nil {
				path, cleanup, linkErr := linkCachedArtifact(tempDir, label, cachePath)
				if linkErr == nil {
					linkedPath = path
					linkedCleanup = cleanup
					return nil
				}
			}
			_ = os.Remove(cachePath)
		}
		return errArtifactCacheMiss
	})
	if err == nil {
		return linkedPath, linkedCleanup, nil
	}
	if !errors.Is(err, errArtifactCacheMiss) {
		return "", func() {}, workspaceMountFailure{code: "workspace_mount_cache_unavailable", err: fmt.Errorf("open %s artifact cache: %w", label, err)}
	}
	reader, err := m.CAS.Get(ctx, strings.TrimSpace(artifact.Digest))
	if err != nil {
		return "", func() {}, workspaceMountFailure{code: codeLabel + "_artifact_missing", err: fmt.Errorf("get %s artifact: %w", label, err)}
	}
	defer reader.Close()
	staged, err := os.CreateTemp(filepath.Dir(cachePath), ".staging-"+filepath.Base(cachePath)+"-*")
	if err != nil {
		return "", func() {}, workspaceMountFailure{code: "workspace_mount_cache_unavailable", err: fmt.Errorf("stage %s artifact cache: %w", label, err)}
	}
	stagedPath := staged.Name()
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(staged, hash), reader)
	closeErr := staged.Close()
	if copyErr != nil {
		_ = os.Remove(stagedPath)
		return "", func() {}, workspaceMountFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("copy %s artifact: %w", label, copyErr)}
	}
	if closeErr != nil {
		_ = os.Remove(stagedPath)
		return "", func() {}, workspaceMountFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("close %s artifact cache: %w", label, closeErr)}
	}
	if written != artifact.SizeBytes {
		_ = os.Remove(stagedPath)
		return "", func() {}, workspaceMountFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("%s artifact size mismatch", label)}
	}
	if sha256sum.DigestHash(hash) != strings.TrimSpace(artifact.Digest) {
		_ = os.Remove(stagedPath)
		return "", func() {}, workspaceMountFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("%s artifact digest mismatch", label)}
	}
	if err := os.Chmod(stagedPath, 0o644); err != nil {
		_ = os.Remove(stagedPath)
		return "", func() {}, workspaceMountFailure{code: "workspace_mount_cache_unavailable", err: fmt.Errorf("chmod %s artifact cache: %w", label, err)}
	}
	defer func() {
		if stagedPath != "" {
			_ = os.Remove(stagedPath)
		}
	}()
	err = localcache.WithRootLock(cacheRoot, func(lock localcache.RootLock) error {
		if err := validateCachedArtifact(cachePath, artifact); err == nil {
			if touchErr := localcache.Touch(cachePath); touchErr == nil {
				_ = os.Remove(stagedPath)
				stagedPath = ""
				path, cleanup, linkErr := linkCachedArtifact(tempDir, label, cachePath)
				if linkErr == nil {
					linkedPath = path
					linkedCleanup = cleanup
					return nil
				}
			}
			_ = os.Remove(cachePath)
		}
		if err := os.Rename(stagedPath, cachePath); err != nil {
			return fmt.Errorf("publish %s artifact cache: %w", label, err)
		}
		stagedPath = ""
		if _, err := lock.EnforceByteLimit(m.ArtifactCacheMaxBytes, cleanArtifactCachePreserveSet(map[string]bool{cachePath: true})); err != nil {
			return fmt.Errorf("evict %s artifact cache: %w", label, err)
		}
		path, cleanup, err := linkCachedArtifact(tempDir, label, cachePath)
		if err != nil {
			_ = os.Remove(cachePath)
			return fmt.Errorf("link %s artifact cache: %w", label, err)
		}
		linkedPath = path
		linkedCleanup = cleanup
		return nil
	})
	if err != nil {
		return "", func() {}, workspaceMountFailure{code: "workspace_mount_cache_unavailable", err: err}
	}
	return linkedPath, linkedCleanup, nil
}

var errArtifactCacheMiss = errors.New("artifact cache miss")

func artifactCachePath(cacheDir string, digest string) (string, error) {
	hash, ok := strings.CutPrefix(strings.TrimSpace(digest), "sha256:")
	if !ok || len(hash) != 64 {
		return "", fmt.Errorf("unsupported artifact digest %q", digest)
	}
	return filepath.Join(cacheDir, "sha256", hash), nil
}

func cleanArtifactCachePreserveSet(paths map[string]bool) map[string]bool {
	if len(paths) == 0 {
		return nil
	}
	cleaned := make(map[string]bool, len(paths))
	for path, keep := range paths {
		if keep {
			cleaned[filepath.Clean(path)] = true
		}
	}
	return cleaned
}

func validateCachedArtifact(path string, artifact api.CASObject) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cached artifact is not a regular file")
	}
	if info.Size() != artifact.SizeBytes {
		return fmt.Errorf("cached artifact size mismatch")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if sha256sum.DigestHash(hash) != strings.TrimSpace(artifact.Digest) {
		return fmt.Errorf("cached artifact digest mismatch")
	}
	return nil
}

func linkCachedArtifact(tempDir string, label string, cachePath string) (string, func(), error) {
	file, err := os.CreateTemp(tempDir, label+"-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create %s artifact temp file: %w", label, err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", func() {}, fmt.Errorf("close %s artifact temp file: %w", label, err)
	}
	if err := os.Remove(path); err != nil {
		return "", func() {}, fmt.Errorf("replace %s artifact temp file: %w", label, err)
	}
	if err := os.Link(cachePath, path); err != nil {
		source, openErr := os.Open(cachePath)
		if openErr != nil {
			return "", func() {}, openErr
		}
		defer source.Close()
		target, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if createErr != nil {
			return "", func() {}, createErr
		}
		_, copyErr := io.Copy(target, source)
		closeErr := target.Close()
		if copyErr != nil {
			_ = os.Remove(path)
			return "", func() {}, copyErr
		}
		if closeErr != nil {
			_ = os.Remove(path)
			return "", func() {}, closeErr
		}
	}
	return path, func() { _ = os.Remove(path) }, nil
}

func (m WorkspaceMaterializer) registerWorkspaceMount(ctx context.Context, session vm.Session, mount api.WorkerWorkspaceMount, sandboxImagePath string, workspaceArtifactPath string, preparedRuntimeKey string, usePreparedRuntime bool) error {
	channelToken := m.channelToken(mount)
	if channelToken == "" {
		return errors.New("workspace mount guest channel token is required")
	}
	if strings.TrimSpace(mount.GuestdChannelTokenHash) == "" {
		return errors.New("workspace mount guest channel token hash is required")
	}
	stream := session.Stream()
	closeStream := func() {}
	if usePreparedRuntime {
		preparedStream, err := session.OpenStream(ctx)
		if err != nil {
			return fmt.Errorf("open prepared runtime materialize stream: %w", err)
		}
		stream = preparedStream
		closeStream = func() { _ = preparedStream.Close() }
	}
	defer closeStream()
	phaseStarted := time.Now()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:        wire.StreamTypeWorkspaceMaterialize,
		WorkspaceID: mount.WorkspaceID,
	}, 0); err != nil {
		m.logWorkspaceMountPhase(mount, "workspace mount header written", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", err.Error())
		return fmt.Errorf("write workspace materialize header: %w", err)
	}
	m.logWorkspaceMountPhase(mount, "workspace mount header written", "duration_ms", time.Since(phaseStarted).Milliseconds())
	request := &workspacev0.MaterializeWorkspaceRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			WorkspaceMountId:  mount.ID,
			WorkspaceId:       mount.WorkspaceID,
			ChannelToken:      channelToken,
			FencingGeneration: uint64(mount.FencingGeneration),
		},
		MountPath:     strings.TrimSpace(mount.WorkspaceMountPath),
		BaseVersionId: strings.TrimSpace(mount.BaseVersionID),
		BaseArtifact: &workspacev0.WorkspaceArtifact{
			Digest:     strings.TrimSpace(mount.WorkspaceArtifact.Digest),
			MediaType:  strings.TrimSpace(mount.WorkspaceArtifact.MediaType),
			Encoding:   strings.TrimSpace(mount.WorkspaceArtifact.Encoding),
			SizeBytes:  uint64(mount.WorkspaceArtifact.SizeBytes),
			EntryCount: uint32(mount.WorkspaceArtifact.EntryCount),
		},
		SandboxArtifact: &workspacev0.WorkspaceArtifact{
			Digest:    strings.TrimSpace(mount.SandboxImageArtifact.Digest),
			MediaType: strings.TrimSpace(mount.SandboxImageArtifact.MediaType),
			Encoding:  strings.TrimSpace(mount.SandboxImageArtifactFormat),
			SizeBytes: uint64(mount.SandboxImageArtifact.SizeBytes),
		},
		UsePreparedRuntime: usePreparedRuntime,
		RuntimeKey:         strings.TrimSpace(preparedRuntimeKey),
	}
	phaseStarted = time.Now()
	if err := frameio.WriteProtoFrame(stream, request); err != nil {
		m.logWorkspaceMountPhase(mount, "workspace mount request written", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", err.Error())
		return fmt.Errorf("write workspace materialize request: %w", err)
	}
	m.logWorkspaceMountPhase(mount, "workspace mount request written", "duration_ms", time.Since(phaseStarted).Milliseconds())
	if !usePreparedRuntime {
		phaseStarted = time.Now()
		if err := wire.WriteFileFrameWithMetadata(stream, wire.StreamHeader{
			Type:        wire.StreamTypeRunImage,
			WorkspaceID: mount.WorkspaceID,
		}, sandboxImagePath, strings.TrimSpace(mount.SandboxImageArtifact.Digest), mount.SandboxImageArtifact.SizeBytes); err != nil {
			m.logWorkspaceMountPhase(mount, "workspace mount sandbox image sent", "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", mount.SandboxImageArtifact.SizeBytes, "error", err.Error())
			return fmt.Errorf("write sandbox image artifact: %w", err)
		}
		m.logWorkspaceMountPhase(mount, "workspace mount sandbox image sent", "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", mount.SandboxImageArtifact.SizeBytes)
	} else {
		m.logWorkspaceMountPhase(mount, "workspace mount sandbox image skipped", "prepared_runtime_hit", true, "runtime_key_id", runtime.ID(strings.TrimSpace(preparedRuntimeKey)), "size_bytes", mount.SandboxImageArtifact.SizeBytes)
	}
	phaseStarted = time.Now()
	if err := wire.WriteFileFrameWithMetadata(stream, wire.StreamHeader{
		Type:        wire.StreamTypeWorkspaceArtifact,
		WorkspaceID: mount.WorkspaceID,
	}, workspaceArtifactPath, strings.TrimSpace(mount.WorkspaceArtifact.Digest), mount.WorkspaceArtifact.SizeBytes); err != nil {
		m.logWorkspaceMountPhase(mount, "workspace mount workspace artifact sent", "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", mount.WorkspaceArtifact.SizeBytes, "error", err.Error())
		return fmt.Errorf("write workspace artifact: %w", err)
	}
	m.logWorkspaceMountPhase(mount, "workspace mount workspace artifact sent", "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", mount.WorkspaceArtifact.SizeBytes)
	var response workspacev0.MaterializeWorkspaceResponse
	phaseStarted = time.Now()
	if err := readProtoFrameFromReaderContext(ctx, session, stream, &response); err != nil {
		m.logWorkspaceMountPhase(mount, "workspace mount response read", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", err.Error())
		return fmt.Errorf("read workspace materialize response: %w", err)
	}
	m.logWorkspaceMountPhase(mount, "workspace mount response read", "duration_ms", time.Since(phaseStarted).Milliseconds(), "state", strings.TrimSpace(response.State))
	for _, guestPhase := range response.GetPhases() {
		if guestPhase == nil {
			continue
		}
		m.logWorkspaceMountPhase(mount, "workspace mount guest phase",
			"guest_phase", strings.TrimSpace(guestPhase.GetName()),
			"duration_ms", guestPhase.GetDurationMs(),
			"size_bytes", guestPhase.GetSizeBytes(),
			"entry_count", guestPhase.GetEntryCount(),
			"error", strings.TrimSpace(guestPhase.GetError()),
		)
	}
	if response.State != "running" {
		if phaseError := workspaceMountPhaseError(response.GetPhases()); phaseError != "" {
			return fmt.Errorf("workspace materialize returned state %q: %s", response.State, phaseError)
		}
		return fmt.Errorf("workspace materialize returned state %q", response.State)
	}
	expectedHash := strings.TrimSpace(mount.GuestdChannelTokenHash)
	if strings.TrimSpace(response.GuestdChannelTokenHash) != expectedHash {
		return errors.New("workspace materialize guest channel token hash mismatch")
	}
	return nil
}

func workspaceMountPhaseError(phases []*workspacev0.WorkspaceMountPhase) string {
	for i := len(phases) - 1; i >= 0; i-- {
		phase := phases[i]
		if phase == nil {
			continue
		}
		message := strings.TrimSpace(phase.GetError())
		if message == "" {
			continue
		}
		name := strings.TrimSpace(phase.GetName())
		if name == "" {
			return message
		}
		return name + ": " + message
	}
	return ""
}

func (m WorkspaceMaterializer) registerWorkspaceMountContext(ctx context.Context, session vm.Session, mount api.WorkerWorkspaceMount, sandboxImagePath string, workspaceArtifactPath string, preparedRuntimeKey string, usePreparedRuntime bool) error {
	result := make(chan error, 1)
	go func() {
		result <- m.registerWorkspaceMount(ctx, session, mount, sandboxImagePath, workspaceArtifactPath, preparedRuntimeKey, usePreparedRuntime)
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = m.closeSession(session)
		return ctx.Err()
	}
}

func (m WorkspaceMaterializer) stopControlledWorkspaceMount(ctx context.Context, session vm.Session, mount api.WorkerWorkspaceMount, update api.WorkspaceMountResponse, client api.WorkerWorkspaceMaterializerControlClient) error {
	capture := strings.TrimSpace(update.State) == "unmounting" && update.DirtyGeneration > 0
	fencingGeneration := max(update.FencingGeneration, mount.FencingGeneration)
	artifact, err := m.stopWorkspaceGuest(ctx, session, mount, fencingGeneration, capture, !capture)
	if err != nil {
		if capture {
			_ = m.failWorkspaceMount(client, mount, workspaceMountFailure{
				code: "workspace_mount_recovery_required",
				err:  fmt.Errorf("capture workspace before stop: %w", err),
			})
		} else {
			_ = m.failWorkspaceMount(client, mount, workspaceMountFailure{
				code: "workspace_mount_stop_failed",
				err:  fmt.Errorf("stop workspace guest: %w", err),
			})
		}
		return err
	}
	if capture {
		if _, err := client.CaptureWorkspaceMount(ctx, api.WorkerWorkspaceMountCaptureRequest{
			OrgID:                mount.OrgID,
			ProjectID:            mount.ProjectID,
			EnvironmentID:        mount.EnvironmentID,
			WorkspaceID:          mount.WorkspaceID,
			WorkspaceMountID:     mount.ID,
			RuntimeInstanceToken: mount.RuntimeInstanceToken,
			ArtifactDigest:       artifact.Digest,
			ArtifactSizeBytes:    artifact.SizeBytes,
			ArtifactMediaType:    artifact.MediaType,
			ArtifactEncoding:     artifact.Encoding,
			ArtifactEntryCount:   int32(artifact.EntryCount),
		}); err != nil {
			_ = m.failWorkspaceMount(client, mount, workspaceMountFailure{
				code: "workspace_mount_recovery_required",
				err:  fmt.Errorf("promote workspace stop capture: %w", err),
			})
			return err
		}
	}
	if capture {
		if _, err := m.stopWorkspaceGuest(ctx, session, mount, fencingGeneration, false, true); err != nil {
			_ = m.failWorkspaceMount(client, mount, workspaceMountFailure{
				code: "workspace_mount_stop_failed",
				err:  fmt.Errorf("finalize workspace stop: %w", err),
			})
			return fmt.Errorf("finalize workspace stop: %w", err)
		}
	}
	if _, err := client.StopWorkspaceMount(context.Background(), api.WorkerWorkspaceMountStopRequest{
		OrgID:                mount.OrgID,
		WorkspaceMountID:     mount.ID,
		RuntimeInstanceToken: mount.RuntimeInstanceToken,
	}); err != nil {
		return fmt.Errorf("stop workspace mount: %w", err)
	}
	return nil
}

func (m WorkspaceMaterializer) stopWorkspaceGuest(ctx context.Context, session vm.Session, mount api.WorkerWorkspaceMount, fencingGeneration int64, capture bool, finalize bool) (workspace.WorkspaceArtifact, error) {
	channelToken := m.channelToken(mount)
	if channelToken == "" {
		return workspace.WorkspaceArtifact{}, errors.New("workspace mount guest channel token is required")
	}
	if m.CAS == nil {
		return workspace.WorkspaceArtifact{}, errors.New("workspace materializer CAS is required")
	}
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("open workspace stop stream: %w", err)
	}
	defer stream.Close()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:        wire.StreamTypeWorkspaceStop,
		WorkspaceID: mount.WorkspaceID,
	}, 0); err != nil {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("write workspace stop header: %w", err)
	}
	if err := frameio.WriteProtoFrame(stream, &workspacev0.StopWorkspaceRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			WorkspaceMountId:  mount.ID,
			WorkspaceId:       mount.WorkspaceID,
			ChannelToken:      channelToken,
			FencingGeneration: uint64(fencingGeneration),
		},
		CaptureBeforeStop: capture,
		FinalizeStop:      finalize,
	}); err != nil {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("write workspace stop request: %w", err)
	}
	var response workspacev0.StopWorkspaceResponse
	if err := readProtoFrameFromReaderContext(ctx, session, stream, &response); err != nil {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("read workspace stop response: %w", err)
	}
	if strings.TrimSpace(response.GetErrorJson()) != "" {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace stop failed: %s", strings.TrimSpace(response.GetErrorJson()))
	}
	expectedState := "stopped"
	if capture && !finalize {
		expectedState = "captured"
	}
	if strings.TrimSpace(response.State) != expectedState {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace stop returned state %q", response.State)
	}
	if !capture {
		return workspace.WorkspaceArtifact{}, nil
	}
	captured := response.GetCapturedArtifact()
	if captured == nil {
		return workspace.WorkspaceArtifact{}, errors.New("workspace stop response missing captured artifact")
	}
	if strings.TrimSpace(captured.GetDigest()) == "" {
		return workspace.WorkspaceArtifact{}, errors.New("workspace stop captured artifact digest is required")
	}
	if strings.TrimSpace(captured.GetMediaType()) != workspace.ArtifactMediaType {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace stop captured artifact media_type %q is unsupported", captured.GetMediaType())
	}
	if strings.TrimSpace(captured.GetEncoding()) != workspace.ArtifactEncoding {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace stop captured artifact encoding %q is unsupported", captured.GetEncoding())
	}
	header, bodyLen, err := wire.ReadStreamFrameHeader(stream)
	if err != nil {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("read workspace stop artifact header: %w", err)
	}
	if header.Type != wire.StreamTypeWorkspaceArtifact {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace stop returned artifact stream type %q", header.Type)
	}
	if strings.TrimSpace(header.WorkspaceID) != strings.TrimSpace(mount.WorkspaceID) {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace stop artifact workspace_id %q does not match %q", header.WorkspaceID, mount.WorkspaceID)
	}
	if uint64(captured.GetSizeBytes()) != bodyLen {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace stop artifact size %d does not match frame size %d", captured.GetSizeBytes(), bodyLen)
	}
	if header.BodyDigest != nil && strings.TrimSpace(*header.BodyDigest) != strings.TrimSpace(captured.GetDigest()) {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace stop artifact digest %q does not match frame digest %q", captured.GetDigest(), *header.BodyDigest)
	}
	body := &io.LimitedReader{R: stream, N: int64(bodyLen)}
	object, err := m.CAS.Put(ctx, workspace.ArtifactMediaType, body)
	if err != nil {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("store workspace stop artifact: %w", err)
	}
	if body.N != 0 {
		return workspace.WorkspaceArtifact{}, errors.New("workspace stop artifact stream ended early")
	}
	if object.Digest != strings.TrimSpace(captured.GetDigest()) || object.SizeBytes != int64(captured.GetSizeBytes()) || object.MediaType != workspace.ArtifactMediaType {
		return workspace.WorkspaceArtifact{}, errors.New("workspace stop artifact CAS metadata mismatch")
	}
	return workspace.WorkspaceArtifact{
		Digest:     object.Digest,
		MediaType:  object.MediaType,
		Encoding:   workspace.ArtifactEncoding,
		SizeBytes:  object.SizeBytes,
		EntryCount: int(captured.GetEntryCount()),
	}, nil
}

func (m WorkspaceMaterializer) dispatchOperation(ctx context.Context, session vm.Session, mount api.WorkerWorkspaceMount, operation api.WorkerWorkspaceOperation) (api.WorkerWorkspaceOperationCompleteRequest, error) {
	channelToken := m.channelToken(mount)
	if channelToken == "" {
		return api.WorkerWorkspaceOperationCompleteRequest{}, errors.New("workspace mount guest channel token is required")
	}
	if strings.TrimSpace(operation.WorkspaceMountID) != strings.TrimSpace(mount.ID) {
		return api.WorkerWorkspaceOperationCompleteRequest{}, fmt.Errorf("claimed operation mount %s does not match live mount %s", operation.WorkspaceMountID, mount.ID)
	}
	if strings.TrimSpace(operation.WorkspaceID) != strings.TrimSpace(mount.WorkspaceID) {
		return api.WorkerWorkspaceOperationCompleteRequest{}, fmt.Errorf("claimed operation workspace %s does not match live workspace %s", operation.WorkspaceID, mount.WorkspaceID)
	}
	if strings.TrimSpace(operation.RequestFingerprint) == "" {
		return api.WorkerWorkspaceOperationCompleteRequest{}, errors.New("claimed operation request_fingerprint is required")
	}
	if operation.OperationExpiresAt.IsZero() || !operation.OperationExpiresAt.After(time.Now()) {
		return api.WorkerWorkspaceOperationCompleteRequest{}, errors.New("claimed operation expired")
	}
	if operation.FencingGeneration <= 0 {
		return api.WorkerWorkspaceOperationCompleteRequest{}, errors.New("claimed operation fencing_generation is required")
	}
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return api.WorkerWorkspaceOperationCompleteRequest{}, fmt.Errorf("open workspace operation stream: %w", err)
	}
	defer stream.Close()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:        wire.StreamTypeWorkspaceOperation,
		WorkspaceID: mount.WorkspaceID,
		OperationID: operation.ID,
	}, 0); err != nil {
		return api.WorkerWorkspaceOperationCompleteRequest{}, fmt.Errorf("write workspace operation header: %w", err)
	}
	if err := frameio.WriteProtoFrame(stream, &workspacev0.WorkspaceOperationRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			OperationId:                operation.ID,
			WorkspaceMountId:           operation.WorkspaceMountID,
			WorkspaceId:                operation.WorkspaceID,
			ChannelToken:               channelToken,
			FencingGeneration:          uint64(operation.FencingGeneration),
			InstanceLeaseId:            operation.InstanceLeaseID,
			WriteLeaseId:               operation.WriteLeaseID,
			FencingToken:               operation.FencingToken,
			OperationExpiresAtUnixNano: operation.OperationExpiresAt.UnixNano(),
			RequestFingerprint:         strings.TrimSpace(operation.RequestFingerprint),
		},
		OperationKind: operation.OperationKind,
		RequestJson:   string(operation.Request),
	}); err != nil {
		return api.WorkerWorkspaceOperationCompleteRequest{}, fmt.Errorf("write workspace operation request: %w", err)
	}
	var result workspacev0.WorkspaceOperationResult
	if err := readProtoFrameFromReaderContext(ctx, session, stream, &result); err != nil {
		return api.WorkerWorkspaceOperationCompleteRequest{}, fmt.Errorf("read workspace operation result: %w", err)
	}
	complete := api.WorkerWorkspaceOperationCompleteRequest{
		OrgID:       operation.OrgID,
		OperationID: operation.ID,
		ClaimToken:  operation.ClaimToken,
	}
	if strings.TrimSpace(result.ErrorJson) != "" {
		complete.Error = json.RawMessage(result.ErrorJson)
	} else if strings.TrimSpace(result.ResultJson) != "" {
		complete.Result = json.RawMessage(result.ResultJson)
	} else {
		complete.Result = json.RawMessage(`{}`)
	}
	return complete, nil
}

func (m WorkspaceMaterializer) openWorkspaceEventStream(ctx context.Context, session vm.Session, mount api.WorkerWorkspaceMount) (io.ReadWriteCloser, error) {
	channelToken := m.channelToken(mount)
	if channelToken == "" {
		return nil, errors.New("workspace mount guest channel token is required")
	}
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("open workspace event stream: %w", err)
	}
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:        wire.StreamTypeWorkspaceEvents,
		WorkspaceID: mount.WorkspaceID,
	}, 0); err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("write workspace event stream header: %w", err)
	}
	if err := frameio.WriteProtoFrame(stream, workspaceEventEnvelope(mount, channelToken)); err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("write workspace event stream envelope: %w", err)
	}
	return stream, nil
}

func workspaceEventEnvelope(mount api.WorkerWorkspaceMount, channelToken string) *workspacev0.WorkspaceOperationEnvelope {
	return &workspacev0.WorkspaceOperationEnvelope{
		WorkspaceMountId:  mount.ID,
		WorkspaceId:       mount.WorkspaceID,
		ChannelToken:      channelToken,
		FencingGeneration: uint64(mount.FencingGeneration),
	}
}

func (m WorkspaceMaterializer) runWorkspaceEventLoop(ctx context.Context, session vm.Session, stream io.ReadWriteCloser, mount api.WorkerWorkspaceMount, client api.WorkerWorkspaceMaterializerControlClient) error {
	defer stream.Close()
	persistState := newWorkspaceOperationEventPersistState()
	for {
		var event workspacev0.WorkspaceOperationEvent
		if err := readProtoFrameFromReaderContext(ctx, session, stream, &event); err != nil {
			return fmt.Errorf("read workspace operation event: %w", err)
		}
		if err := retryWorkspaceOperation(ctx, m.CompleteErrorBackoff, func() error {
			return m.persistWorkspaceOperationEvent(ctx, client, mount, persistState, &event)
		}); err != nil {
			return err
		}
	}
}

type workspaceInputRelayRegistry struct {
	mu     sync.Mutex
	active map[string]struct{}
}

func newWorkspaceInputRelayRegistry() *workspaceInputRelayRegistry {
	return &workspaceInputRelayRegistry{active: make(map[string]struct{})}
}

func (r *workspaceInputRelayRegistry) start(resourceKind string, resourceID string) (func(), bool) {
	key := resourceKind + ":" + resourceID
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.active[key]; ok {
		return nil, false
	}
	r.active[key] = struct{}{}
	return func() {
		r.mu.Lock()
		delete(r.active, key)
		r.mu.Unlock()
	}, true
}

func (m WorkspaceMaterializer) startWorkspaceInputRelay(ctx context.Context, session vm.Session, mount api.WorkerWorkspaceMount, operation api.WorkerWorkspaceOperation, client api.WorkerWorkspaceMaterializerControlClient, failures chan<- error, relays *workspaceInputRelayRegistry) {
	switch strings.TrimSpace(operation.OperationKind) {
	case "StartExec":
		execID := strings.TrimSpace(operation.ResourceID)
		if execID != "" {
			done, ok := relays.start("exec", execID)
			if !ok {
				return
			}
			go m.reportWorkspaceInputRelayFailure(ctx, failures, "exec", execID, func() error {
				defer done()
				return m.runWorkspaceExecInputRelay(ctx, session, mount, operation, execID, client)
			})
		}
	case "CreatePty":
		ptyID := strings.TrimSpace(operation.ResourceID)
		if ptyID != "" {
			done, ok := relays.start("pty", ptyID)
			if !ok {
				return
			}
			go m.reportWorkspaceInputRelayFailure(ctx, failures, "pty", ptyID, func() error {
				defer done()
				return m.runWorkspacePtyInputRelay(ctx, session, mount, operation, ptyID, client)
			})
		}
	}
}

func (m WorkspaceMaterializer) reportWorkspaceInputRelayFailure(ctx context.Context, failures chan<- error, resourceKind string, resourceID string, run func() error) {
	if err := run(); err != nil && ctx.Err() == nil {
		failure := workspaceMountFailure{
			code: "workspace_mount_input_stream_lost",
			err:  fmt.Errorf("workspace %s %s input relay failed: %w", resourceKind, resourceID, err),
		}
		select {
		case failures <- failure:
		case <-ctx.Done():
		}
	}
}

func (m WorkspaceMaterializer) runWorkspaceExecInputRelay(ctx context.Context, session vm.Session, mount api.WorkerWorkspaceMount, operation api.WorkerWorkspaceOperation, execID string, client api.WorkerWorkspaceMaterializerControlClient) error {
	stream, err := m.openWorkspaceInputStream(ctx, session, mount, operation)
	if err != nil {
		return err
	}
	defer stream.Close()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	scope := workerPrimitiveScope(mount)
	closed := false
	deliveredOffset := int64(0)
	for {
		response, err := client.ListWorkspaceExecInput(ctx, api.WorkerWorkspaceExecInputRequest{
			WorkerWorkspacePrimitiveScope: scope,
			ExecID:                        execID,
			Limit:                         100,
		})
		if err != nil {
			return err
		}
		if response.StdinDeliveredCursor > deliveredOffset {
			deliveredOffset = response.StdinDeliveredCursor
		}
		if workspaceExecStateTerminal(response.State) {
			return nil
		}
		for _, chunk := range response.Chunks {
			if err := writeWorkspaceInputChunk(ctx, session, stream, "workspace_exec", execID, "stdin", chunk.OffsetStart, chunk.Data); err != nil {
				return err
			}
			if _, err := client.AdvanceWorkspaceExecInputDelivered(ctx, api.WorkerWorkspaceExecInputDeliveredRequest{
				WorkerWorkspacePrimitiveScope: scope,
				ExecID:                        execID,
				OffsetStart:                   chunk.OffsetStart,
				OffsetEnd:                     chunk.OffsetEnd,
			}); err != nil {
				return err
			}
			deliveredOffset = chunk.OffsetEnd
		}
		if !closed && response.StdinClosedAt != nil && deliveredOffset == response.StdinCursor {
			closed = true
			if err := writeWorkspaceInputClose(ctx, session, stream, "workspace_exec", execID, "stdin", response.StdinCursor); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m WorkspaceMaterializer) runWorkspacePtyInputRelay(ctx context.Context, session vm.Session, mount api.WorkerWorkspaceMount, operation api.WorkerWorkspaceOperation, ptyID string, client api.WorkerWorkspaceMaterializerControlClient) error {
	stream, err := m.openWorkspaceInputStream(ctx, session, mount, operation)
	if err != nil {
		return err
	}
	defer stream.Close()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	scope := workerPrimitiveScope(mount)
	for {
		response, err := client.ListWorkspacePtyInput(ctx, api.WorkerWorkspacePtyInputRequest{
			WorkerWorkspacePrimitiveScope: scope,
			PtyID:                         ptyID,
			Limit:                         100,
		})
		if err != nil {
			return err
		}
		if workspacePtyStateTerminal(response.State) {
			return nil
		}
		for _, chunk := range response.Chunks {
			if err := writeWorkspaceInputChunk(ctx, session, stream, "workspace_pty", ptyID, "input", chunk.OffsetStart, chunk.Data); err != nil {
				return err
			}
			if _, err := client.AdvanceWorkspacePtyInputDelivered(ctx, api.WorkerWorkspacePtyInputDeliveredRequest{
				WorkerWorkspacePrimitiveScope: scope,
				PtyID:                         ptyID,
				OffsetStart:                   chunk.OffsetStart,
				OffsetEnd:                     chunk.OffsetEnd,
			}); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m WorkspaceMaterializer) openWorkspaceInputStream(ctx context.Context, session vm.Session, mount api.WorkerWorkspaceMount, operation api.WorkerWorkspaceOperation) (io.ReadWriteCloser, error) {
	channelToken := m.channelToken(mount)
	if channelToken == "" {
		return nil, errors.New("workspace mount guest channel token is required")
	}
	if strings.TrimSpace(operation.WorkspaceMountID) != strings.TrimSpace(mount.ID) {
		return nil, fmt.Errorf("input relay operation mount %s does not match live mount %s", operation.WorkspaceMountID, mount.ID)
	}
	if strings.TrimSpace(operation.WorkspaceID) != strings.TrimSpace(mount.WorkspaceID) {
		return nil, fmt.Errorf("input relay operation workspace %s does not match live workspace %s", operation.WorkspaceID, mount.WorkspaceID)
	}
	if operation.FencingGeneration <= 0 {
		return nil, errors.New("input relay operation fencing_generation is required")
	}
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("open workspace input stream: %w", err)
	}
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:        wire.StreamTypeWorkspaceInput,
		WorkspaceID: mount.WorkspaceID,
	}, 0); err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("write workspace input header: %w", err)
	}
	if err := frameio.WriteProtoFrame(stream, workspaceInputEnvelope(operation, channelToken)); err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("write workspace input envelope: %w", err)
	}
	return stream, nil
}

func workspaceInputEnvelope(operation api.WorkerWorkspaceOperation, channelToken string) *workspacev0.WorkspaceOperationEnvelope {
	return &workspacev0.WorkspaceOperationEnvelope{
		OperationId:                operation.ID,
		WorkspaceMountId:           operation.WorkspaceMountID,
		WorkspaceId:                operation.WorkspaceID,
		ChannelToken:               channelToken,
		FencingGeneration:          uint64(operation.FencingGeneration),
		InstanceLeaseId:            operation.InstanceLeaseID,
		WriteLeaseId:               operation.WriteLeaseID,
		FencingToken:               operation.FencingToken,
		OperationExpiresAtUnixNano: operation.OperationExpiresAt.UnixNano(),
		RequestFingerprint:         strings.TrimSpace(operation.RequestFingerprint),
	}
}

func writeWorkspaceInputChunk(ctx context.Context, session vm.Session, stream io.ReadWriter, resourceKind string, resourceID string, streamName string, offsetStart int64, data []byte) error {
	if offsetStart < 0 {
		return fmt.Errorf("workspace input offset must be non-negative")
	}
	if err := frameio.WriteProtoFrame(stream, &workspacev0.WorkspaceInputFrame{
		Frame: &workspacev0.WorkspaceInputFrame_Chunk{Chunk: &workspacev0.WorkspaceInputChunk{
			ResourceKind: resourceKind,
			ResourceId:   resourceID,
			Stream:       streamName,
			OffsetStart:  uint64(offsetStart),
			Data:         data,
		}},
	}); err != nil {
		return fmt.Errorf("write workspace input chunk: %w", err)
	}
	var ack workspacev0.WorkspaceStreamAck
	if err := readProtoFrameFromReaderContext(ctx, session, stream, &ack); err != nil {
		return fmt.Errorf("read workspace input ack: %w", err)
	}
	if err := validateWorkspaceInputAck(&ack, resourceKind, resourceID, streamName, offsetStart+int64(len(data))); err != nil {
		return err
	}
	return nil
}

func validateWorkspaceInputAck(ack *workspacev0.WorkspaceStreamAck, resourceKind string, resourceID string, streamName string, durableOffset int64) error {
	if int64(ack.DurableOffset) != durableOffset {
		return fmt.Errorf("workspace input ack offset %d does not match expected offset %d", ack.DurableOffset, durableOffset)
	}
	if strings.TrimSpace(ack.ResourceKind) != resourceKind || strings.TrimSpace(ack.ResourceId) != resourceID || strings.TrimSpace(ack.Stream) != streamName {
		return fmt.Errorf("workspace input ack scope mismatch")
	}
	return nil
}

func writeWorkspaceInputClose(ctx context.Context, session vm.Session, stream io.ReadWriter, resourceKind string, resourceID string, streamName string, offset int64) error {
	if offset < 0 {
		return fmt.Errorf("workspace input close offset must be non-negative")
	}
	if err := frameio.WriteProtoFrame(stream, &workspacev0.WorkspaceInputFrame{
		Frame: &workspacev0.WorkspaceInputFrame_Close{Close: &workspacev0.WorkspaceInputClose{
			ResourceKind: resourceKind,
			ResourceId:   resourceID,
			Stream:       streamName,
			Offset:       uint64(offset),
		}},
	}); err != nil {
		return fmt.Errorf("write workspace input close: %w", err)
	}
	var ack workspacev0.WorkspaceStreamAck
	if err := readProtoFrameFromReaderContext(ctx, session, stream, &ack); err != nil {
		return fmt.Errorf("read workspace input close ack: %w", err)
	}
	return validateWorkspaceInputAck(&ack, resourceKind, resourceID, streamName, offset)
}

func workerPrimitiveScope(mount api.WorkerWorkspaceMount) api.WorkerWorkspacePrimitiveScope {
	return api.WorkerWorkspacePrimitiveScope{
		OrgID:                mount.OrgID,
		ProjectID:            mount.ProjectID,
		EnvironmentID:        mount.EnvironmentID,
		WorkspaceID:          mount.WorkspaceID,
		WorkspaceMountID:     mount.ID,
		RuntimeInstanceToken: mount.RuntimeInstanceToken,
	}
}

func workspaceExecStateTerminal(state string) bool {
	switch strings.TrimSpace(state) {
	case "exited", "terminated", "lost", "failed":
		return true
	default:
		return false
	}
}

func workspacePtyStateTerminal(state string) bool {
	switch strings.TrimSpace(state) {
	case "closed", "lost", "failed":
		return true
	default:
		return false
	}
}

type workspaceOperationEventPersistState struct {
	execOutputOffsets map[string]map[string]int64
	ptyOutputOffsets  map[string]int64
}

func newWorkspaceOperationEventPersistState() *workspaceOperationEventPersistState {
	return &workspaceOperationEventPersistState{
		execOutputOffsets: map[string]map[string]int64{},
		ptyOutputOffsets:  map[string]int64{},
	}
}

func (s *workspaceOperationEventPersistState) seedExecOutputOffsets(execID string, stdoutCursor int64, stderrCursor int64) {
	if s == nil {
		return
	}
	streams := s.execOutputOffsets[execID]
	if streams == nil {
		streams = map[string]int64{}
		s.execOutputOffsets[execID] = streams
	}
	if streams["stdout"] < stdoutCursor {
		streams["stdout"] = stdoutCursor
	}
	if streams["stderr"] < stderrCursor {
		streams["stderr"] = stderrCursor
	}
}

func (s *workspaceOperationEventPersistState) execOutputOffset(execID string, stream string) int64 {
	if s == nil {
		return 0
	}
	streams := s.execOutputOffsets[execID]
	if streams == nil {
		streams = map[string]int64{}
		s.execOutputOffsets[execID] = streams
	}
	return streams[stream]
}

func (s *workspaceOperationEventPersistState) advanceExecOutputOffset(execID string, stream string, offsetEnd int64) {
	if s == nil {
		return
	}
	streams := s.execOutputOffsets[execID]
	if streams == nil {
		streams = map[string]int64{}
		s.execOutputOffsets[execID] = streams
	}
	if streams[stream] < offsetEnd {
		streams[stream] = offsetEnd
	}
}

func (s *workspaceOperationEventPersistState) seedPtyOutputOffset(ptyID string, outputCursor int64) {
	if s == nil {
		return
	}
	if s.ptyOutputOffsets[ptyID] < outputCursor {
		s.ptyOutputOffsets[ptyID] = outputCursor
	}
}

func (s *workspaceOperationEventPersistState) ptyOutputOffset(ptyID string) int64 {
	if s == nil {
		return 0
	}
	return s.ptyOutputOffsets[ptyID]
}

func (s *workspaceOperationEventPersistState) advancePtyOutputOffset(ptyID string, offsetEnd int64) {
	if s == nil {
		return
	}
	if s.ptyOutputOffsets[ptyID] < offsetEnd {
		s.ptyOutputOffsets[ptyID] = offsetEnd
	}
}

func (m WorkspaceMaterializer) persistWorkspaceOperationEvent(ctx context.Context, client api.WorkerWorkspaceMaterializerControlClient, mount api.WorkerWorkspaceMount, state *workspaceOperationEventPersistState, event *workspacev0.WorkspaceOperationEvent) error {
	scope := api.WorkerWorkspacePrimitiveScope{
		OrgID:                mount.OrgID,
		ProjectID:            mount.ProjectID,
		EnvironmentID:        mount.EnvironmentID,
		WorkspaceID:          mount.WorkspaceID,
		WorkspaceMountID:     mount.ID,
		RuntimeInstanceToken: mount.RuntimeInstanceToken,
	}
	switch payload := event.GetEvent().(type) {
	case *workspacev0.WorkspaceOperationEvent_ExecStarted:
		response, err := client.MarkWorkspaceExecStarted(ctx, api.WorkerWorkspaceExecStartedRequest{
			WorkerWorkspacePrimitiveScope: scope,
			ExecID:                        payload.ExecStarted.GetExecId(),
			ProcessID:                     payload.ExecStarted.GetProcessId(),
		})
		if err == nil {
			state.seedExecOutputOffsets(payload.ExecStarted.GetExecId(), response.Exec.StdoutCursor, response.Exec.StderrCursor)
		}
		return err
	case *workspacev0.WorkspaceOperationEvent_ExecStdoutChunk:
		offset := state.execOutputOffset(payload.ExecStdoutChunk.GetExecId(), "stdout")
		response, err := client.AppendWorkspaceExecOutput(ctx, api.WorkerWorkspaceExecOutputRequest{
			WorkerWorkspacePrimitiveScope: scope,
			ExecID:                        payload.ExecStdoutChunk.GetExecId(),
			Chunks: []api.WorkerWorkspaceExecOutputChunk{{
				Stream:      "stdout",
				OffsetStart: &offset,
				Data:        payload.ExecStdoutChunk.GetData(),
			}},
		})
		if err == nil {
			offsetEnd := offset + int64(len(payload.ExecStdoutChunk.GetData()))
			if len(response.Chunks) > 0 {
				offsetEnd = response.Chunks[len(response.Chunks)-1].OffsetEnd
			}
			state.advanceExecOutputOffset(payload.ExecStdoutChunk.GetExecId(), "stdout", offsetEnd)
		}
		return err
	case *workspacev0.WorkspaceOperationEvent_ExecStderrChunk:
		offset := state.execOutputOffset(payload.ExecStderrChunk.GetExecId(), "stderr")
		response, err := client.AppendWorkspaceExecOutput(ctx, api.WorkerWorkspaceExecOutputRequest{
			WorkerWorkspacePrimitiveScope: scope,
			ExecID:                        payload.ExecStderrChunk.GetExecId(),
			Chunks: []api.WorkerWorkspaceExecOutputChunk{{
				Stream:      "stderr",
				OffsetStart: &offset,
				Data:        payload.ExecStderrChunk.GetData(),
			}},
		})
		if err == nil {
			offsetEnd := offset + int64(len(payload.ExecStderrChunk.GetData()))
			if len(response.Chunks) > 0 {
				offsetEnd = response.Chunks[len(response.Chunks)-1].OffsetEnd
			}
			state.advanceExecOutputOffset(payload.ExecStderrChunk.GetExecId(), "stderr", offsetEnd)
		}
		return err
	case *workspacev0.WorkspaceOperationEvent_ExecExited:
		exitCode := payload.ExecExited.GetExitCode()
		_, err := client.MarkWorkspaceExecExited(ctx, api.WorkerWorkspaceExecExitedRequest{
			WorkerWorkspacePrimitiveScope: scope,
			ExecID:                        payload.ExecExited.GetExecId(),
			State:                         "exited",
			ExitCode:                      &exitCode,
			Signal:                        payload.ExecExited.GetSignal(),
			Error:                         json.RawMessage(payload.ExecExited.GetErrorJson()),
		})
		return err
	case *workspacev0.WorkspaceOperationEvent_ExecError:
		_, err := client.MarkWorkspaceExecExited(ctx, api.WorkerWorkspaceExecExitedRequest{
			WorkerWorkspacePrimitiveScope: scope,
			ExecID:                        payload.ExecError.GetExecId(),
			State:                         "failed",
			Error:                         json.RawMessage(payload.ExecError.GetErrorJson()),
		})
		return err
	case *workspacev0.WorkspaceOperationEvent_PtyOpened:
		response, err := client.MarkWorkspacePtyOpened(ctx, api.WorkerWorkspacePtyOpenedRequest{
			WorkerWorkspacePrimitiveScope: scope,
			PtyID:                         payload.PtyOpened.GetPtyId(),
			ProcessID:                     payload.PtyOpened.GetProcessId(),
		})
		if err == nil {
			state.seedPtyOutputOffset(payload.PtyOpened.GetPtyId(), response.Pty.OutputCursor)
		}
		return err
	case *workspacev0.WorkspaceOperationEvent_PtyOutputChunk:
		offset := state.ptyOutputOffset(payload.PtyOutputChunk.GetPtyId())
		response, err := client.AppendWorkspacePtyOutput(ctx, api.WorkerWorkspacePtyOutputRequest{
			WorkerWorkspacePrimitiveScope: scope,
			PtyID:                         payload.PtyOutputChunk.GetPtyId(),
			Chunks: []api.WorkerWorkspacePtyOutputChunk{{
				OffsetStart: &offset,
				Data:        payload.PtyOutputChunk.GetData(),
			}},
		})
		if err == nil {
			offsetEnd := offset + int64(len(payload.PtyOutputChunk.GetData()))
			if len(response.Chunks) > 0 {
				offsetEnd = response.Chunks[len(response.Chunks)-1].OffsetEnd
			}
			state.advancePtyOutputOffset(payload.PtyOutputChunk.GetPtyId(), offsetEnd)
		}
		return err
	case *workspacev0.WorkspaceOperationEvent_PtyResizeApplied:
		_, err := client.MarkWorkspacePtyResizeApplied(ctx, api.WorkerWorkspacePtyResizeAppliedRequest{
			WorkerWorkspacePrimitiveScope: scope,
			PtyID:                         payload.PtyResizeApplied.GetPtyId(),
			Cols:                          int32(payload.PtyResizeApplied.GetCols()),
			Rows:                          int32(payload.PtyResizeApplied.GetRows()),
		})
		return err
	case *workspacev0.WorkspaceOperationEvent_PtyClosed:
		_, err := client.MarkWorkspacePtyClosed(ctx, api.WorkerWorkspacePtyClosedRequest{
			WorkerWorkspacePrimitiveScope: scope,
			PtyID:                         payload.PtyClosed.GetPtyId(),
			Reason:                        payload.PtyClosed.GetReason(),
			Error:                         json.RawMessage(payload.PtyClosed.GetErrorJson()),
		})
		return err
	case *workspacev0.WorkspaceOperationEvent_PtyError:
		_, err := client.MarkWorkspacePtyClosed(ctx, api.WorkerWorkspacePtyClosedRequest{
			WorkerWorkspacePrimitiveScope: scope,
			PtyID:                         payload.PtyError.GetPtyId(),
			Error:                         json.RawMessage(payload.PtyError.GetErrorJson()),
		})
		return err
	default:
		return errors.New("workspace operation event payload is required")
	}
}

func (m WorkspaceMaterializer) networkPolicy() compute.NetworkPolicy {
	if m.Network.Internet || len(m.Network.Allow) > 0 || len(m.Network.Deny) > 0 {
		return m.Network
	}
	return compute.DefaultNetworkPolicy()
}

func (m WorkspaceMaterializer) startupTimeout() time.Duration {
	if m.StartupTimeout > 0 {
		return m.StartupTimeout
	}
	return 20 * time.Minute
}

func (m WorkspaceMaterializer) failureTimeout() time.Duration {
	if m.FailureTimeout > 0 {
		return m.FailureTimeout
	}
	return 30 * time.Second
}

func (m WorkspaceMaterializer) closeSession(session vm.Session) error {
	ctx, cancel := context.WithTimeout(context.Background(), m.failureTimeout())
	defer cancel()
	return session.Close(ctx)
}

func (m WorkspaceMaterializer) channelToken(mount api.WorkerWorkspaceMount) string {
	token := strings.TrimSpace(mount.GuestdChannelToken)
	if token == "" {
		return ""
	}
	return token
}

func (m WorkspaceMaterializer) failWorkspaceMount(client api.WorkerWorkspaceMaterializerControlClient, mount api.WorkerWorkspaceMount, cause error) error {
	body := workspaceMountError(cause)
	ctx, cancel := context.WithTimeout(context.Background(), m.failureTimeout())
	defer cancel()
	_, err := client.FailWorkspaceMount(ctx, api.WorkerWorkspaceMountFailRequest{
		OrgID:                mount.OrgID,
		WorkspaceMountID:     mount.ID,
		RuntimeInstanceToken: mount.RuntimeInstanceToken,
		Error:                body,
	})
	return err
}

func workspaceMountError(err error) json.RawMessage {
	code := "workspace_mount_failed"
	var failure workspaceMountFailure
	if errors.As(err, &failure) && strings.TrimSpace(failure.code) != "" {
		code = strings.TrimSpace(failure.code)
	} else if errors.Is(err, context.DeadlineExceeded) {
		code = "workspace_mount_startup_timeout"
	}
	body, marshalErr := json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{
		Code:    code,
		Message: err.Error(),
	})
	if marshalErr != nil {
		return json.RawMessage(`{"code":"workspace_mount_failed"}`)
	}
	return body
}

func workspaceOperationDispatchError(err error) json.RawMessage {
	body, marshalErr := json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{
		Code:    "workspace_operation_dispatch_failed",
		Message: err.Error(),
	})
	if marshalErr != nil {
		return json.RawMessage(`{"code":"workspace_operation_dispatch_failed"}`)
	}
	return body
}

func startWorkspaceOperation(ctx context.Context, client api.WorkerWorkspaceMaterializerControlClient, request api.WorkerWorkspaceOperationStartRequest, backoff time.Duration) error {
	return retryWorkspaceOperation(ctx, backoff, func() error {
		_, err := client.StartWorkspaceOperation(ctx, request)
		return err
	})
}

func completeWorkspaceOperation(ctx context.Context, client api.WorkerWorkspaceMaterializerControlClient, request api.WorkerWorkspaceOperationCompleteRequest, backoff time.Duration) error {
	return retryWorkspaceOperation(ctx, backoff, func() error {
		_, err := client.CompleteWorkspaceOperation(ctx, request)
		return err
	})
}

func retryWorkspaceOperation(ctx context.Context, backoff time.Duration, fn func() error) error {
	const attempts = 3
	if backoff <= 0 {
		backoff = 250 * time.Millisecond
	}
	var lastErr error
	for attempt := range attempts {
		if err := fn(); err != nil {
			lastErr = err
		} else {
			return nil
		}
		if attempt == attempts-1 {
			break
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
}
