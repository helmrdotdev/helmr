package workerapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func (value *CompleteTaskRequest) UnmarshalJSON(raw []byte) error {
	type request CompleteTaskRequest
	var decoded request
	if err := decodeClosedTaskCompletionJSON(raw, &decoded); err != nil {
		return fmt.Errorf("decode task completion request: %w", err)
	}
	*value = CompleteTaskRequest(decoded)
	return nil
}

func (value *TaskOutcome) UnmarshalJSON(raw []byte) error {
	*value = TaskOutcome{}
	var envelope struct {
		Succeeded      json.RawMessage `json:"succeeded"`
		Failed         json.RawMessage `json:"failed"`
		PayloadInvalid json.RawMessage `json:"payload_invalid"`
	}
	if err := decodeClosedTaskCompletionJSON(raw, &envelope); err != nil {
		return fmt.Errorf("decode task outcome: %w", err)
	}
	variants := 0
	if len(envelope.Succeeded) != 0 {
		variants++
		if isJSONNull(envelope.Succeeded) {
			return errors.New("task outcome succeeded variant must not be null")
		}
		var succeeded TaskSucceeded
		if err := decodeClosedTaskCompletionJSON(envelope.Succeeded, &succeeded); err != nil {
			return fmt.Errorf("decode task succeeded outcome: %w", err)
		}
		value.Succeeded = &succeeded
	}
	if len(envelope.Failed) != 0 {
		variants++
		if isJSONNull(envelope.Failed) {
			return errors.New("task outcome failed variant must not be null")
		}
		var failed TaskFailure
		if err := decodeClosedTaskCompletionJSON(envelope.Failed, &failed); err != nil {
			return fmt.Errorf("decode task failed outcome: %w", err)
		}
		value.Failed = &failed
	}
	if len(envelope.PayloadInvalid) != 0 {
		variants++
		if isJSONNull(envelope.PayloadInvalid) {
			return errors.New("task outcome payload_invalid variant must not be null")
		}
		var invalid TaskFailure
		if err := decodeClosedTaskCompletionJSON(envelope.PayloadInvalid, &invalid); err != nil {
			return fmt.Errorf("decode task payload_invalid outcome: %w", err)
		}
		value.PayloadInvalid = &invalid
	}
	if variants != 1 {
		return errors.New("task outcome must contain exactly one variant")
	}
	return nil
}

func (value *TaskWorkspaceProof) UnmarshalJSON(raw []byte) error {
	*value = TaskWorkspaceProof{}
	var envelope struct {
		Captured   json.RawMessage `json:"captured"`
		RolledBack json.RawMessage `json:"rolled_back"`
	}
	if err := decodeClosedTaskCompletionJSON(raw, &envelope); err != nil {
		return fmt.Errorf("decode task workspace proof: %w", err)
	}
	variants := 0
	if len(envelope.Captured) != 0 {
		variants++
		if isJSONNull(envelope.Captured) {
			return errors.New("task workspace captured proof must not be null")
		}
		var captured TaskWorkspaceCapture
		if err := decodeClosedTaskCompletionJSON(envelope.Captured, &captured); err != nil {
			return fmt.Errorf("decode task workspace captured proof: %w", err)
		}
		value.Captured = &captured
	}
	if len(envelope.RolledBack) != 0 {
		variants++
		if isJSONNull(envelope.RolledBack) {
			return errors.New("task workspace rolled_back proof must not be null")
		}
		var rolledBack TaskWorkspaceRollback
		if err := decodeClosedTaskCompletionJSON(envelope.RolledBack, &rolledBack); err != nil {
			return fmt.Errorf("decode task workspace rolled_back proof: %w", err)
		}
		value.RolledBack = &rolledBack
	}
	if variants != 1 {
		return errors.New("task workspace proof must contain exactly one variant")
	}
	return nil
}

func (value *WorkspaceResetTarget) UnmarshalJSON(raw []byte) error {
	type target WorkspaceResetTarget
	var decoded target
	if err := decodeClosedTaskCompletionJSON(raw, &decoded); err != nil {
		return fmt.Errorf("decode workspace reset target: %w", err)
	}
	variants := 0
	if decoded.Empty != nil {
		variants++
	}
	if decoded.Artifact != nil {
		variants++
	}
	if variants != 1 {
		return errors.New("workspace reset target must contain exactly one source")
	}
	*value = WorkspaceResetTarget(decoded)
	return nil
}

func (value *TaskFailure) UnmarshalJSON(raw []byte) error {
	var envelope struct {
		Message json.RawMessage `json:"message"`
		Details json.RawMessage `json:"details"`
	}
	if err := decodeClosedTaskCompletionJSON(raw, &envelope); err != nil {
		return fmt.Errorf("decode task failure: %w", err)
	}
	if len(envelope.Message) == 0 || isJSONNull(envelope.Message) {
		return errors.New("task failure message is required")
	}
	var message string
	if err := json.Unmarshal(envelope.Message, &message); err != nil {
		return errors.New("task failure message must be a string")
	}
	value.Message = message
	value.Details = append(value.Details[:0], envelope.Details...)
	return nil
}

func decodeClosedTaskCompletionJSON(raw []byte, value any) error {
	if _, err := jsoncanon.Transform(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func isJSONNull(raw []byte) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
