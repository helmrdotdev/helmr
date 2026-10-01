package controlplane

import (
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// parsedActorCompletion is a decoded Actor completion: the worker's lease
// and the session owner's completion.
type parsedActorCompletion struct {
	lease      parsedRunLeaseFence
	completion session.ActorCompletion
}

func parseActorCompletionRequest(request workerapi.CompleteActorRequest) (parsedActorCompletion, error) {
	lease, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		return parsedActorCompletion{}, err
	}

	if request.Outcome.RunGeneration <= 0 {
		return parsedActorCompletion{}, errors.New("outcome.run_generation must be positive")
	}
	normalized := request
	parsed := parsedActorCompletion{lease: lease}
	completion := &parsed.completion
	completion.RunGeneration = request.Outcome.RunGeneration
	variants := 0
	if request.Outcome.Succeeded != nil {
		variants++
		completion.Kind = session.ActorSucceeded
		normalized.Outcome.Succeeded = &workerapi.ActorSucceeded{}
	}
	if request.Outcome.Failed != nil {
		variants++
		completion.Kind = session.ActorFailed
		completion.Error, normalized.Outcome.Failed, err = normalizeTaskFailure("outcome.failed", request.Outcome.Failed)
		if err != nil {
			return parsedActorCompletion{}, err
		}
	}
	if stopped := request.Outcome.Interrupted; stopped != nil {
		variants++
		completion.Kind = session.ActorInterrupted
		completion.HoldID, err = parseCanonicalUUID("outcome.interrupted.hold_id", stopped.HoldID)
		if err != nil {
			return parsedActorCompletion{}, err
		}
		if stopped.TurnID != nil {
			id, err := parseCanonicalUUID("outcome.interrupted.turn_id", *stopped.TurnID)
			if err != nil {
				return parsedActorCompletion{}, err
			}
			completion.TurnID = &id
		}
	}
	if variants != 1 {
		return parsedActorCompletion{}, errors.New("outcome must contain exactly one variant")
	}

	completion.OperationID, err = parseCanonicalUUID("operation_id", request.OperationID)
	if err != nil {
		return parsedActorCompletion{}, err
	}
	normalized.OperationID = completion.OperationID.String()

	completion.Fingerprint, err = run.RequestFingerprint("actor.complete.v0", normalized)
	if err != nil {
		return parsedActorCompletion{}, fmt.Errorf("fingerprint actor completion: %w", err)
	}
	return parsed, nil
}
