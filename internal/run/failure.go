package run

import (
	"encoding/json"

	"uuid"
)

type Failure struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

func MarshalFailure(code, message string, details map[string]any) (json.RawMessage, error) {
	if details == nil {
		details = map[string]any{}
	}
	return json.Marshal(Failure{Code: code, Message: message, Details: details})
}

func marshalChildFailureResult(runID uuid.UUID, code, message string) (json.RawMessage, error) {
	type childRun struct {
		ID string `json:"id"`
	}
	return json.Marshal(struct {
		OK      bool     `json:"ok"`
		Failure Failure  `json:"failure"`
		Run     childRun `json:"run"`
	}{
		Failure: Failure{Code: code, Message: message, Details: map[string]any{}},
		Run:     childRun{ID: runID.String()},
	})
}
