package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// freeze returns the complete captured identity only after the guest confirms
// every member. The physical owner must exclude the source on an uncertain
// response; freeze never thaws it or closes the whole VM. Cancellation closes
// only the freeze stream, at any step of the exchange.
func (g guestControl) freeze(ctx context.Context, target workerapi.RuntimeReconcileTarget) (workerapi.CheckpointRecoveryPoint, error) {
	request, err := computerFreezeRequest(target)
	if err != nil {
		return workerapi.CheckpointRecoveryPoint{}, err
	}
	if g.machine == nil {
		return workerapi.CheckpointRecoveryPoint{}, errors.New("computer capture session is required")
	}
	if err = ctx.Err(); err != nil {
		return workerapi.CheckpointRecoveryPoint{}, err
	}
	var response computerv0.FreezeComputerResponse
	step, err := g.exchange(ctx, guestControlExchange{
		header:       wire.StreamHeader{Type: wire.StreamTypeComputerFreeze, ComputerID: request.ComputerId, CheckpointID: request.CheckpointId},
		request:      request,
		response:     &response,
		cancellation: guestControlCancelCloseStream,
	})
	if err != nil {
		if step == guestControlOpen {
			return workerapi.CheckpointRecoveryPoint{}, fmt.Errorf("open computer freeze stream: %w", err)
		}
		return workerapi.CheckpointRecoveryPoint{}, err
	}
	return computerFrozenRecoveryPoint(target, request, &response)
}

func computerFreezeRequest(target workerapi.RuntimeReconcileTarget) (*computerv0.FreezeComputerRequest, error) {
	capture := target.Capture
	if target.Action != workerapi.RuntimeReconcileCapture || strings.TrimSpace(target.ID) == "" || target.WorkerEpoch <= 0 || target.DesiredVersion <= 0 || strings.TrimSpace(target.Source.ComputerID) == "" || strings.TrimSpace(target.Source.ComputerSpecID) == "" || target.Source.WriterGeneration <= 0 || capture == nil || strings.TrimSpace(capture.CheckpointID) == "" || capture.MembershipRevision < 0 || (len(capture.Runs) > 0 && strings.TrimSpace(capture.ProgramDeploymentID) == "") {
		return nil, errors.New("computer capture intent is incomplete")
	}
	request := &computerv0.FreezeComputerRequest{ComputerId: target.Source.ComputerID, ComputerInstanceId: target.ID, WriterGeneration: target.Source.WriterGeneration, CheckpointId: capture.CheckpointID, DesiredVersion: target.DesiredVersion, MembershipRevision: capture.MembershipRevision, Runs: make([]*computerv0.ComputerCaptureRun, 0, len(capture.Runs))}
	seen := make(map[string]bool, len(capture.Runs))
	for _, run := range capture.Runs {
		if strings.TrimSpace(run.RunID) == "" || run.AttemptNumber <= 0 || strings.TrimSpace(run.RunWaitID) == "" || strings.TrimSpace(run.RunLeaseID) == "" || seen[run.RunID] || (run.ActorSpeculativeInputSequence != nil && *run.ActorSpeculativeInputSequence < 0) {
			return nil, errors.New("computer capture member is incomplete or duplicated")
		}
		seen[run.RunID] = true
		request.Runs = append(request.Runs, &computerv0.ComputerCaptureRun{RunId: run.RunID, AttemptNumber: uint32(run.AttemptNumber), RunWaitId: run.RunWaitID, RunLeaseId: run.RunLeaseID})
	}
	return request, nil
}

func computerFrozenRecoveryPoint(target workerapi.RuntimeReconcileTarget, request *computerv0.FreezeComputerRequest, response *computerv0.FreezeComputerResponse) (workerapi.CheckpointRecoveryPoint, error) {
	identity := response.GetIdentity()
	if identity == nil || identity.ComputerId != request.ComputerId || identity.SourceComputerInstanceId != request.ComputerInstanceId || identity.WriterGeneration != request.WriterGeneration || identity.CheckpointId != request.CheckpointId || response.GetDesiredVersion() != request.DesiredVersion || response.GetMembershipRevision() != request.MembershipRevision || len(identity.Runs) != len(request.Runs) {
		return workerapi.CheckpointRecoveryPoint{}, errors.New("computer freeze proof changed the capture identity")
	}
	expected := make(map[string]*computerv0.ComputerCaptureRun, len(request.Runs))
	for _, run := range request.Runs {
		expected[run.RunId] = run
	}
	correlations := make(map[string]string, len(identity.Runs))
	for _, run := range identity.Runs {
		if run == nil {
			return workerapi.CheckpointRecoveryPoint{}, errors.New("computer freeze proof contains an empty member")
		}
		member, ok := expected[run.RunId]
		if !ok || run.AttemptNumber != member.AttemptNumber || run.RunWaitId != member.RunWaitId || run.RunLeaseId != member.RunLeaseId || strings.TrimSpace(run.CorrelationId) == "" {
			return workerapi.CheckpointRecoveryPoint{}, errors.New("computer freeze proof changed captured membership")
		}
		correlations[run.RunId] = run.CorrelationId
		delete(expected, run.RunId)
	}
	point := workerapi.CheckpointRecoveryPoint{ID: request.CheckpointId, ComputerID: request.ComputerId, ComputerInstanceID: request.ComputerInstanceId, WriterGeneration: request.WriterGeneration, MembershipRevision: request.MembershipRevision, ComputerSpecID: target.Source.ComputerSpecID, ProgramDeploymentID: target.Capture.ProgramDeploymentID, Runs: make([]workerapi.CheckpointRun, 0, len(target.Capture.Runs))}
	for _, run := range target.Capture.Runs {
		captured := workerapi.CheckpointRun{RunID: run.RunID, AttemptNumber: run.AttemptNumber, RunWaitID: run.RunWaitID, RunLeaseID: run.RunLeaseID, CorrelationID: correlations[run.RunID]}
		if run.ActorSpeculativeInputSequence != nil {
			cursor := *run.ActorSpeculativeInputSequence
			captured.ActorSpeculativeInputSequence = &cursor
		}
		point.Runs = append(point.Runs, captured)
	}
	return point, nil
}
