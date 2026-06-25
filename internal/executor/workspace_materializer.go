package executor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/transport"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

type WorkspaceMaterializer struct {
	Connector            vm.Connector
	CAS                  cas.Store
	Sessions             WorkspaceMaterializationSessionRegistry
	TempDir              string
	Heartbeat            time.Duration
	StartupTimeout       time.Duration
	FailureTimeout       time.Duration
	PollEvery            time.Duration
	ClaimErrorBackoff    time.Duration
	CompleteErrorBackoff time.Duration
	Network              compute.NetworkPolicy
}

func (m WorkspaceMaterializer) RunWorkspaceMaterialization(ctx context.Context, materialization api.WorkerWorkspaceMaterialization, client api.WorkerWorkspaceMaterializerControlClient) error {
	if m.Connector == nil {
		return errors.New("workspace materializer connector is required")
	}
	renewEvery := m.Heartbeat
	if renewEvery <= 0 {
		renewEvery = 15 * time.Second
	}
	renewal := m.startRenewalLoop(ctx, api.WorkerWorkspaceMaterializationRenewRequest{
		OrgID:             materialization.OrgID,
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
	}, client, renewEvery)
	defer renewal.stopAndWait()
	startupCtx, cancelStartup := context.WithTimeout(renewal.ctx, m.startupTimeout())
	defer cancelStartup()
	rawSession, sandboxImagePath, workspaceArtifactPath, cleanup, err := m.materializeSession(startupCtx, materialization)
	if err != nil {
		cleanup()
		if renewalErr := renewal.stopAndWait(); renewalErr != nil {
			err = renewalErr
		}
		_ = m.failMaterialization(client, materialization, err)
		return fmt.Errorf("connect workspace materialization guest: %w", err)
	}
	session := newManagedWorkspaceMaterializationSession(rawSession)
	defer cleanup()
	defer func() { _ = m.closeSession(session) }()
	if err := m.registerMaterializationContext(startupCtx, session, materialization, sandboxImagePath, workspaceArtifactPath); err != nil {
		if renewalErr := renewal.stopAndWait(); renewalErr != nil {
			err = renewalErr
		}
		_ = m.failMaterialization(client, materialization, err)
		return err
	}
	running, err := client.MarkWorkspaceMaterializationRunning(renewal.ctx, api.WorkerWorkspaceMaterializationRunningRequest{
		OrgID:             materialization.OrgID,
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
	})
	if err != nil {
		if renewalErr := renewal.stopAndWait(); renewalErr != nil {
			err = renewalErr
		}
		_ = m.failMaterialization(client, materialization, err)
		return fmt.Errorf("mark workspace materialization running: %w", err)
	}
	switch strings.TrimSpace(running.State) {
	case "capturing", "stopping":
		if err := m.stopControlledWorkspaceMaterialization(renewal.ctx, session, materialization, running, client); err != nil {
			return err
		}
		_ = renewal.stopAndWait()
		return nil
	}
	unregisterSession := func() {}
	if m.Sessions != nil {
		unregisterSession = m.Sessions.RegisterWorkspaceMaterializationSession(materialization.ID, session)
	}
	defer unregisterSession()
	sessionExited := make(chan error, 1)
	go func() {
		sessionExited <- session.Wait(renewal.ctx)
	}()
	eventStream, err := m.openWorkspaceEventStream(renewal.ctx, session, materialization)
	if err != nil {
		if renewalErr := renewal.stopAndWait(); renewalErr != nil {
			err = renewalErr
		}
		_ = m.failMaterialization(client, materialization, err)
		return fmt.Errorf("open workspace event stream: %w", err)
	}
	eventLoopExited := make(chan error, 1)
	go func() {
		eventLoopExited <- m.runWorkspaceEventLoop(renewal.ctx, session, eventStream, materialization, client)
	}()
	inputRelays := newWorkspaceInputRelayRegistry()
	inputRelayExited := make(chan error, 1)
	failAndReturn := func(cause error) error {
		if ctx.Err() == nil {
			_ = m.failMaterialization(client, materialization, cause)
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
			case "capturing", "stopping":
				if err := m.stopControlledWorkspaceMaterialization(renewal.ctx, session, materialization, update, client); err != nil {
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
					return failAndReturn(materializationFailure{
						code: "workspace_materialization_checkpoint_release_failed",
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
				err = errors.New("workspace materialization session exited")
			}
			return failAndReturn(materializationFailure{
				code: "workspace_materialization_vm_exited",
				err:  fmt.Errorf("workspace materialization VM exited: %w", err),
			})
		case err := <-eventLoopExited:
			eventLoopExited = nil
			if released, releaseErr := session.CheckpointReleaseResult(context.Background()); released {
				_ = renewal.stopAndWait()
				if releaseErr != nil {
					return failAndReturn(materializationFailure{
						code: "workspace_materialization_checkpoint_release_failed",
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
				err = errors.New("workspace materialization event stream exited")
			}
			return failAndReturn(materializationFailure{
				code: "workspace_materialization_event_stream_lost",
				err:  fmt.Errorf("workspace materialization event stream exited: %w", err),
			})
		case err := <-inputRelayExited:
			if released, releaseErr := session.CheckpointReleaseResult(context.Background()); released {
				_ = renewal.stopAndWait()
				if releaseErr != nil {
					return failAndReturn(materializationFailure{
						code: "workspace_materialization_checkpoint_release_failed",
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
			claimed, err := client.ClaimWorkspaceMaterializationOperation(renewal.ctx, api.WorkerWorkspaceOperationClaimRequest{
				OrgID:             materialization.OrgID,
				MaterializationID: materialization.ID,
				ReservationToken:  materialization.ReservationToken,
			})
			if err != nil {
				poll.Reset(claimErrorBackoff)
				continue
			}
			if claimed.Operation == nil {
				poll.Reset(pollEvery)
				continue
			}
			if err := startWorkspaceMaterializationOperation(renewal.ctx, client, api.WorkerWorkspaceOperationStartRequest{
				OrgID:       claimed.Operation.OrgID,
				OperationID: claimed.Operation.ID,
				ClaimToken:  claimed.Operation.ClaimToken,
			}, completeErrorBackoff); err != nil {
				poll.Reset(completeErrorBackoff)
				continue
			}
			complete, err := m.dispatchOperation(renewal.ctx, session, materialization, *claimed.Operation)
			if err != nil {
				complete = api.WorkerWorkspaceOperationCompleteRequest{
					OrgID:       claimed.Operation.OrgID,
					OperationID: claimed.Operation.ID,
					ClaimToken:  claimed.Operation.ClaimToken,
					Error:       workspaceOperationDispatchError(err),
				}
			}
			if err := completeWorkspaceMaterializationOperation(renewal.ctx, client, complete, completeErrorBackoff); err != nil {
				return failAndReturn(fmt.Errorf("complete workspace operation: %w", err))
			}
			if err == nil && len(complete.Error) == 0 {
				m.startWorkspaceInputRelay(renewal.ctx, session, materialization, *claimed.Operation, client, inputRelayExited, inputRelays)
			}
			poll.Reset(pollEvery)
		}
	}
}

type materializationRenewal struct {
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan error
	updates chan api.WorkspaceMaterializationResponse
	once    sync.Once
	err     error
}

func (r *materializationRenewal) stopAndWait() error {
	r.once.Do(func() {
		r.cancel()
		r.err = <-r.done
	})
	return r.err
}

func (m WorkspaceMaterializer) startRenewalLoop(ctx context.Context, request api.WorkerWorkspaceMaterializationRenewRequest, client api.WorkerWorkspaceMaterializerControlClient, every time.Duration) *materializationRenewal {
	renewCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	updates := make(chan api.WorkspaceMaterializationResponse, 1)
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
				response, renewErr := client.RenewWorkspaceMaterialization(renewCtx, request)
				if renewErr != nil {
					err = fmt.Errorf("renew workspace materialization: %w", renewErr)
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
	return &materializationRenewal{ctx: renewCtx, cancel: cancel, done: done, updates: updates}
}

type materializationFailure struct {
	code string
	err  error
}

func (e materializationFailure) Error() string {
	if e.err == nil {
		return e.code
	}
	return e.err.Error()
}

func (e materializationFailure) Unwrap() error {
	return e.err
}

func (m WorkspaceMaterializer) materializeSession(ctx context.Context, materialization api.WorkerWorkspaceMaterialization) (vm.Session, string, string, func(), error) {
	if m.CAS == nil {
		return nil, "", "", func() {}, materializationFailure{code: "workspace_materialization_cas_unconfigured", err: errors.New("workspace materializer CAS is required")}
	}
	connector, ok := m.Connector.(vm.MaterializingConnector)
	if !ok {
		return nil, "", "", func() {}, materializationFailure{code: "workspace_materialization_connector_unsupported", err: errors.New("workspace materializer connector does not support artifact materialization")}
	}
	tempDir := strings.TrimSpace(m.TempDir)
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return nil, "", "", func() {}, materializationFailure{code: "workspace_materialization_temp_unavailable", err: fmt.Errorf("create materialization temp dir: %w", err)}
	}
	sandboxImagePath, cleanupSandboxImage, err := m.restoreCASObject(ctx, tempDir, "sandbox-image", materialization.SandboxImageArtifact)
	if err != nil {
		return nil, "", "", cleanupSandboxImage, err
	}
	workspaceArtifact := api.CASObject{
		Digest:    strings.TrimSpace(materialization.WorkspaceArtifact.Digest),
		SizeBytes: materialization.WorkspaceArtifact.SizeBytes,
		MediaType: strings.TrimSpace(materialization.WorkspaceArtifact.MediaType),
	}
	workspacePath, cleanupWorkspace, err := m.restoreCASObject(ctx, tempDir, "workspace-version", workspaceArtifact)
	cleanup := func() {
		cleanupWorkspace()
		cleanupSandboxImage()
	}
	if err != nil {
		return nil, "", "", cleanup, err
	}
	if strings.TrimSpace(materialization.BaseVersionID) == "" {
		cleanup()
		return nil, "", "", func() {}, materializationFailure{code: "workspace_version_missing", err: errors.New("workspace materialization base_version_id is required")}
	}
	if strings.TrimSpace(materialization.WorkspaceMountPath) == "" {
		cleanup()
		return nil, "", "", func() {}, materializationFailure{code: "workspace_mount_path_missing", err: errors.New("workspace materialization mount path is required")}
	}
	if strings.TrimSpace(materialization.WorkspaceArtifact.Encoding) != workspace.ArtifactEncoding {
		cleanup()
		return nil, "", "", func() {}, materializationFailure{code: "workspace_version_artifact_incompatible", err: fmt.Errorf("workspace artifact encoding %q is not supported", materialization.WorkspaceArtifact.Encoding)}
	}
	session, err := connector.Materialize(ctx, vm.MaterializeRequest{
		ID:                         materialization.ID,
		RootfsDigest:               materialization.RootfsDigest,
		ImageDigest:                materialization.ImageDigest,
		ImageFormat:                materialization.ImageFormat,
		WorkspaceArtifactPath:      workspacePath,
		WorkspaceArtifactDigest:    materialization.WorkspaceArtifact.Digest,
		WorkspaceArtifactMediaType: materialization.WorkspaceArtifact.MediaType,
		WorkspaceArtifactEncoding:  materialization.WorkspaceArtifact.Encoding,
		WorkspaceMountPath:         materialization.WorkspaceMountPath,
		BaseVersionID:              materialization.BaseVersionID,
		Network:                    m.networkPolicy(),
	})
	if err != nil {
		cleanup()
		return nil, "", "", func() {}, materializationFailure{code: "workspace_sandbox_abi_incompatible", err: err}
	}
	return session, sandboxImagePath, workspacePath, cleanup, nil
}

func (m WorkspaceMaterializer) restoreCASObject(ctx context.Context, tempDir string, label string, artifact api.CASObject) (string, func(), error) {
	cleanup := func() {}
	codeLabel := strings.ReplaceAll(label, "-", "_")
	digest := strings.TrimSpace(artifact.Digest)
	if digest == "" {
		return "", cleanup, materializationFailure{code: codeLabel + "_artifact_missing", err: errors.New(label + " artifact digest is required")}
	}
	if artifact.SizeBytes <= 0 {
		return "", cleanup, materializationFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("%s artifact size_bytes must be positive", label)}
	}
	mediaType := strings.TrimSpace(artifact.MediaType)
	if mediaType == "" {
		return "", cleanup, materializationFailure{code: codeLabel + "_artifact_missing", err: fmt.Errorf("%s artifact media_type is required", label)}
	}
	stat, err := m.CAS.Stat(ctx, digest)
	if err != nil {
		return "", cleanup, materializationFailure{code: codeLabel + "_artifact_missing", err: fmt.Errorf("stat %s artifact: %w", label, err)}
	}
	if stat.SizeBytes != artifact.SizeBytes || strings.TrimSpace(stat.MediaType) != mediaType {
		return "", cleanup, materializationFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("%s artifact metadata mismatch", label)}
	}
	reader, err := m.CAS.Get(ctx, digest)
	if err != nil {
		return "", cleanup, materializationFailure{code: codeLabel + "_artifact_missing", err: fmt.Errorf("get %s artifact: %w", label, err)}
	}
	defer reader.Close()
	file, err := os.CreateTemp(tempDir, label+"-*")
	if err != nil {
		return "", cleanup, materializationFailure{code: "workspace_materialization_temp_unavailable", err: fmt.Errorf("create %s artifact temp file: %w", label, err)}
	}
	path := file.Name()
	cleanup = func() { _ = os.Remove(path) }
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), reader)
	closeErr := file.Close()
	if copyErr != nil {
		cleanup()
		return "", func() {}, materializationFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("copy %s artifact: %w", label, copyErr)}
	}
	if closeErr != nil {
		cleanup()
		return "", func() {}, materializationFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("close %s artifact: %w", label, closeErr)}
	}
	if written != artifact.SizeBytes {
		cleanup()
		return "", func() {}, materializationFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("%s artifact size mismatch", label)}
	}
	if sha256sum.DigestHash(hash) != digest {
		cleanup()
		return "", func() {}, materializationFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("%s artifact digest mismatch", label)}
	}
	return path, cleanup, nil
}

func (m WorkspaceMaterializer) registerMaterialization(ctx context.Context, session vm.Session, materialization api.WorkerWorkspaceMaterialization, sandboxImagePath string, workspaceArtifactPath string) error {
	channelToken := m.channelToken(materialization)
	if channelToken == "" {
		return errors.New("workspace materialization guest channel token is required")
	}
	if strings.TrimSpace(materialization.GuestdChannelTokenHash) == "" {
		return errors.New("workspace materialization guest channel token hash is required")
	}
	stream := session.Stream()
	if err := transport.WriteStreamFrameHeader(stream, transport.StreamHeader{
		Type:        transport.StreamTypeWorkspaceMaterialize,
		WorkspaceID: materialization.WorkspaceID,
	}, 0); err != nil {
		return fmt.Errorf("write workspace materialize header: %w", err)
	}
	request := &workspacev0.MaterializeWorkspaceRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			MaterializationId: materialization.ID,
			WorkspaceId:       materialization.WorkspaceID,
			ChannelToken:      channelToken,
			FencingGeneration: uint64(materialization.FencingGeneration),
		},
		MountPath:     strings.TrimSpace(materialization.WorkspaceMountPath),
		BaseVersionId: strings.TrimSpace(materialization.BaseVersionID),
		BaseArtifact: &workspacev0.WorkspaceArtifact{
			Digest:     strings.TrimSpace(materialization.WorkspaceArtifact.Digest),
			MediaType:  strings.TrimSpace(materialization.WorkspaceArtifact.MediaType),
			Encoding:   strings.TrimSpace(materialization.WorkspaceArtifact.Encoding),
			SizeBytes:  uint64(materialization.WorkspaceArtifact.SizeBytes),
			EntryCount: uint32(materialization.WorkspaceArtifact.EntryCount),
		},
		SandboxArtifact: &workspacev0.WorkspaceArtifact{
			Digest:    strings.TrimSpace(materialization.SandboxImageArtifact.Digest),
			MediaType: strings.TrimSpace(materialization.SandboxImageArtifact.MediaType),
			Encoding:  strings.TrimSpace(materialization.SandboxImageArtifactFormat),
			SizeBytes: uint64(materialization.SandboxImageArtifact.SizeBytes),
		},
	}
	if err := transport.WriteProtoFrame(stream, request); err != nil {
		return fmt.Errorf("write workspace materialize request: %w", err)
	}
	if err := transport.WriteFileFrameWithMetadata(stream, transport.StreamHeader{
		Type:        transport.StreamTypeRunImage,
		WorkspaceID: materialization.WorkspaceID,
	}, sandboxImagePath, strings.TrimSpace(materialization.SandboxImageArtifact.Digest), materialization.SandboxImageArtifact.SizeBytes); err != nil {
		return fmt.Errorf("write sandbox image artifact: %w", err)
	}
	if err := transport.WriteFileFrameWithMetadata(stream, transport.StreamHeader{
		Type:        transport.StreamTypeWorkspaceArtifact,
		WorkspaceID: materialization.WorkspaceID,
	}, workspaceArtifactPath, strings.TrimSpace(materialization.WorkspaceArtifact.Digest), materialization.WorkspaceArtifact.SizeBytes); err != nil {
		return fmt.Errorf("write workspace artifact: %w", err)
	}
	var response workspacev0.MaterializeWorkspaceResponse
	if err := readProtoFrameFromReaderContext(ctx, session, stream, &response); err != nil {
		return fmt.Errorf("read workspace materialize response: %w", err)
	}
	if response.State != "running" {
		return fmt.Errorf("workspace materialize returned state %q", response.State)
	}
	expectedHash := strings.TrimSpace(materialization.GuestdChannelTokenHash)
	if strings.TrimSpace(response.GuestdChannelTokenHash) != expectedHash {
		return errors.New("workspace materialize guest channel token hash mismatch")
	}
	return nil
}

func (m WorkspaceMaterializer) registerMaterializationContext(ctx context.Context, session vm.Session, materialization api.WorkerWorkspaceMaterialization, sandboxImagePath string, workspaceArtifactPath string) error {
	result := make(chan error, 1)
	go func() {
		result <- m.registerMaterialization(ctx, session, materialization, sandboxImagePath, workspaceArtifactPath)
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = m.closeSession(session)
		return ctx.Err()
	}
}

func (m WorkspaceMaterializer) stopControlledWorkspaceMaterialization(ctx context.Context, session vm.Session, materialization api.WorkerWorkspaceMaterialization, update api.WorkspaceMaterializationResponse, client api.WorkerWorkspaceMaterializerControlClient) error {
	capture := strings.TrimSpace(update.State) == "capturing" && update.DirtyGeneration > 0
	fencingGeneration := materialization.FencingGeneration
	if update.FencingGeneration > fencingGeneration {
		fencingGeneration = update.FencingGeneration
	}
	artifact, err := m.stopWorkspaceGuest(ctx, session, materialization, fencingGeneration, capture, !capture)
	if err != nil {
		if capture {
			_ = m.failMaterialization(client, materialization, materializationFailure{
				code: "workspace_materialization_recovery_required",
				err:  fmt.Errorf("capture workspace before stop: %w", err),
			})
		} else {
			_ = m.failMaterialization(client, materialization, materializationFailure{
				code: "workspace_materialization_stop_failed",
				err:  fmt.Errorf("stop workspace guest: %w", err),
			})
		}
		return err
	}
	if capture {
		if _, err := client.CaptureWorkspaceMaterialization(ctx, api.WorkerWorkspaceMaterializationCaptureRequest{
			OrgID:              materialization.OrgID,
			ProjectID:          materialization.ProjectID,
			EnvironmentID:      materialization.EnvironmentID,
			WorkspaceID:        materialization.WorkspaceID,
			MaterializationID:  materialization.ID,
			ReservationToken:   materialization.ReservationToken,
			ArtifactDigest:     artifact.Digest,
			ArtifactSizeBytes:  artifact.SizeBytes,
			ArtifactMediaType:  artifact.MediaType,
			ArtifactEncoding:   artifact.Encoding,
			ArtifactEntryCount: int32(artifact.EntryCount),
		}); err != nil {
			_ = m.failMaterialization(client, materialization, materializationFailure{
				code: "workspace_materialization_recovery_required",
				err:  fmt.Errorf("promote workspace stop capture: %w", err),
			})
			return err
		}
	}
	if capture {
		if _, err := m.stopWorkspaceGuest(ctx, session, materialization, fencingGeneration, false, true); err != nil {
			_ = m.failMaterialization(client, materialization, materializationFailure{
				code: "workspace_materialization_stop_failed",
				err:  fmt.Errorf("finalize workspace stop: %w", err),
			})
			return fmt.Errorf("finalize workspace stop: %w", err)
		}
	}
	if _, err := client.StopWorkspaceMaterialization(context.Background(), api.WorkerWorkspaceMaterializationStopRequest{
		OrgID:             materialization.OrgID,
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
	}); err != nil {
		return fmt.Errorf("stop workspace materialization: %w", err)
	}
	return nil
}

func (m WorkspaceMaterializer) stopWorkspaceGuest(ctx context.Context, session vm.Session, materialization api.WorkerWorkspaceMaterialization, fencingGeneration int64, capture bool, finalize bool) (workspace.WorkspaceArtifact, error) {
	channelToken := m.channelToken(materialization)
	if channelToken == "" {
		return workspace.WorkspaceArtifact{}, errors.New("workspace materialization guest channel token is required")
	}
	if m.CAS == nil {
		return workspace.WorkspaceArtifact{}, errors.New("workspace materializer CAS is required")
	}
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("open workspace stop stream: %w", err)
	}
	defer stream.Close()
	if err := transport.WriteStreamFrameHeader(stream, transport.StreamHeader{
		Type:        transport.StreamTypeWorkspaceStop,
		WorkspaceID: materialization.WorkspaceID,
	}, 0); err != nil {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("write workspace stop header: %w", err)
	}
	if err := transport.WriteProtoFrame(stream, &workspacev0.StopWorkspaceRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			MaterializationId: materialization.ID,
			WorkspaceId:       materialization.WorkspaceID,
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
	header, bodyLen, err := transport.ReadStreamFrameHeader(stream)
	if err != nil {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("read workspace stop artifact header: %w", err)
	}
	if header.Type != transport.StreamTypeWorkspaceArtifact {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace stop returned artifact stream type %q", header.Type)
	}
	if strings.TrimSpace(header.WorkspaceID) != strings.TrimSpace(materialization.WorkspaceID) {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace stop artifact workspace_id %q does not match %q", header.WorkspaceID, materialization.WorkspaceID)
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

func (m WorkspaceMaterializer) dispatchOperation(ctx context.Context, session vm.Session, materialization api.WorkerWorkspaceMaterialization, operation api.WorkerWorkspaceOperation) (api.WorkerWorkspaceOperationCompleteRequest, error) {
	channelToken := m.channelToken(materialization)
	if channelToken == "" {
		return api.WorkerWorkspaceOperationCompleteRequest{}, errors.New("workspace materialization guest channel token is required")
	}
	if strings.TrimSpace(operation.MaterializationID) != strings.TrimSpace(materialization.ID) {
		return api.WorkerWorkspaceOperationCompleteRequest{}, fmt.Errorf("claimed operation materialization %s does not match live materialization %s", operation.MaterializationID, materialization.ID)
	}
	if strings.TrimSpace(operation.WorkspaceID) != strings.TrimSpace(materialization.WorkspaceID) {
		return api.WorkerWorkspaceOperationCompleteRequest{}, fmt.Errorf("claimed operation workspace %s does not match live workspace %s", operation.WorkspaceID, materialization.WorkspaceID)
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
	if err := transport.WriteStreamFrameHeader(stream, transport.StreamHeader{
		Type:        transport.StreamTypeWorkspaceOperation,
		WorkspaceID: materialization.WorkspaceID,
		OperationID: operation.ID,
	}, 0); err != nil {
		return api.WorkerWorkspaceOperationCompleteRequest{}, fmt.Errorf("write workspace operation header: %w", err)
	}
	if err := transport.WriteProtoFrame(stream, &workspacev0.WorkspaceOperationRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			OperationId:                operation.ID,
			MaterializationId:          operation.MaterializationID,
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

func (m WorkspaceMaterializer) openWorkspaceEventStream(ctx context.Context, session vm.Session, materialization api.WorkerWorkspaceMaterialization) (io.ReadWriteCloser, error) {
	channelToken := m.channelToken(materialization)
	if channelToken == "" {
		return nil, errors.New("workspace materialization guest channel token is required")
	}
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("open workspace event stream: %w", err)
	}
	if err := transport.WriteStreamFrameHeader(stream, transport.StreamHeader{
		Type:        transport.StreamTypeWorkspaceEvents,
		WorkspaceID: materialization.WorkspaceID,
	}, 0); err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("write workspace event stream header: %w", err)
	}
	if err := transport.WriteProtoFrame(stream, workspaceEventEnvelope(materialization, channelToken)); err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("write workspace event stream envelope: %w", err)
	}
	return stream, nil
}

func workspaceEventEnvelope(materialization api.WorkerWorkspaceMaterialization, channelToken string) *workspacev0.WorkspaceOperationEnvelope {
	return &workspacev0.WorkspaceOperationEnvelope{
		MaterializationId: materialization.ID,
		WorkspaceId:       materialization.WorkspaceID,
		ChannelToken:      channelToken,
		FencingGeneration: uint64(materialization.FencingGeneration),
	}
}

func (m WorkspaceMaterializer) runWorkspaceEventLoop(ctx context.Context, session vm.Session, stream io.ReadWriteCloser, materialization api.WorkerWorkspaceMaterialization, client api.WorkerWorkspaceMaterializerControlClient) error {
	defer stream.Close()
	persistState := newWorkspaceOperationEventPersistState()
	for {
		var event workspacev0.WorkspaceOperationEvent
		if err := readProtoFrameFromReaderContext(ctx, session, stream, &event); err != nil {
			return fmt.Errorf("read workspace operation event: %w", err)
		}
		if err := retryWorkspaceOperation(ctx, m.CompleteErrorBackoff, func() error {
			return m.persistWorkspaceOperationEvent(ctx, client, materialization, persistState, &event)
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

func (m WorkspaceMaterializer) startWorkspaceInputRelay(ctx context.Context, session vm.Session, materialization api.WorkerWorkspaceMaterialization, operation api.WorkerWorkspaceOperation, client api.WorkerWorkspaceMaterializerControlClient, failures chan<- error, relays *workspaceInputRelayRegistry) {
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
				return m.runWorkspaceExecInputRelay(ctx, session, materialization, operation, execID, client)
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
				return m.runWorkspacePtyInputRelay(ctx, session, materialization, operation, ptyID, client)
			})
		}
	}
}

func (m WorkspaceMaterializer) reportWorkspaceInputRelayFailure(ctx context.Context, failures chan<- error, resourceKind string, resourceID string, run func() error) {
	if err := run(); err != nil && ctx.Err() == nil {
		failure := materializationFailure{
			code: "workspace_materialization_input_stream_lost",
			err:  fmt.Errorf("workspace %s %s input relay failed: %w", resourceKind, resourceID, err),
		}
		select {
		case failures <- failure:
		case <-ctx.Done():
		}
	}
}

func (m WorkspaceMaterializer) runWorkspaceExecInputRelay(ctx context.Context, session vm.Session, materialization api.WorkerWorkspaceMaterialization, operation api.WorkerWorkspaceOperation, execID string, client api.WorkerWorkspaceMaterializerControlClient) error {
	stream, err := m.openWorkspaceInputStream(ctx, session, materialization, operation)
	if err != nil {
		return err
	}
	defer stream.Close()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	scope := workerPrimitiveScope(materialization)
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

func (m WorkspaceMaterializer) runWorkspacePtyInputRelay(ctx context.Context, session vm.Session, materialization api.WorkerWorkspaceMaterialization, operation api.WorkerWorkspaceOperation, ptyID string, client api.WorkerWorkspaceMaterializerControlClient) error {
	stream, err := m.openWorkspaceInputStream(ctx, session, materialization, operation)
	if err != nil {
		return err
	}
	defer stream.Close()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	scope := workerPrimitiveScope(materialization)
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

func (m WorkspaceMaterializer) openWorkspaceInputStream(ctx context.Context, session vm.Session, materialization api.WorkerWorkspaceMaterialization, operation api.WorkerWorkspaceOperation) (io.ReadWriteCloser, error) {
	channelToken := m.channelToken(materialization)
	if channelToken == "" {
		return nil, errors.New("workspace materialization guest channel token is required")
	}
	if strings.TrimSpace(operation.MaterializationID) != strings.TrimSpace(materialization.ID) {
		return nil, fmt.Errorf("input relay operation materialization %s does not match live materialization %s", operation.MaterializationID, materialization.ID)
	}
	if strings.TrimSpace(operation.WorkspaceID) != strings.TrimSpace(materialization.WorkspaceID) {
		return nil, fmt.Errorf("input relay operation workspace %s does not match live workspace %s", operation.WorkspaceID, materialization.WorkspaceID)
	}
	if operation.FencingGeneration <= 0 {
		return nil, errors.New("input relay operation fencing_generation is required")
	}
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("open workspace input stream: %w", err)
	}
	if err := transport.WriteStreamFrameHeader(stream, transport.StreamHeader{
		Type:        transport.StreamTypeWorkspaceInput,
		WorkspaceID: materialization.WorkspaceID,
	}, 0); err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("write workspace input header: %w", err)
	}
	if err := transport.WriteProtoFrame(stream, workspaceInputEnvelope(operation, channelToken)); err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("write workspace input envelope: %w", err)
	}
	return stream, nil
}

func workspaceInputEnvelope(operation api.WorkerWorkspaceOperation, channelToken string) *workspacev0.WorkspaceOperationEnvelope {
	return &workspacev0.WorkspaceOperationEnvelope{
		OperationId:                operation.ID,
		MaterializationId:          operation.MaterializationID,
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
	if err := transport.WriteProtoFrame(stream, &workspacev0.WorkspaceInputFrame{
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
	if err := transport.WriteProtoFrame(stream, &workspacev0.WorkspaceInputFrame{
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

func workerPrimitiveScope(materialization api.WorkerWorkspaceMaterialization) api.WorkerWorkspacePrimitiveScope {
	return api.WorkerWorkspacePrimitiveScope{
		OrgID:             materialization.OrgID,
		ProjectID:         materialization.ProjectID,
		EnvironmentID:     materialization.EnvironmentID,
		WorkspaceID:       materialization.WorkspaceID,
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
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

func (m WorkspaceMaterializer) persistWorkspaceOperationEvent(ctx context.Context, client api.WorkerWorkspaceMaterializerControlClient, materialization api.WorkerWorkspaceMaterialization, state *workspaceOperationEventPersistState, event *workspacev0.WorkspaceOperationEvent) error {
	scope := api.WorkerWorkspacePrimitiveScope{
		OrgID:             materialization.OrgID,
		ProjectID:         materialization.ProjectID,
		EnvironmentID:     materialization.EnvironmentID,
		WorkspaceID:       materialization.WorkspaceID,
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
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

func (m WorkspaceMaterializer) channelToken(materialization api.WorkerWorkspaceMaterialization) string {
	token := strings.TrimSpace(materialization.GuestdChannelToken)
	if token == "" {
		return ""
	}
	return token
}

func (m WorkspaceMaterializer) failMaterialization(client api.WorkerWorkspaceMaterializerControlClient, materialization api.WorkerWorkspaceMaterialization, cause error) error {
	body := workspaceMaterializationError(cause)
	ctx, cancel := context.WithTimeout(context.Background(), m.failureTimeout())
	defer cancel()
	_, err := client.FailWorkspaceMaterialization(ctx, api.WorkerWorkspaceMaterializationFailRequest{
		OrgID:             materialization.OrgID,
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
		Error:             body,
	})
	return err
}

func workspaceMaterializationError(err error) json.RawMessage {
	code := "workspace_materialization_failed"
	var failure materializationFailure
	if errors.As(err, &failure) && strings.TrimSpace(failure.code) != "" {
		code = strings.TrimSpace(failure.code)
	} else if errors.Is(err, context.DeadlineExceeded) {
		code = "workspace_materialization_startup_timeout"
	}
	body, marshalErr := json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{
		Code:    code,
		Message: err.Error(),
	})
	if marshalErr != nil {
		return json.RawMessage(`{"code":"workspace_materialization_failed"}`)
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

func startWorkspaceMaterializationOperation(ctx context.Context, client api.WorkerWorkspaceMaterializerControlClient, request api.WorkerWorkspaceOperationStartRequest, backoff time.Duration) error {
	return retryWorkspaceOperation(ctx, backoff, func() error {
		_, err := client.StartWorkspaceMaterializationOperation(ctx, request)
		return err
	})
}

func completeWorkspaceMaterializationOperation(ctx context.Context, client api.WorkerWorkspaceMaterializerControlClient, request api.WorkerWorkspaceOperationCompleteRequest, backoff time.Duration) error {
	return retryWorkspaceOperation(ctx, backoff, func() error {
		_, err := client.CompleteWorkspaceMaterializationOperation(ctx, request)
		return err
	})
}

func retryWorkspaceOperation(ctx context.Context, backoff time.Duration, fn func() error) error {
	const attempts = 3
	if backoff <= 0 {
		backoff = 250 * time.Millisecond
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
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
