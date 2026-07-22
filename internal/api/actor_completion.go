package api

import (
	"encoding/json"
	"errors"
	"fmt"
)

func (value *WorkerCompleteActorRequest) UnmarshalJSON(raw []byte) error {
	type request WorkerCompleteActorRequest
	var decoded request
	if err := decodeClosedTaskCompletionJSON(raw, &decoded); err != nil {
		return fmt.Errorf("decode Actor completion request: %w", err)
	}
	*value = WorkerCompleteActorRequest(decoded)
	return nil
}

func (value *WorkerActorOutcome) UnmarshalJSON(raw []byte) error {
	*value = WorkerActorOutcome{}
	var envelope struct {
		TerminalInputSequence *int64          `json:"terminal_input_sequence"`
		Succeeded             json.RawMessage `json:"succeeded"`
		Failed                json.RawMessage `json:"failed"`
	}
	if err := decodeClosedTaskCompletionJSON(raw, &envelope); err != nil {
		return fmt.Errorf("decode Actor outcome: %w", err)
	}
	if envelope.TerminalInputSequence == nil || *envelope.TerminalInputSequence < 0 {
		return errors.New("Actor terminal_input_sequence must be a non-negative integer")
	}
	value.TerminalInputSequence = *envelope.TerminalInputSequence
	variants := 0
	if len(envelope.Succeeded) != 0 {
		variants++
		if isJSONNull(envelope.Succeeded) {
			return errors.New("Actor outcome succeeded variant must not be null")
		}
		var succeeded WorkerActorSucceeded
		if err := decodeClosedTaskCompletionJSON(envelope.Succeeded, &succeeded); err != nil {
			return fmt.Errorf("decode Actor succeeded outcome: %w", err)
		}
		value.Succeeded = &succeeded
	}
	if len(envelope.Failed) != 0 {
		variants++
		if isJSONNull(envelope.Failed) {
			return errors.New("Actor outcome failed variant must not be null")
		}
		var failed WorkerTaskFailure
		if err := decodeClosedTaskCompletionJSON(envelope.Failed, &failed); err != nil {
			return fmt.Errorf("decode Actor failed outcome: %w", err)
		}
		value.Failed = &failed
	}
	if variants != 1 {
		return errors.New("Actor outcome must contain exactly one variant")
	}
	return nil
}
