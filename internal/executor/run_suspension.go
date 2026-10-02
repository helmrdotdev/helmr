package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type RunWaitClient interface {
	CreateRunWait(context.Context, workerapi.CreateRunWaitRequest) (workerapi.CreateRunWaitResponse, error)
	PollRunWait(context.Context, workerapi.RunWaitPollRequest) (workerapi.RunWaitPollResponse, error)
	AcknowledgeRunWaitResume(context.Context, workerapi.RunWaitResumeAckRequest) (workerapi.RunWaitResumeAckResponse, error)
}

type ControlPlaneRunWaits struct {
	Client RunWaitClient
}

type RestoreAcknowledgement struct {
	Lease        workerapi.RunLeaseAssignment
	RunWaitID    string
	CheckpointID string
}

type RestoreAcknowledger interface {
	AcknowledgeRestore(context.Context, RestoreAcknowledgement) error
}

func (w ControlPlaneRunWaits) AcknowledgeRestore(ctx context.Context, request RestoreAcknowledgement) error {
	if w.Client == nil {
		return errors.New("run wait control plane client is required")
	}
	response, err := w.Client.AcknowledgeRunWaitResume(ctx, workerapi.RunWaitResumeAckRequest{
		Lease: request.Lease.Fence(), RunWaitID: request.RunWaitID, CheckpointID: request.CheckpointID,
	})
	if err != nil {
		return err
	}
	if response.RunID != request.Lease.RunID || response.RunWaitID != request.RunWaitID || response.CheckpointID != request.CheckpointID {
		return errors.New("run wait resume acknowledgement did not match restored member")
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
			if intent.ResumeKind == "" {
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
			return nil
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

func (w ControlPlaneRunWaits) AddRunWait(ctx context.Context, request WaitRequest) (workerapi.CreateRunWaitResponse, error) {
	if w.Client == nil {
		return workerapi.CreateRunWaitResponse{}, errors.New("run wait control plane client is required")
	}
	lease, err := request.currentLeaseAssignment()
	if err != nil {
		return workerapi.CreateRunWaitResponse{}, err
	}
	return w.Client.CreateRunWait(ctx, workerapi.CreateRunWaitRequest{
		Lease:                           lease.Fence(),
		CorrelationID:                   request.CorrelationID,
		RunWaitID:                       request.RunWaitID,
		ResumeAttachID:                  request.ResumeAttachID,
		Kind:                            request.Kind,
		Params:                          request.Params,
		Metadata:                        request.Metadata,
		Tags:                            request.Tags,
		TimeoutMS:                       request.TimeoutMS,
		IdleTimeoutMS:                   request.IdleTimeoutMS,
		SessionSpeculativeInputSequence: request.SessionSpeculativeInputSequence,
		TurnID:                          request.TurnID, RunGeneration: executionGeneration(request.Execution),
	})
}

func (request WaitRequest) currentLeaseAssignment() (workerapi.RunLeaseAssignment, error) {
	if request.Leases != nil {
		return request.Leases.CurrentWorkerRunLeaseAssignment(), nil
	}
	if request.LeaseAssignment.ID == "" {
		return workerapi.RunLeaseAssignment{}, errors.New("run lease assignment is required for durable waits")
	}
	return request.LeaseAssignment, nil
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

func executionGeneration(execution *programv0.SessionExecution) *int64 {
	if execution == nil {
		return nil
	}
	generation := execution.GetRunGeneration()
	return &generation
}
