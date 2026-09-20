package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type actorCompletionKind string

const (
	actorCompletionSucceeded   actorCompletionKind = "succeeded"
	actorCompletionFailed      actorCompletionKind = "failed"
	actorCompletionInterrupted actorCompletionKind = "interrupted"
)

type parsedActorCompletion struct {
	lease         parsedRunLeaseFence
	kind          actorCompletionKind
	runGeneration int64
	holdID        uuid.UUID
	turnID        *uuid.UUID
	errorObject   json.RawMessage
	capture       *parsedTaskWorkspaceCapture
	rollback      *parsedTaskWorkspaceRollback
	fingerprint   string
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
	parsed := parsedActorCompletion{
		lease:         lease,
		runGeneration: request.Outcome.RunGeneration,
	}
	variants := 0
	if request.Outcome.Succeeded != nil {
		variants++
		parsed.kind = actorCompletionSucceeded
		normalized.Outcome.Succeeded = &workerapi.ActorSucceeded{}
	}
	if request.Outcome.Failed != nil {
		variants++
		parsed.kind = actorCompletionFailed
		parsed.errorObject, normalized.Outcome.Failed, err = normalizeTaskFailure("outcome.failed", request.Outcome.Failed)
		if err != nil {
			return parsedActorCompletion{}, err
		}
	}
	if stopped := request.Outcome.Interrupted; stopped != nil {
		variants++
		parsed.kind = actorCompletionInterrupted
		parsed.holdID, err = parseCanonicalUUID("outcome.interrupted.hold_id", stopped.HoldID)
		if err != nil {
			return parsedActorCompletion{}, err
		}
		if stopped.TurnID != nil {
			id, err := parseCanonicalUUID("outcome.interrupted.turn_id", *stopped.TurnID)
			if err != nil {
				return parsedActorCompletion{}, err
			}
			parsed.turnID = &id
		}
	}
	if variants != 1 {
		return parsedActorCompletion{}, errors.New("outcome must contain exactly one variant")
	}

	proofs := 0
	if request.Workspace.Captured != nil {
		proofs++
		capture, normalizedCapture, parseErr := parseTaskWorkspaceCapture(*request.Workspace.Captured)
		if parseErr != nil {
			return parsedActorCompletion{}, parseErr
		}
		parsed.capture = &capture
		normalized.Workspace.Captured = &normalizedCapture
	}
	if request.Workspace.RolledBack != nil {
		proofs++
		rollback, normalizedRollback, parseErr := parseTaskWorkspaceRollback(*request.Workspace.RolledBack)
		if parseErr != nil {
			return parsedActorCompletion{}, parseErr
		}
		parsed.rollback = &rollback
		normalized.Workspace.RolledBack = &normalizedRollback
	}
	if proofs != 1 {
		return parsedActorCompletion{}, errors.New("workspace must contain exactly one proof")
	}
	if (parsed.kind == actorCompletionSucceeded || parsed.kind == actorCompletionInterrupted) && parsed.capture == nil {
		return parsedActorCompletion{}, errors.New("a successful or cooperatively interrupted actor requires a captured workspace")
	}
	if parsed.kind == actorCompletionFailed && parsed.rollback == nil {
		return parsedActorCompletion{}, errors.New("a failed actor requires a workspace rollback")
	}
	parsed.fingerprint, err = terminalRequestFingerprint("actor.complete.v0", normalized)
	if err != nil {
		return parsedActorCompletion{}, fmt.Errorf("fingerprint actor completion: %w", err)
	}
	return parsed, nil
}
