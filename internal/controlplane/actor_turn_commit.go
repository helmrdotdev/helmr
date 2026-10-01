package controlplane

import (
	"encoding/json"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type parsedActorTurnCommit struct {
	turnID              uuid.UUID
	generation          int64
	disposition         string
	result              json.RawMessage
	fingerprint         string
	lease               parsedRunLeaseFence
	correlationID       uuid.UUID
	targetInputSequence int64
}

func parseActorTurnCommitRequest(request workerapi.CommitActorTurnRequest) (parsedActorTurnCommit, error) {
	turnID, err := parseCanonicalUUID("turn_id", request.TurnID)
	if err != nil {
		return parsedActorTurnCommit{}, err
	}
	if request.RunGeneration <= 0 {
		return parsedActorTurnCommit{}, errors.New("run_generation must be positive")
	}
	if request.Disposition != "completed" && request.Disposition != "failed" {
		return parsedActorTurnCommit{}, errors.New("disposition must be completed or failed")
	}
	payload := request.Result
	if request.Disposition == "failed" {
		if len(request.Result) != 0 || len(request.Error) == 0 {
			return parsedActorTurnCommit{}, errors.New("failed settlement requires error and forbids result")
		}
		payload = request.Error
	} else if len(request.Error) != 0 {
		return parsedActorTurnCommit{}, errors.New("completed settlement forbids error")
	}
	var result json.RawMessage
	if len(payload) != 0 {
		result, err = canonicalJSON(payload)
		if err != nil {
			return parsedActorTurnCommit{}, errors.New("settlement payload must be valid JSON")
		}
	}
	if request.Disposition == "failed" {
		request.Error = result
	} else {
		request.Result = result
	}
	fingerprint, err := run.RequestFingerprint("worker.turn.settle.v1", request)
	if err != nil {
		return parsedActorTurnCommit{}, err
	}
	lease, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		return parsedActorTurnCommit{}, err
	}
	correlationID, err := parseCanonicalUUID("correlation_id", request.CorrelationID)
	if err != nil {
		return parsedActorTurnCommit{}, err
	}
	if request.TargetInputSequence <= 0 {
		return parsedActorTurnCommit{}, errors.New("target_input_sequence must be positive")
	}
	return parsedActorTurnCommit{
		turnID: turnID, generation: request.RunGeneration, disposition: request.Disposition, result: result, fingerprint: fingerprint,
		lease: lease, correlationID: correlationID, targetInputSequence: request.TargetInputSequence,
	}, nil
}

// turnCommit is the session owner's commit of the parsed request.
func (c parsedActorTurnCommit) turnCommit() session.TurnCommit {
	return session.TurnCommit{
		TurnID: c.turnID, RunGeneration: c.generation, Disposition: c.disposition,
		Result: c.result, Fingerprint: c.fingerprint, TargetInputSequence: c.targetInputSequence,
	}
}

func projectActorTurnResponse(
	request workerapi.CommitActorTurnRequest,
	commit parsedActorTurnCommit,
) workerapi.CommitActorTurnResponse {
	return workerapi.CommitActorTurnResponse{
		Lease: request.Lease, CorrelationID: commit.correlationID.String(),
		CommittedInputSequence: commit.targetInputSequence,
	}
}
