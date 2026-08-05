package controlplane

import (
	"encoding/json"
	"errors"

	"github.com/helmrdotdev/helmr/internal/api"
)

func runFailureFromCompletion(code string, raw []byte) (json.RawMessage, error) {
	var completion struct {
		Message string          `json:"message"`
		Details json.RawMessage `json:"details"`
	}
	if err := json.Unmarshal(raw, &completion); err != nil || completion.Message == "" {
		return nil, errors.New("run completion failure is invalid")
	}
	if len(completion.Details) == 0 {
		completion.Details = json.RawMessage("{}")
	}
	var details map[string]json.RawMessage
	if err := json.Unmarshal(completion.Details, &details); err != nil || details == nil {
		return nil, errors.New("run completion failure details must be an object")
	}
	return json.Marshal(api.RunFailureResponse{
		Code: code, Message: completion.Message, Details: completion.Details,
	})
}

func runFailure(code, message string) (json.RawMessage, error) {
	return json.Marshal(api.RunFailureResponse{
		Code: code, Message: message, Details: json.RawMessage("{}"),
	})
}

func sessionFailure(code, message, runID string) (json.RawMessage, error) {
	return json.Marshal(api.SessionFailure{
		Code: code, Message: message,
		Details: api.SessionFailureDetails{RunID: runID},
	})
}
