package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"strings"
	"time"
)

func (task *guestRunLeaseTask) handleTurnSettle(
	ctx context.Context,
	requested *programv0.TurnSettleRequested,
) (retErr error) {
	defer func() {
		if retErr == nil || task.program.session == nil {
			return
		}
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		var err error
		if source, ok := task.program.session.(CheckpointSourceReleaser); ok {
			err = source.ReleaseCheckpointSource(stopCtx)
		} else {
			err = task.program.session.Close(stopCtx)
		}
		if err != nil {
			retErr = errors.Join(retErr, &checkpointSourceReleaseError{err: err})
		}
	}()
	if requested == nil || strings.TrimSpace(requested.GetCorrelationId()) == "" || requested.GetTargetInputSequence() <= 0 {
		return errors.New("actor turn commit request is invalid")
	}
	scope, err := task.turnRequest(requested.GetCorrelationId(), requested.GetExecution())
	if err != nil {
		return err
	}
	if requested.GetDisposition() != "completed" && requested.GetDisposition() != "failed" {
		return errors.New("turn settlement disposition is invalid")
	}
	if requested.ResultJson != nil && !json.Valid([]byte(requested.GetResultJson())) || requested.ErrorJson != nil && !json.Valid([]byte(requested.GetErrorJson())) {
		return errors.New("turn settlement payload is invalid")
	}
	task.mu.Lock()
	if task.finished || task.finalizingKind != "" {
		task.mu.Unlock()
		return errors.New("run lease task cannot commit an actor turn")
	}
	stream := task.programStream()
	task.mu.Unlock()
	request := workerapi.CommitActorTurnRequest{
		TurnID: scope.TurnID, RunGeneration: scope.RunGeneration,
		Disposition: requested.GetDisposition(), Result: json.RawMessage(requested.GetResultJson()), Error: json.RawMessage(requested.GetErrorJson()),
		CorrelationID: requested.GetCorrelationId(), TargetInputSequence: requested.GetTargetInputSequence(),
	}
	var response workerapi.CommitActorTurnResponse
	if err := task.callRunSourceRuntime(ctx, func(callCtx context.Context, lease workerapi.RunLeaseAssignment) error {
		request.Lease = lease.Fence()
		var err error
		response, err = task.controlPlane.CommitActorTurn(callCtx, request)
		return err
	}); err != nil {
		return fmt.Errorf("commit actor turn: %w", err)
	}
	if response.Lease != request.Lease || response.CorrelationID != request.CorrelationID ||
		response.CommittedInputSequence != request.TargetInputSequence || strings.TrimSpace(response.EventID) == "" {
		return errors.New("actor turn commit response did not match the request")
	}
	stopClose := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stopClose()
	return wire.WriteResumeDecision(stream, &programv0.ResumeDecision{
		CorrelationId: requested.GetCorrelationId(), Kind: "committed", DataJson: `{}`,
	})
}
