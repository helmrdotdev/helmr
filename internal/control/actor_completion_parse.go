package control

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/api"
)

type actorCompletionKind string

const (
	actorCompletionSucceeded actorCompletionKind = "succeeded"
	actorCompletionFailed    actorCompletionKind = "failed"
)

type parsedActorCompletion struct {
	lease                 parsedRunLeaseFence
	kind                  actorCompletionKind
	terminalInputSequence int64
	errorObject           json.RawMessage
	capture               *parsedTaskWorkspaceCapture
	rollback              *parsedTaskWorkspaceRollback
	fingerprint           string
}

func parseActorCompletionRequest(request api.WorkerCompleteActorRequest) (parsedActorCompletion, error) {
	lease, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		return parsedActorCompletion{}, err
	}
	if request.Outcome.TerminalInputSequence < 0 {
		return parsedActorCompletion{}, errors.New("outcome.terminal_input_sequence must be non-negative")
	}
	normalized := request
	parsed := parsedActorCompletion{
		lease: lease, terminalInputSequence: request.Outcome.TerminalInputSequence,
	}
	variants := 0
	if request.Outcome.Succeeded != nil {
		variants++
		parsed.kind = actorCompletionSucceeded
		normalized.Outcome.Succeeded = &api.WorkerActorSucceeded{}
	}
	if request.Outcome.Failed != nil {
		variants++
		parsed.kind = actorCompletionFailed
		parsed.errorObject, normalized.Outcome.Failed, err = normalizeTaskFailure("outcome.failed", request.Outcome.Failed)
		if err != nil {
			return parsedActorCompletion{}, err
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
	if parsed.kind == actorCompletionSucceeded && parsed.capture == nil {
		return parsedActorCompletion{}, errors.New("a successful Actor requires a captured Workspace")
	}
	if parsed.kind == actorCompletionFailed && parsed.rollback == nil {
		return parsedActorCompletion{}, errors.New("a failed Actor requires a Workspace rollback")
	}
	parsed.fingerprint, err = terminalRequestFingerprint("actor.complete.v0", normalized)
	if err != nil {
		return parsedActorCompletion{}, fmt.Errorf("fingerprint Actor completion: %w", err)
	}
	return parsed, nil
}
