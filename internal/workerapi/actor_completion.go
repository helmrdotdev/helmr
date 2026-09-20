package workerapi

import (
	"encoding/json"
	"errors"
	"fmt"
)

func (value *CompleteActorRequest) UnmarshalJSON(raw []byte) error {
	type request CompleteActorRequest
	var decoded request
	if err := decodeClosedTaskCompletionJSON(raw, &decoded); err != nil {
		return fmt.Errorf("decode actor completion request: %w", err)
	}
	*value = CompleteActorRequest(decoded)
	return nil
}

func (value *ActorOutcome) UnmarshalJSON(raw []byte) error {
	*value = ActorOutcome{}
	var envelope struct {
		RunGeneration *int64          `json:"run_generation"`
		Interrupted   json.RawMessage `json:"interrupted"`
		Succeeded     json.RawMessage `json:"succeeded"`
		Failed        json.RawMessage `json:"failed"`
	}
	if err := decodeClosedTaskCompletionJSON(raw, &envelope); err != nil {
		return fmt.Errorf("decode actor outcome: %w", err)
	}
	if envelope.RunGeneration == nil || *envelope.RunGeneration <= 0 {
		return errors.New("actor run_generation must be a positive integer")
	}
	value.RunGeneration = *envelope.RunGeneration
	variants := 0
	if len(envelope.Succeeded) != 0 {
		variants++
		if isJSONNull(envelope.Succeeded) {
			return errors.New("actor outcome succeeded variant must not be null")
		}
		var succeeded ActorSucceeded
		if err := decodeClosedTaskCompletionJSON(envelope.Succeeded, &succeeded); err != nil {
			return fmt.Errorf("decode actor succeeded outcome: %w", err)
		}
		value.Succeeded = &succeeded
	}
	if len(envelope.Failed) != 0 {
		variants++
		if isJSONNull(envelope.Failed) {
			return errors.New("actor outcome failed variant must not be null")
		}
		var failed TaskFailure
		if err := decodeClosedTaskCompletionJSON(envelope.Failed, &failed); err != nil {
			return fmt.Errorf("decode actor failed outcome: %w", err)
		}
		value.Failed = &failed
	}
	if len(envelope.Interrupted) != 0 {
		variants++
		if isJSONNull(envelope.Interrupted) {
			return errors.New("actor outcome interrupted variant must not be null")
		}
		var interrupted ActorInterrupted
		if err := decodeClosedTaskCompletionJSON(envelope.Interrupted, &interrupted); err != nil {
			return fmt.Errorf("decode actor interrupted outcome: %w", err)
		}
		value.Interrupted = &interrupted
	}
	if variants != 1 {
		return errors.New("actor outcome must contain exactly one variant")
	}
	return nil
}
