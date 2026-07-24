package executor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/localcache"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
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
	Capacity              *capacity.Ledger
	RuntimeScratchBytes   int64
}

func (m WorkspaceMaterializer) RunWorkspaceMount(ctx context.Context, mount api.WorkerWorkspaceMount, client api.WorkerWorkspaceMaterializerControlClient) (runErr error) {
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
	rawSession, workspaceImagePath, workspaceArtifactPath, cleanup, runtimeInstanceID, usePreparedRuntime, resourceKey, err := m.materializeSession(startupCtx, &mount)
	m.logWorkspaceMountPhase(mount, "workspace mount session materialized", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", errorString(err))
	if err != nil {
		cleanup()
		_ = m.failWorkspaceMount(client, mount, err)
		return fmt.Errorf("connect workspace mount guest: %w", err)
	}
	if usePreparedRuntime && m.RuntimePool != nil {
		mount.RestoreCheckpointID = m.RuntimePool.checkedOutRestoreCheckpoint(
			mount.RuntimeInstanceID, mount.RuntimeEpoch,
		)
	}
	renewal := m.startRenewalLoop(ctx, api.WorkerWorkspaceMountRenewRequest{
		OrgID: mount.OrgID, WorkspaceMountID: mount.ID,
	}, client, renewEvery)
	defer renewal.stopAndWait()
	session := newManagedWorkspaceMountSession(rawSession)
	defer cleanup()
	defer func() {
		if closeErr := m.closeSession(session); closeErr != nil {
			failure := workspaceMountFailure{
				code: "workspace_mount_runtime_close_failed",
				err:  fmt.Errorf("close workspace mount runtime: %w", closeErr),
			}
			m.logWorkspaceMountPhase(mount, "workspace mount session close failed", "error", closeErr.Error())
			_ = m.failWorkspaceMount(client, mount, failure)
			runErr = errors.Join(runErr, failure)
			return
		}
		if usePreparedRuntime && m.RuntimePool != nil {
			if releaseErr := m.RuntimePool.ReleaseCheckout(mount.RuntimeInstanceID, mount.RuntimeEpoch); releaseErr != nil {
				runErr = errors.Join(runErr, fmt.Errorf("release workspace mount runtime checkout: %w", releaseErr))
			}
			return
		}
		if m.Capacity != nil && resourceKey.ID != "" {
			if releaseErr := m.Capacity.Release(resourceKey); releaseErr != nil {
				m.logWorkspaceMountPhase(mount, "workspace mount capacity release failed", "error", releaseErr.Error())
				runErr = errors.Join(runErr, fmt.Errorf("release workspace mount capacity: %w", releaseErr))
			}
		}
	}()
	phaseStarted = time.Now()
	if err := m.registerWorkspaceMountContext(startupCtx, session, mount, workspaceImagePath, workspaceArtifactPath, runtimeInstanceID, usePreparedRuntime); err != nil {
		m.logWorkspaceMountPhase(mount, "workspace mount guest registered", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", err.Error())
		if renewalErr := renewal.stopAndWait(); renewalErr != nil {
			err = renewalErr
		}
		_ = m.failWorkspaceMount(client, mount, err)
		return err
	}
	m.logWorkspaceMountPhase(mount, "workspace mount guest registered", "duration_ms", time.Since(phaseStarted).Milliseconds())
	unregisterSession := func() {}
	if m.Sessions != nil {
		unregisterSession = m.Sessions.RegisterWorkspaceMountSession(mount, session, m.channelToken(mount))
	}
	defer func() { unregisterSession() }()
	phaseStarted = time.Now()
	mounted, err := client.MarkWorkspaceMountMounted(renewal.ctx, api.WorkerWorkspaceMountMountedRequest{
		OrgID: mount.OrgID, WorkspaceMountID: mount.ID,
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
		unregisterSession()
		unregisterSession = func() {}
		if err := m.stopControlledWorkspaceMount(renewal.ctx, session, mount, mounted, client); err != nil {
			return err
		}
		_ = renewal.stopAndWait()
		return nil
	}
	endForeground()
	foregroundActive = false
	m.logWorkspaceMountPhase(mount, "workspace mount ready", "duration_ms", time.Since(totalStarted).Milliseconds())
	return m.serveWorkspaceMount(ctx, renewal, session, mount, client)
}

func (m WorkspaceMaterializer) serveWorkspaceMount(
	ctx context.Context,
	renewal *workspaceMountRenewal,
	session *managedWorkspaceMountSession,
	mount api.WorkerWorkspaceMount,
	client api.WorkerWorkspaceMaterializerControlClient,
) error {
	sessionExited := make(chan error, 1)
	go func() {
		sessionExited <- session.Wait(renewal.ctx)
	}()
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
		case <-poll.C:
			claimed, err := client.ClaimWorkspaceExec(renewal.ctx, api.WorkerWorkspaceExecClaimRequest{
				OrgID: mount.OrgID, WorkspaceMountID: mount.ID,
			})
			if err != nil {
				poll.Reset(claimErrorBackoff)
				continue
			}
			if claimed.Exec == nil {
				poll.Reset(pollEvery)
				continue
			}
			completion, err := m.dispatchWorkspaceBasicExec(
				renewal.ctx,
				session,
				mount,
				*claimed.Exec,
			)
			if err != nil {
				var protocolError *workspaceBasicExecProtocolError
				if errors.As(err, &protocolError) {
					return failAndReturn(protocolError)
				}
				poll.Reset(claimErrorBackoff)
				continue
			}
			update, err := m.completeWorkspaceBasicExec(renewal.ctx, client, completion)
			if err != nil {
				return failAndReturn(fmt.Errorf("complete Workspace exec: %w", err))
			}
			if strings.TrimSpace(update.State) != "unmounting" {
				return failAndReturn(fmt.Errorf(
					"complete Workspace exec returned mount state %q",
					update.State,
				))
			}
			if err := m.stopControlledWorkspaceMount(
				renewal.ctx,
				session,
				mount,
				update,
				client,
			); err != nil {
				return err
			}
			_ = renewal.stopAndWait()
			return nil
		}
	}
}

type workspaceBasicExecProtocolError struct {
	err error
}

func (e *workspaceBasicExecProtocolError) Error() string {
	return e.err.Error()
}

func (e *workspaceBasicExecProtocolError) Unwrap() error {
	return e.err
}

func workspaceBasicExecProtocol(err error) error {
	return &workspaceBasicExecProtocolError{err: err}
}

func (m WorkspaceMaterializer) dispatchWorkspaceBasicExec(
	ctx context.Context,
	session vm.Session,
	mount api.WorkerWorkspaceMount,
	exec api.WorkerWorkspaceExec,
) (api.WorkerWorkspaceExecCompleteRequest, error) {
	defer func() {
		clear(exec.Stdin)
		for index := range exec.Secrets {
			clear(exec.Secrets[index].Value)
		}
	}()
	channelToken := m.channelToken(mount)
	if channelToken == "" {
		return api.WorkerWorkspaceExecCompleteRequest{}, workspaceBasicExecProtocol(
			errors.New("Workspace mount guest channel token is required"),
		)
	}
	if strings.TrimSpace(exec.ProcessID) == "" ||
		strings.TrimSpace(exec.RequestFingerprint) == "" ||
		strings.TrimSpace(exec.WorkspaceLeaseID) == "" ||
		strings.TrimSpace(exec.WriteCapability) == "" {
		return api.WorkerWorkspaceExecCompleteRequest{}, workspaceBasicExecProtocol(
			errors.New("Workspace exec claim is incomplete"),
		)
	}
	if strings.TrimSpace(exec.WorkspaceMountID) != strings.TrimSpace(mount.ID) ||
		strings.TrimSpace(exec.WorkspaceID) != strings.TrimSpace(mount.WorkspaceID) {
		return api.WorkerWorkspaceExecCompleteRequest{}, workspaceBasicExecProtocol(
			errors.New("Workspace exec claim does not match the live mount"),
		)
	}
	if exec.FencingGeneration <= 0 ||
		exec.OwnershipGeneration <= 0 ||
		exec.WriterGeneration <= 0 ||
		exec.ExpiresAt.IsZero() ||
		!exec.ExpiresAt.After(time.Now()) {
		return api.WorkerWorkspaceExecCompleteRequest{}, workspaceBasicExecProtocol(
			errors.New("Workspace exec claim fence is invalid or expired"),
		)
	}
	request := &workspacev0.WorkspaceBasicExecRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			OperationId:                strings.TrimSpace(exec.ProcessID),
			WorkspaceMountId:           strings.TrimSpace(exec.WorkspaceMountID),
			WorkspaceId:                strings.TrimSpace(exec.WorkspaceID),
			ChannelToken:               channelToken,
			FencingGeneration:          uint64(exec.FencingGeneration),
			InstanceLeaseId:            strings.TrimSpace(exec.WorkspaceLeaseID),
			WriteLeaseId:               strings.TrimSpace(exec.WorkspaceLeaseID),
			FencingToken:               strings.TrimSpace(exec.WriteCapability),
			OperationExpiresAtUnixNano: exec.ExpiresAt.UnixNano(),
			RequestFingerprint:         strings.TrimSpace(exec.RequestFingerprint),
		},
		RequestJson: string(exec.Request),
		Stdin:       exec.Stdin,
	}
	for _, delivery := range exec.Secrets {
		secret := &workspacev0.WorkspaceSecretDelivery{Value: delivery.Value}
		switch {
		case delivery.Env != nil && delivery.File == nil:
			secret.PlacementKind = "env"
			secret.PlacementTarget = strings.TrimSpace(delivery.Env.Name)
		case delivery.Env == nil && delivery.File != nil:
			secret.PlacementKind = "file"
			secret.PlacementTarget = strings.TrimSpace(delivery.File.Path)
		default:
			return api.WorkerWorkspaceExecCompleteRequest{}, workspaceBasicExecProtocol(
				errors.New("Workspace exec Secret placement is invalid"),
			)
		}
		if secret.PlacementTarget == "" {
			return api.WorkerWorkspaceExecCompleteRequest{}, workspaceBasicExecProtocol(
				errors.New("Workspace exec Secret placement target is required"),
			)
		}
		request.Secrets = append(request.Secrets, secret)
	}
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return api.WorkerWorkspaceExecCompleteRequest{}, fmt.Errorf("open Workspace exec stream: %w", err)
	}
	defer stream.Close()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:        wire.StreamTypeWorkspaceBasicExec,
		WorkspaceID: mount.WorkspaceID,
		OperationID: exec.ProcessID,
	}, 0); err != nil {
		return api.WorkerWorkspaceExecCompleteRequest{}, fmt.Errorf("write Workspace exec header: %w", err)
	}
	if err := frameio.WriteProtoFrame(stream, request); err != nil {
		return api.WorkerWorkspaceExecCompleteRequest{}, fmt.Errorf("write Workspace exec request: %w", err)
	}
	var result workspacev0.WorkspaceBasicExecResult
	if err := readProtoFrameFromReaderContext(ctx, session, stream, &result); err != nil {
		return api.WorkerWorkspaceExecCompleteRequest{}, fmt.Errorf("read Workspace exec result: %w", err)
	}
	if strings.TrimSpace(result.GetRequestFingerprint()) !=
		strings.TrimSpace(exec.RequestFingerprint) {
		return api.WorkerWorkspaceExecCompleteRequest{}, workspaceBasicExecProtocol(
			errors.New("Workspace exec result fingerprint does not match its claim"),
		)
	}
	outcome := strings.TrimSpace(result.GetOutcome())
	if err := validateWorkspaceBasicExecOutcome(outcome); err != nil {
		return api.WorkerWorkspaceExecCompleteRequest{}, err
	}
	completion := api.WorkerWorkspaceExecCompleteRequest{
		OrgID:               mount.OrgID,
		ProcessID:           exec.ProcessID,
		WorkspaceLeaseID:    exec.WorkspaceLeaseID,
		WriteCapability:     exec.WriteCapability,
		FencingGeneration:   exec.FencingGeneration,
		OwnershipGeneration: exec.OwnershipGeneration,
		WriterGeneration:    exec.WriterGeneration,
		RequestFingerprint:  exec.RequestFingerprint,
		Outcome:             outcome,
		Stdout:              result.GetStdout(),
		Stderr:              result.GetStderr(),
	}
	if outcome == "exited" {
		exitCode := result.GetExitCode()
		completion.ExitCode = &exitCode
		if strings.TrimSpace(result.GetErrorJson()) != "" {
			return api.WorkerWorkspaceExecCompleteRequest{}, workspaceBasicExecProtocol(
				errors.New("exited Workspace exec returned an error"),
			)
		}
		return completion, nil
	}
	if strings.TrimSpace(result.GetErrorJson()) == "" ||
		!json.Valid([]byte(result.GetErrorJson())) {
		return api.WorkerWorkspaceExecCompleteRequest{}, workspaceBasicExecProtocol(
			errors.New("failed Workspace exec returned invalid error JSON"),
		)
	}
	completion.Error = json.RawMessage(result.GetErrorJson())
	return completion, nil
}

func validateWorkspaceBasicExecOutcome(outcome string) error {
	switch outcome {
	case "exited",
		"workspace_exec_failed",
		"workspace_exec_timed_out",
		"workspace_exec_signaled",
		"workspace_exec_launch_failed",
		"workspace_exec_secret_delivery_failed",
		"workspace_exec_output_limit_exceeded",
		"workspace_exec_result_uncertain":
		return nil
	case "workspace_exec_fenced",
		"workspace_exec_expired",
		"workspace_exec_invalid",
		"workspace_exec_fingerprint_conflict",
		"workspace_exec_unavailable":
		return workspaceBasicExecProtocol(
			fmt.Errorf("Workspace exec guest rejected authority: %s", outcome),
		)
	default:
		return workspaceBasicExecProtocol(
			fmt.Errorf("Workspace exec result outcome %q is unsupported", outcome),
		)
	}
}

func (m WorkspaceMaterializer) completeWorkspaceBasicExec(
	ctx context.Context,
	client api.WorkerWorkspaceMaterializerControlClient,
	request api.WorkerWorkspaceExecCompleteRequest,
) (api.WorkspaceMountResponse, error) {
	backoff := m.CompleteErrorBackoff
	if backoff <= 0 {
		backoff = 250 * time.Millisecond
	}
	for {
		response, err := client.CompleteWorkspaceExec(ctx, request)
		if err == nil {
			return response, nil
		}
		if !workspaceExecCompletionRetryable(err) {
			return api.WorkspaceMountResponse{}, err
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return api.WorkspaceMountResponse{}, errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}

func workspaceExecCompletionRetryable(err error) bool {
	var statusError interface{ HTTPStatusCode() int }
	if !errors.As(err, &statusError) {
		return true
	}
	status := statusError.HTTPStatusCode()
	return status == http.StatusRequestTimeout ||
		status == http.StatusTooEarly ||
		status == http.StatusTooManyRequests ||
		status >= http.StatusInternalServerError
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

func (m WorkspaceMaterializer) materializeSession(ctx context.Context, mount *api.WorkerWorkspaceMount) (vm.Session, string, string, func(), string, bool, capacity.Key, error) {
	if mount == nil {
		return nil, "", "", func() {}, "", false, capacity.Key{}, workspaceMountFailure{code: "workspace_mount_missing", err: errors.New("workspace mount is required")}
	}
	if m.CAS == nil {
		return nil, "", "", func() {}, "", false, capacity.Key{}, workspaceMountFailure{code: "workspace_mount_cas_unconfigured", err: errors.New("workspace materializer CAS is required")}
	}
	connector, ok := m.Connector.(vm.MaterializingConnector)
	if !ok {
		return nil, "", "", func() {}, "", false, capacity.Key{}, workspaceMountFailure{code: "workspace_mount_connector_unsupported", err: errors.New("workspace materializer connector does not support artifact mount")}
	}
	tempDir := strings.TrimSpace(m.TempDir)
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return nil, "", "", func() {}, "", false, capacity.Key{}, workspaceMountFailure{code: "workspace_mount_temp_unavailable", err: fmt.Errorf("create mount temp dir: %w", err)}
	}
	if m.RuntimePool != nil {
		if session, key, ok := m.RuntimePool.Checkout(ctx, *mount); ok {
			workspaceArtifact := api.CASObject{
				Digest:    strings.TrimSpace(mount.WorkspaceArtifact.Digest),
				SizeBytes: mount.WorkspaceArtifact.SizeBytes,
				MediaType: strings.TrimSpace(mount.WorkspaceArtifact.MediaType),
			}
			workspacePath := ""
			cleanupWorkspace := func() {}
			if !workspaceArtifactIsEmpty(mount.WorkspaceArtifact) {
				phaseStarted := time.Now()
				var err error
				workspacePath, cleanupWorkspace, err = m.restoreCASObject(ctx, tempDir, "workspace-version", workspaceArtifact)
				m.logWorkspaceMountPhase(*mount, "workspace mount workspace artifact restored", "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", workspaceArtifact.SizeBytes, "error", errorString(err), "prepared_runtime_hit", true)
				if err != nil {
					if closeErr := m.closeSession(session); closeErr != nil {
						err = errors.Join(err, workspaceMountFailure{
							code: "workspace_mount_runtime_close_failed",
							err:  fmt.Errorf("close prepared workspace runtime: %w", closeErr),
						})
					} else if releaseErr := m.RuntimePool.ReleaseCheckout(mount.RuntimeInstanceID, mount.RuntimeEpoch); releaseErr != nil {
						err = errors.Join(err, fmt.Errorf("release prepared workspace runtime checkout: %w", releaseErr))
					}
					return nil, "", "", cleanupWorkspace, key, true, capacity.Key{}, err
				}
			}
			m.logWorkspaceMountPhase(*mount, "workspace mount prepared runtime checked out", "runtime_instance_id", key)
			return session, "", workspacePath, cleanupWorkspace, key, true, capacity.Key{}, nil
		}
	}
	mount.RuntimeInstanceID = strings.TrimSpace(mount.RuntimeInstanceID)
	if mount.RuntimeInstanceID == "" {
		return nil, "", "", func() {}, "", false, capacity.Key{}, workspaceMountFailure{code: "runtime_instance_missing", err: errors.New("workspace mount claim must include a runtime instance id")}
	}
	if mount.RuntimeEpoch <= 0 || strings.TrimSpace(mount.NetworkSlotID) == "" || mount.NetworkSlotGeneration <= 0 {
		return nil, "", "", func() {}, "", false, capacity.Key{}, workspaceMountFailure{code: "runtime_instance_fence_missing", err: errors.New("workspace mount claim must include runtime epoch and network slot generation")}
	}
	runtimeInstanceID := runtimeInstanceIDFromWorkspaceMount(*mount)
	m.logWorkspaceMountPhase(*mount, "workspace mount runtime instance claimed", "runtime_instance_id", runtimeInstanceID)
	workspaceArtifact := api.CASObject{
		Digest:    strings.TrimSpace(mount.WorkspaceArtifact.Digest),
		SizeBytes: mount.WorkspaceArtifact.SizeBytes,
		MediaType: strings.TrimSpace(mount.WorkspaceArtifact.MediaType),
	}
	if strings.TrimSpace(mount.BaseVersionID) == "" {
		return nil, "", "", func() {}, "", false, capacity.Key{}, workspaceMountFailure{code: "workspace_version_missing", err: errors.New("workspace mount base_version_id is required")}
	}
	if strings.TrimSpace(mount.WorkspaceMountPath) == "" {
		return nil, "", "", func() {}, "", false, capacity.Key{}, workspaceMountFailure{code: "workspace_mount_path_missing", err: errors.New("workspace mount mount path is required")}
	}
	if strings.TrimSpace(mount.WorkspaceArtifact.Encoding) != workspace.ArtifactEncoding {
		return nil, "", "", func() {}, "", false, capacity.Key{}, workspaceMountFailure{code: "workspace_version_artifact_incompatible", err: fmt.Errorf("workspace artifact encoding %q is not supported", mount.WorkspaceArtifact.Encoding)}
	}
	if err := validateWorkspaceArtifactShape(mount.WorkspaceArtifact); err != nil {
		return nil, "", "", func() {}, "", false, capacity.Key{}, workspaceMountFailure{code: "workspace_version_artifact_incompatible", err: err}
	}
	if m.Capacity == nil {
		return nil, "", "", func() {}, "", false, capacity.Key{}, workspaceMountFailure{code: "workspace_capacity_unavailable", err: errors.New("workspace materializer capacity ledger is required")}
	}
	resourceVector, err := runtimeCapacityVector(mount.RequestedMilliCPU, mount.RequestedMemoryMiB, mount.RequestedDiskMiB, m.RuntimeScratchBytes)
	if err != nil {
		return nil, "", "", func() {}, "", false, capacity.Key{}, workspaceMountFailure{code: "workspace_capacity_unavailable", err: err}
	}
	resourceKey := runtimeCapacityKey(mount.RuntimeInstanceID, mount.RuntimeEpoch)
	created, err := m.Capacity.Reserve(resourceKey, resourceVector)
	if err != nil {
		return nil, "", "", func() {}, "", false, capacity.Key{}, workspaceMountFailure{code: "workspace_capacity_unavailable", err: err}
	}
	if !created {
		return nil, "", "", func() {}, "", false, capacity.Key{}, workspaceMountFailure{code: "workspace_capacity_unavailable", err: errors.New("workspace runtime is already reserved locally")}
	}
	var (
		workspaceImagePath    string
		workspacePath         string
		session               vm.Session
		cleanupWorkspaceImage = func() {}
		cleanupWorkspace      = func() {}
		materializeAttempted  bool
	)
	releaseCapacity := func() error {
		return m.Capacity.Release(resourceKey)
	}
	cleanupMaterializeFailure := func() error {
		if session != nil {
			if closeErr := m.closeSession(session); closeErr != nil {
				return workspaceMountFailure{
					code: "workspace_mount_runtime_close_failed",
					err:  fmt.Errorf("close failed workspace runtime: %w", closeErr),
				}
			}
			return releaseCapacity()
		}
		if !materializeAttempted {
			return releaseCapacity()
		}
		if cleaner, ok := m.Connector.(vm.Cleaner); ok {
			cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), m.failureTimeout())
			err := cleaner.Cleanup(cleanupCtx, vm.Owner{Kind: vm.OwnerRuntime, ID: mount.RuntimeInstanceID})
			cancelCleanup()
			if err != nil {
				return workspaceMountFailure{
					code: "workspace_mount_runtime_cleanup_failed",
					err:  fmt.Errorf("cleanup failed workspace runtime: %w", err),
				}
			}
			return releaseCapacity()
		}
		return workspaceMountFailure{
			code: "workspace_mount_runtime_cleanup_failed",
			err:  errors.New("workspace runtime cleanup connector is required after materialization"),
		}
	}
	cleanup := func() {
		cleanupWorkspace()
		cleanupWorkspaceImage()
	}
	var group errgroup.Group
	group.Go(func() error {
		phaseStarted := time.Now()
		path, cleanupFn, err := m.restoreCASObject(ctx, tempDir, "workspace-image", mount.WorkspaceImage)
		cleanupWorkspaceImage = cleanupFn
		workspaceImagePath = path
		m.logWorkspaceMountPhase(*mount, "workspace image restored", "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", mount.WorkspaceImage.SizeBytes, "error", errorString(err))
		return err
	})
	if !workspaceArtifactIsEmpty(mount.WorkspaceArtifact) && strings.TrimSpace(mount.RestoreCheckpointID) == "" {
		group.Go(func() error {
			phaseStarted := time.Now()
			path, cleanupFn, err := m.restoreCASObject(ctx, tempDir, "workspace-version", workspaceArtifact)
			cleanupWorkspace = cleanupFn
			workspacePath = path
			m.logWorkspaceMountPhase(*mount, "workspace mount workspace artifact restored", "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", workspaceArtifact.SizeBytes, "error", errorString(err))
			return err
		})
	}
	if m.Substrates == nil {
		group.Go(func() error {
			phaseStarted := time.Now()
			materializeAttempted = true
			materialized, err := connector.Materialize(ctx, vm.MaterializeRequest{
				ID:                 mount.RuntimeInstanceID,
				OwnerKind:          vm.OwnerRuntime,
				RootfsDigest:       mount.RootfsDigest,
				WorkspaceMountPath: mount.WorkspaceMountPath,
				BaseVersionID:      mount.BaseVersionID,
				Resources: compute.ResourceVector{
					MilliCPU:  mount.RequestedMilliCPU,
					MemoryMiB: mount.RequestedMemoryMiB,
					DiskMiB:   mount.RequestedDiskMiB,
					Slots:     mount.RequestedExecutionSlots,
				},
				Network: mount.Network,
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
		err = errors.Join(err, cleanupMaterializeFailure())
		cleanup()
		return nil, "", "", func() {}, "", false, capacity.Key{}, err
	}
	if m.Substrates != nil {
		phaseStarted := time.Now()
		topology, err := runtimeSubstrateTopology(ctx, m.Substrates, workspaceImagePath, *mount)
		m.logWorkspaceMountPhase(*mount, "workspace mount substrate resolved", "duration_ms", time.Since(phaseStarted).Milliseconds(), "substrate_digest", runtimeSubstrateDigest(topology), "error", errorString(err))
		if err != nil {
			err = errors.Join(err, releaseCapacity())
			cleanup()
			return nil, "", "", func() {}, "", false, capacity.Key{}, workspaceMountFailure{code: "workspace_sandbox_substrate_unavailable", err: err}
		}
		phaseStarted = time.Now()
		materializeAttempted = true
		materialized, err := connector.Materialize(ctx, vm.MaterializeRequest{
			ID:                 mount.RuntimeInstanceID,
			OwnerKind:          vm.OwnerRuntime,
			RootfsDigest:       mount.RootfsDigest,
			WorkspaceMountPath: mount.WorkspaceMountPath,
			BaseVersionID:      mount.BaseVersionID,
			Resources: compute.ResourceVector{
				MilliCPU:  mount.RequestedMilliCPU,
				MemoryMiB: mount.RequestedMemoryMiB,
				DiskMiB:   mount.RequestedDiskMiB,
				Slots:     mount.RequestedExecutionSlots,
			},
			Network:  mount.Network,
			Topology: topology,
		})
		session = materialized
		m.logWorkspaceMountPhase(*mount, "workspace mount connector materialized", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", errorString(err))
		if err != nil {
			err = errors.Join(err, cleanupMaterializeFailure())
			cleanup()
			return nil, "", "", func() {}, "", false, capacity.Key{}, workspaceMountFailure{code: "workspace_sandbox_abi_incompatible", err: err}
		}
	}
	if session == nil {
		cleanupErr := cleanupMaterializeFailure()
		cleanup()
		return nil, "", "", func() {}, "", false, capacity.Key{}, workspaceMountFailure{
			code: "workspace_sandbox_abi_incompatible",
			err:  errors.Join(errors.New("workspace mount connector returned nil session"), cleanupErr),
		}
	}
	return session, workspaceImagePath, workspacePath, cleanup, runtimeInstanceID, false, resourceKey, nil
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

func workspaceArtifactIsEmpty(artifact api.WorkerWorkspaceArtifact) bool {
	return strings.TrimSpace(artifact.Digest) == "" && artifact.SizeBytes == 0 && artifact.EntryCount == 0
}

func validateWorkspaceArtifactShape(artifact api.WorkerWorkspaceArtifact) error {
	if workspaceArtifactIsEmpty(artifact) {
		return nil
	}
	if strings.TrimSpace(artifact.Digest) == "" || artifact.SizeBytes <= 0 || artifact.EntryCount < 0 {
		return errors.New("workspace artifact must be the canonical empty root or a complete artifact")
	}
	return nil
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

func (m WorkspaceMaterializer) registerWorkspaceMount(ctx context.Context, session vm.Session, mount api.WorkerWorkspaceMount, workspaceImagePath string, workspaceArtifactPath string, runtimeInstanceID string, usePreparedRuntime bool) error {
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
		WorkspaceImage: &workspacev0.WorkspaceArtifact{
			Digest:    strings.TrimSpace(mount.WorkspaceImage.Digest),
			MediaType: strings.TrimSpace(mount.WorkspaceImage.MediaType),
			Encoding:  "oci-tar",
			SizeBytes: uint64(mount.WorkspaceImage.SizeBytes),
		},
		UsePreparedRuntime:   usePreparedRuntime,
		RuntimeInstanceId:    strings.TrimSpace(runtimeInstanceID),
		RestoredCheckpointId: strings.TrimSpace(mount.RestoreCheckpointID),
	}
	if !workspaceArtifactIsEmpty(mount.WorkspaceArtifact) && strings.TrimSpace(mount.RestoreCheckpointID) == "" {
		request.BaseArtifact = &workspacev0.WorkspaceArtifact{
			Digest:     strings.TrimSpace(mount.WorkspaceArtifact.Digest),
			MediaType:  strings.TrimSpace(mount.WorkspaceArtifact.MediaType),
			Encoding:   strings.TrimSpace(mount.WorkspaceArtifact.Encoding),
			SizeBytes:  uint64(mount.WorkspaceArtifact.SizeBytes),
			EntryCount: uint32(mount.WorkspaceArtifact.EntryCount),
		}
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
		}, workspaceImagePath, strings.TrimSpace(mount.WorkspaceImage.Digest), mount.WorkspaceImage.SizeBytes); err != nil {
			m.logWorkspaceMountPhase(mount, "workspace image sent", "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", mount.WorkspaceImage.SizeBytes, "error", err.Error())
			return fmt.Errorf("write workspace image: %w", err)
		}
		m.logWorkspaceMountPhase(mount, "workspace image sent", "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", mount.WorkspaceImage.SizeBytes)
	} else {
		m.logWorkspaceMountPhase(mount, "workspace image transfer skipped", "prepared_runtime_hit", true, "runtime_instance_id", runtimeInstanceID, "size_bytes", mount.WorkspaceImage.SizeBytes)
	}
	if !workspaceArtifactIsEmpty(mount.WorkspaceArtifact) && strings.TrimSpace(mount.RestoreCheckpointID) == "" {
		phaseStarted = time.Now()
		if err := wire.WriteFileFrameWithMetadata(stream, wire.StreamHeader{
			Type:        wire.StreamTypeWorkspaceArtifact,
			WorkspaceID: mount.WorkspaceID,
		}, workspaceArtifactPath, strings.TrimSpace(mount.WorkspaceArtifact.Digest), mount.WorkspaceArtifact.SizeBytes); err != nil {
			m.logWorkspaceMountPhase(mount, "workspace mount workspace artifact sent", "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", mount.WorkspaceArtifact.SizeBytes, "error", err.Error())
			return fmt.Errorf("write workspace artifact: %w", err)
		}
		m.logWorkspaceMountPhase(mount, "workspace mount workspace artifact sent", "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", mount.WorkspaceArtifact.SizeBytes)
	}
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

func (m WorkspaceMaterializer) registerWorkspaceMountContext(ctx context.Context, session vm.Session, mount api.WorkerWorkspaceMount, workspaceImagePath string, workspaceArtifactPath string, runtimeInstanceID string, usePreparedRuntime bool) error {
	result := make(chan error, 1)
	go func() {
		result <- m.registerWorkspaceMount(ctx, session, mount, workspaceImagePath, workspaceArtifactPath, runtimeInstanceID, usePreparedRuntime)
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
	if strings.TrimSpace(update.State) != "unmounting" {
		return fmt.Errorf("Workspace mount stop requires unmounting state, got %q", update.State)
	}
	var capture bool
	switch strings.TrimSpace(update.FinalizationKind) {
	case "capture":
		capture = true
	case "discard":
		capture = false
	case "":
		// Run-owned mounts still carry their capture decision through the Run
		// finalization contract. Process-owned BasicExec mounts always use the
		// explicit capture/discard marker.
		capture = update.DirtyGeneration > 0
	default:
		return fmt.Errorf("Workspace mount finalization kind %q is unsupported", update.FinalizationKind)
	}
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
			OrgID:              mount.OrgID,
			ProjectID:          mount.ProjectID,
			EnvironmentID:      mount.EnvironmentID,
			WorkspaceID:        mount.WorkspaceID,
			WorkspaceMountID:   mount.ID,
			ArtifactDigest:     artifact.Digest,
			ArtifactSizeBytes:  artifact.SizeBytes,
			ArtifactMediaType:  artifact.MediaType,
			ArtifactEncoding:   artifact.Encoding,
			ArtifactEntryCount: int32(artifact.EntryCount),
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
	if err := m.closeSession(session); err != nil {
		_ = m.failWorkspaceMount(client, mount, workspaceMountFailure{
			code: "workspace_mount_runtime_close_failed",
			err:  fmt.Errorf("close workspace runtime: %w", err),
		})
		return fmt.Errorf("close workspace runtime: %w", err)
	}
	if _, err := client.StopWorkspaceMount(context.Background(), api.WorkerWorkspaceMountStopRequest{
		OrgID: mount.OrgID, WorkspaceMountID: mount.ID,
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
		OrgID: mount.OrgID, WorkspaceMountID: mount.ID, Error: body,
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
