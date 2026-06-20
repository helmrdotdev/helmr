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
	TempDir              string
	Heartbeat            time.Duration
	StartupTimeout       time.Duration
	FailureTimeout       time.Duration
	PollEvery            time.Duration
	ClaimErrorBackoff    time.Duration
	CompleteErrorBackoff time.Duration
	Network              compute.NetworkPolicy
}

func (m WorkspaceMaterializer) RunWorkspaceMaterialization(ctx context.Context, materialization api.WorkerWorkspaceMaterialization, client interface {
	RenewWorkspaceMaterialization(context.Context, api.WorkerWorkspaceMaterializationRenewRequest) (api.WorkspaceMaterializationResponse, error)
	MarkWorkspaceMaterializationRunning(context.Context, api.WorkerWorkspaceMaterializationRunningRequest) (api.WorkspaceMaterializationResponse, error)
	StopWorkspaceMaterialization(context.Context, api.WorkerWorkspaceMaterializationStopRequest) (api.WorkspaceMaterializationResponse, error)
	FailWorkspaceMaterialization(context.Context, api.WorkerWorkspaceMaterializationFailRequest) (api.WorkspaceMaterializationResponse, error)
	ClaimWorkspaceMaterializationOperation(context.Context, api.WorkerWorkspaceOperationClaimRequest) (api.WorkerWorkspaceOperationClaimResponse, error)
	StartWorkspaceMaterializationOperation(context.Context, api.WorkerWorkspaceOperationStartRequest) (api.WorkspaceOperationResponse, error)
	CompleteWorkspaceMaterializationOperation(context.Context, api.WorkerWorkspaceOperationCompleteRequest) (api.WorkspaceOperationResponse, error)
}) error {
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
	session, sandboxImagePath, workspaceArtifactPath, cleanup, err := m.materializeSession(startupCtx, materialization)
	if err != nil {
		cleanup()
		if renewalErr := renewal.stopAndWait(); renewalErr != nil {
			err = renewalErr
		}
		_ = m.failMaterialization(client, materialization, err)
		return fmt.Errorf("connect workspace materialization guest: %w", err)
	}
	defer cleanup()
	defer func() { _ = m.closeSession(session) }()
	if err := m.registerMaterializationContext(startupCtx, session, materialization, sandboxImagePath, workspaceArtifactPath); err != nil {
		if renewalErr := renewal.stopAndWait(); renewalErr != nil {
			err = renewalErr
		}
		_ = m.failMaterialization(client, materialization, err)
		return err
	}
	if _, err := client.MarkWorkspaceMaterializationRunning(renewal.ctx, api.WorkerWorkspaceMaterializationRunningRequest{
		OrgID:             materialization.OrgID,
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
	}); err != nil {
		if renewalErr := renewal.stopAndWait(); renewalErr != nil {
			err = renewalErr
		}
		_ = m.failMaterialization(client, materialization, err)
		return fmt.Errorf("mark workspace materialization running: %w", err)
	}
	sessionExited := make(chan error, 1)
	go func() {
		sessionExited <- session.Wait(renewal.ctx)
	}()
	failAndReturn := func(cause error) error {
		if ctx.Err() == nil {
			_ = m.failMaterialization(client, materialization, cause)
		}
		return cause
	}
	stopAndReturn := func() error {
		_ = renewal.stopAndWait()
		_, _ = client.StopWorkspaceMaterialization(context.Background(), api.WorkerWorkspaceMaterializationStopRequest{
			OrgID:             materialization.OrgID,
			MaterializationID: materialization.ID,
			ReservationToken:  materialization.ReservationToken,
		})
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
	for {
		select {
		case <-ctx.Done():
			return stopAndReturn()
		case err := <-renewDone:
			renewDone = nil
			renewal.once.Do(func() { renewal.err = err })
			if err != nil {
				return failAndReturn(err)
			}
		case err := <-sessionExited:
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
			poll.Reset(pollEvery)
		}
	}
}

type workspaceMaterializationClient interface {
	RenewWorkspaceMaterialization(context.Context, api.WorkerWorkspaceMaterializationRenewRequest) (api.WorkspaceMaterializationResponse, error)
	MarkWorkspaceMaterializationRunning(context.Context, api.WorkerWorkspaceMaterializationRunningRequest) (api.WorkspaceMaterializationResponse, error)
	StopWorkspaceMaterialization(context.Context, api.WorkerWorkspaceMaterializationStopRequest) (api.WorkspaceMaterializationResponse, error)
	FailWorkspaceMaterialization(context.Context, api.WorkerWorkspaceMaterializationFailRequest) (api.WorkspaceMaterializationResponse, error)
	ClaimWorkspaceMaterializationOperation(context.Context, api.WorkerWorkspaceOperationClaimRequest) (api.WorkerWorkspaceOperationClaimResponse, error)
	StartWorkspaceMaterializationOperation(context.Context, api.WorkerWorkspaceOperationStartRequest) (api.WorkspaceOperationResponse, error)
	CompleteWorkspaceMaterializationOperation(context.Context, api.WorkerWorkspaceOperationCompleteRequest) (api.WorkspaceOperationResponse, error)
}

type materializationRenewal struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
	err    error
}

func (r *materializationRenewal) stopAndWait() error {
	r.once.Do(func() {
		r.cancel()
		r.err = <-r.done
	})
	return r.err
}

func (m WorkspaceMaterializer) startRenewalLoop(ctx context.Context, request api.WorkerWorkspaceMaterializationRenewRequest, client workspaceMaterializationClient, every time.Duration) *materializationRenewal {
	renewCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
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
				if _, renewErr := client.RenewWorkspaceMaterialization(renewCtx, request); renewErr != nil {
					err = fmt.Errorf("renew workspace materialization: %w", renewErr)
					cancel()
					return
				}
			}
		}
	}()
	return &materializationRenewal{ctx: renewCtx, cancel: cancel, done: done}
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

func (m WorkspaceMaterializer) failMaterialization(client workspaceMaterializationClient, materialization api.WorkerWorkspaceMaterialization, cause error) error {
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

func startWorkspaceMaterializationOperation(ctx context.Context, client workspaceMaterializationClient, request api.WorkerWorkspaceOperationStartRequest, backoff time.Duration) error {
	return retryWorkspaceOperation(ctx, backoff, func() error {
		_, err := client.StartWorkspaceMaterializationOperation(ctx, request)
		return err
	})
}

func completeWorkspaceMaterializationOperation(ctx context.Context, client workspaceMaterializationClient, request api.WorkerWorkspaceOperationCompleteRequest, backoff time.Duration) error {
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
