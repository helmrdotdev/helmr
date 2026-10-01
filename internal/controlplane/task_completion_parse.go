package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const (
	maxTaskCompletionOutputBytes  = 16 << 20
	maxTaskCompletionErrorBytes   = 16 << 10
	maxTaskCompletionMessageBytes = 1024
)

type taskCompletionKind string

const (
	taskCompletionSucceeded      taskCompletionKind = "succeeded"
	taskCompletionFailed         taskCompletionKind = "failed"
	taskCompletionPayloadInvalid taskCompletionKind = "payload_invalid"
)

type parsedTaskCompletion struct {
	lease       parsedRunLeaseFence
	kind        taskCompletionKind
	output      json.RawMessage
	errorObject json.RawMessage
	operationID uuid.UUID
	fingerprint string
}

func parseTaskCompletionRequest(request workerapi.CompleteTaskRequest) (parsedTaskCompletion, error) {
	lease, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		return parsedTaskCompletion{}, err
	}
	normalized := request

	parsed := parsedTaskCompletion{lease: lease}
	outcomes := 0
	if request.Outcome.Succeeded != nil {
		outcomes++
		parsed.kind = taskCompletionSucceeded
		if len(request.Outcome.Succeeded.Output) == 0 {
			return parsedTaskCompletion{}, errors.New("outcome.succeeded.output is required")
		}
		if len(request.Outcome.Succeeded.Output) > maxTaskCompletionOutputBytes {
			return parsedTaskCompletion{}, errors.New("outcome.succeeded.output is too large")
		}
		parsed.output, err = canonicalJSON(request.Outcome.Succeeded.Output)
		if err != nil {
			return parsedTaskCompletion{}, fmt.Errorf("outcome.succeeded.output must be an unambiguous JSON value: %w", err)
		}
		if len(parsed.output) > maxTaskCompletionOutputBytes {
			return parsedTaskCompletion{}, errors.New("outcome.succeeded.output is too large")
		}
		normalized.Outcome.Succeeded = &workerapi.TaskSucceeded{Output: parsed.output}
	}
	if request.Outcome.Failed != nil {
		outcomes++
		parsed.kind = taskCompletionFailed
		parsed.errorObject, normalized.Outcome.Failed, err = normalizeTaskFailure("outcome.failed", request.Outcome.Failed)
		if err != nil {
			return parsedTaskCompletion{}, err
		}
	}
	if request.Outcome.PayloadInvalid != nil {
		outcomes++
		parsed.kind = taskCompletionPayloadInvalid
		parsed.errorObject, normalized.Outcome.PayloadInvalid, err = normalizeTaskFailure("outcome.payload_invalid", request.Outcome.PayloadInvalid)
		if err != nil {
			return parsedTaskCompletion{}, err
		}
	}
	if outcomes != 1 {
		return parsedTaskCompletion{}, errors.New("outcome must contain exactly one variant")
	}

	parsed.operationID, err = parseCanonicalUUID("operation_id", request.OperationID)
	if err != nil {
		return parsedTaskCompletion{}, err
	}
	normalized.OperationID = parsed.operationID.String()

	parsed.fingerprint, err = run.RequestFingerprint("task.complete.v0", normalized)
	if err != nil {
		return parsedTaskCompletion{}, fmt.Errorf("fingerprint task completion: %w", err)
	}
	return parsed, nil
}

func normalizeTaskFailure(label string, failure *workerapi.TaskFailure) (json.RawMessage, *workerapi.TaskFailure, error) {
	if failure.Message == "" || failure.Message != strings.TrimSpace(failure.Message) ||
		!utf8.ValidString(failure.Message) || len(failure.Message) > maxTaskCompletionMessageBytes {
		return nil, nil, fmt.Errorf("%s.message must be canonical nonempty valid UTF-8 no larger than %d bytes", label, maxTaskCompletionMessageBytes)
	}
	var details json.RawMessage
	if len(failure.Details) != 0 {
		if len(failure.Details) > maxTaskCompletionErrorBytes {
			return nil, nil, fmt.Errorf("%s.details is too large", label)
		}
		canonical, err := canonicalJSON(failure.Details)
		if err != nil {
			return nil, nil, fmt.Errorf("%s.details must be an unambiguous JSON value: %w", label, err)
		}
		details = canonical
		var object map[string]json.RawMessage
		if err := json.Unmarshal(details, &object); err != nil || object == nil {
			return nil, nil, fmt.Errorf("%s.details must be a JSON object", label)
		}
	} else {
		details = json.RawMessage("{}")
	}
	errorObject, err := json.Marshal(struct {
		Message string          `json:"message"`
		Details json.RawMessage `json:"details"`
	}{Message: failure.Message, Details: details})
	if err != nil {
		return nil, nil, fmt.Errorf("encode %s: %w", label, err)
	}
	errorObject, err = canonicalJSON(errorObject)
	if err != nil {
		return nil, nil, fmt.Errorf("canonicalize %s: %w", label, err)
	}
	if len(errorObject) > maxTaskCompletionErrorBytes {
		return nil, nil, fmt.Errorf("%s exceeds %d bytes", label, maxTaskCompletionErrorBytes)
	}
	return errorObject, &workerapi.TaskFailure{Message: failure.Message, Details: details}, nil
}

func parseCanonicalUUID(name, value string) (uuid.UUID, error) {
	parsed, err := ids.Parse(value)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("%s must be a canonical UUIDv7", name)
	}
	return parsed, nil
}
