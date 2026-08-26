package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type RunWaitClient interface {
	CreateRunWait(context.Context, workerapi.CreateRunWaitRequest) (workerapi.CreateRunWaitResponse, error)
	PollRunWait(context.Context, workerapi.RunWaitPollRequest) (workerapi.RunWaitPollResponse, error)
	AcknowledgeRunWaitResume(context.Context, workerapi.RunWaitResumeAckRequest) (workerapi.RunWaitResumeAckResponse, error)
	MarkCheckpointReady(context.Context, workerapi.CheckpointReadyRequest) (workerapi.CheckpointResponse, error)
	MarkCheckpointFailed(context.Context, workerapi.CheckpointFailedRequest) (workerapi.CheckpointResponse, error)
}

type ControlPlaneRunWaits struct {
	Client RunWaitClient
}

type RestoreAcknowledgement struct {
	Lease                workerapi.RunLeaseAssignment
	RunWaitID            string
	CheckpointID         string
	ResumeAttachID       string
	CorrelationID        string
	ResumeRequestVersion int64
	Phases               []workerapi.CheckpointPhase
}

type RestoreAcknowledger interface {
	AcknowledgeRestore(context.Context, RestoreAcknowledgement) error
}

func (w ControlPlaneRunWaits) AcknowledgeRestore(ctx context.Context, request RestoreAcknowledgement) error {
	client, ok := w.Client.(interface {
		AcknowledgeRunResumeRelease(context.Context, workerapi.RunResumeReleaseRequest) (workerapi.RunResumeReleaseResponse, error)
	})
	if !ok {
		return errors.New("exact run resume release client is required")
	}
	response, err := client.AcknowledgeRunResumeRelease(ctx, workerapi.RunResumeReleaseRequest{
		Lease:                request.Lease.Fence(),
		RunWaitID:            request.RunWaitID,
		CheckpointID:         request.CheckpointID,
		ResumeAttachID:       request.ResumeAttachID,
		ResumeRequestVersion: request.ResumeRequestVersion,
	})
	if err != nil {
		return err
	}
	if response.Lease != request.Lease.Fence() ||
		response.RunWaitID != request.RunWaitID || response.CheckpointID != request.CheckpointID ||
		response.ResumeAttachID != request.ResumeAttachID ||
		response.ResumeRequestVersion != request.ResumeRequestVersion {
		return errors.New("run resume release response did not match exact guest proof")
	}
	return nil
}

func (w ControlPlaneRunWaits) Wait(ctx context.Context, request WaitRequest) error {
	if w.Client == nil {
		return errors.New("run wait control plane client is required")
	}
	opened, err := w.AddRunWait(ctx, request)
	if err != nil {
		return fmt.Errorf("create run wait: %w", err)
	}
	return w.ContinueRunWait(ctx, request, opened)
}

// ContinueRunWait drives an already-created durable Wait. It is used when
// creating a resource and registering the Wait must be one atomic operation.
func (w ControlPlaneRunWaits) ContinueRunWait(
	ctx context.Context,
	request WaitRequest,
	opened workerapi.CreateRunWaitResponse,
) error {
	if w.Client == nil {
		return errors.New("run wait control plane client is required")
	}
	lease, err := request.currentLeaseAssignment()
	if err != nil {
		return err
	}
	if opened.RunID != lease.RunID ||
		opened.RunWaitID != request.RunWaitID ||
		opened.ResumeAttachID != request.ResumeAttachID {
		return errors.New("run wait creation response did not match exact request identity")
	}
	if opened.ResolutionKind != "" {
		if request.Resume == nil {
			return errors.New("runtime resume support is required")
		}
		return request.Resume(ctx, WaitResumeDecision{
			Kind: opened.ResolutionKind,
			Data: opened.Resolution,
		})
	}
	pollDelay := 100 * time.Millisecond
	for {
		lease, err := request.currentLeaseAssignment()
		if err != nil {
			return err
		}
		intent, pollErr := w.Client.PollRunWait(ctx, workerapi.RunWaitPollRequest{
			Lease:     lease.Fence(),
			RunWaitID: opened.RunWaitID,
		})
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if pollErr != nil {
			return fmt.Errorf("poll run wait: %w", pollErr)
		}
		if intent.RunID != opened.RunID || intent.RunWaitID != opened.RunWaitID {
			return errors.New("run wait poll returned a mismatched fence")
		}
		switch intent.Status {
		case workerapi.RunWaitPollStatusWaiting:
		case workerapi.RunWaitPollStatusResumeRequested:
			if intent.ResumeKind == "" || (intent.RequireAck && intent.RequestVersion <= 0) {
				return errors.New("run wait resume request fence and kind are invalid")
			}
			if request.Resume == nil {
				return errors.New("runtime resume support is required")
			}
			payload := intent.ResumePayload
			if len(payload) == 0 {
				payload = []byte("null")
			}
			if err := request.Resume(ctx, WaitResumeDecision{Kind: intent.ResumeKind, Data: payload}); err != nil {
				return err
			}
			if intent.RequireAck {
				lease, err := request.currentLeaseAssignment()
				if err != nil {
					return err
				}
				if _, err := w.Client.AcknowledgeRunWaitResume(ctx, workerapi.RunWaitResumeAckRequest{
					Lease: lease.Fence(), RunWaitID: opened.RunWaitID, ResumeRequestVersion: intent.RequestVersion,
				}); err != nil {
					return fmt.Errorf("acknowledge run wait resume: %w", err)
				}
			}
			return nil
		case workerapi.RunWaitPollStatusCheckpointRequested:
			return w.handleCheckpointDecision(ctx, request, intent)
		case workerapi.RunWaitPollStatusTerminal:
			return errors.New("run wait became terminal before resume")
		default:
			return fmt.Errorf("unsupported run wait poll status %q", intent.Status)
		}
		if err := sleepWithContext(ctx, pollDelay); err != nil {
			return err
		}
		if pollDelay < time.Second {
			pollDelay *= 2
		}
	}
}

func (w ControlPlaneRunWaits) handleCheckpointDecision(ctx context.Context, request WaitRequest, intent workerapi.RunWaitPollResponse) (resultErr error) {
	if intent.CheckpointID == "" || intent.RequestVersion <= 0 {
		return errors.New("checkpoint request id and version are required")
	}
	failCheckpoint := func(err error) error {
		lease, leaseErr := request.currentLeaseAssignment()
		if leaseErr != nil {
			return leaseErr
		}
		failedRequest := workerapi.CheckpointFailedRequest{
			Lease: lease.Fence(), RequestVersion: intent.RequestVersion,
			RunWaitID: intent.RunWaitID, CheckpointID: intent.CheckpointID, Error: err.Error(),
		}
		for {
			if _, failErr := w.Client.MarkCheckpointFailed(ctx, failedRequest); failErr == nil {
				return ErrDetached
			} else if !checkpointReadyRetryable(failErr) {
				return errors.Join(err, failErr)
			} else if sleepErr := sleepWithContext(ctx, 250*time.Millisecond); sleepErr != nil {
				return errors.Join(err, failErr, sleepErr)
			}
		}
	}
	if request.Checkpointer == nil {
		return failCheckpoint(errors.New("run checkpoint support is required"))
	}
	lease, err := request.currentLeaseAssignment()
	if err != nil {
		return err
	}
	checkpointRequest := CheckpointRequest{
		RunID:            lease.RunID,
		RunWaitID:        intent.RunWaitID,
		CorrelationID:    request.CorrelationID,
		CheckpointID:     intent.CheckpointID,
		CaptureWorkspace: intent.CaptureWorkspace,
	}
	if request.ResumeAttachID != "" {
		checkpointRequest.AttemptNumber = lease.AttemptNumber
		checkpointRequest.RunLeaseID = lease.ID
		checkpointRequest.ResumeAttachID = request.ResumeAttachID
		checkpointRequest.CheckpointRequestVersion = intent.RequestVersion
	}
	checkpoint, err := request.Checkpointer.CreateCheckpoint(ctx, checkpointRequest)
	if err != nil {
		return failCheckpoint(err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := request.Checkpointer.ReleaseCheckpointSource(cleanupCtx); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("release checkpoint source: %w", err))
		}
	}()
	if checkpoint.WorkspaceCapture == nil {
		err := errors.New("workspace capture is required before parking")
		return failCheckpoint(err)
	}
	lease, err = request.currentLeaseAssignment()
	if err != nil {
		return err
	}
	readyRequest := workerapi.CheckpointReadyRequest{
		Lease: lease.Fence(), RequestVersion: intent.RequestVersion,
		RunWaitID: intent.RunWaitID, CheckpointID: intent.CheckpointID,
		WorkspaceCapture: *workerCheckpointWorkspaceCapture(checkpoint.WorkspaceCapture),
		Manifest:         checkpoint.Manifest,
	}
	for {
		if _, err := w.Client.MarkCheckpointReady(ctx, readyRequest); err == nil {
			return ErrDetached
		} else if !checkpointReadyRetryable(err) {
			return failCheckpoint(fmt.Errorf("mark checkpoint ready: %w", err))
		} else if err := sleepWithContext(ctx, 250*time.Millisecond); err != nil {
			return err
		}
	}
}

func workerCheckpointWorkspaceCapture(capture *CheckpointWorkspaceCapture) *workerapi.CheckpointWorkspaceCapture {
	if capture == nil {
		return nil
	}
	return &workerapi.CheckpointWorkspaceCapture{
		Tree: workerapi.WorkspaceTreeIdentity{
			Digest: capture.Tree.Digest, SizeBytes: capture.Tree.SizeBytes, EntryCount: int32(capture.Tree.EntryCount),
		},
		Artifact: workerapi.WorkspaceArtifact{
			Digest: capture.Artifact.Digest, MediaType: capture.Artifact.MediaType,
			Encoding: capture.Artifact.Encoding, SizeBytes: capture.Artifact.SizeBytes,
			EntryCount: int32(capture.Artifact.EntryCount),
		},
	}
}

func (w ControlPlaneRunWaits) AddRunWait(ctx context.Context, request WaitRequest) (workerapi.CreateRunWaitResponse, error) {
	if w.Client == nil {
		return workerapi.CreateRunWaitResponse{}, errors.New("run wait control plane client is required")
	}
	lease, err := request.currentLeaseAssignment()
	if err != nil {
		return workerapi.CreateRunWaitResponse{}, err
	}
	return w.Client.CreateRunWait(ctx, workerapi.CreateRunWaitRequest{
		Lease:                         lease.Fence(),
		CorrelationID:                 request.CorrelationID,
		RunWaitID:                     request.RunWaitID,
		ResumeAttachID:                request.ResumeAttachID,
		Kind:                          request.Kind,
		Params:                        request.Params,
		Metadata:                      request.Metadata,
		Tags:                          request.Tags,
		TimeoutMS:                     request.TimeoutMS,
		IdleTimeoutMS:                 request.IdleTimeoutMS,
		ActorSpeculativeInputSequence: request.ActorSpeculativeInputSequence,
	})
}

func (request WaitRequest) currentLeaseAssignment() (workerapi.RunLeaseAssignment, error) {
	if request.Leases != nil {
		provider, ok := request.Leases.(workerapi.RunLeaseAssignmentProvider)
		if !ok {
			return workerapi.RunLeaseAssignment{}, errors.New("run lease assignment provider is required for durable waits")
		}
		return provider.CurrentWorkerRunLeaseAssignment(), nil
	}
	if request.LeaseAssignment.ID == "" {
		return workerapi.RunLeaseAssignment{}, errors.New("run lease assignment is required for durable waits")
	}
	return request.LeaseAssignment, nil
}

func checkpointReadyRetryable(err error) bool {
	var status interface{ HTTPStatusCode() int }
	if errors.As(err, &status) {
		code := status.HTTPStatusCode()
		return code < 400 || code >= 500
	}
	return true
}

func sleepWithContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func durationMilliseconds(value time.Duration) int64 {
	if value <= 0 {
		return 0
	}
	return value.Milliseconds()
}

var _ WaitHandler = ControlPlaneRunWaits{}
