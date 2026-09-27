package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/helmrdotdev/helmr/internal/api"
)

func projectRunFailure(raw []byte) (api.RunFailureResponse, error) {
	var response api.RunFailureResponse
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil || response.Code == "" ||
		response.Message == "" || len(response.Details) == 0 {
		return api.RunFailureResponse{}, errors.New("run failure projection is invalid")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return api.RunFailureResponse{}, errors.New("run failure projection is invalid")
	}
	var details map[string]json.RawMessage
	if err := json.Unmarshal(response.Details, &details); err != nil || details == nil {
		return api.RunFailureResponse{}, errors.New("run failure details are invalid")
	}
	return response, nil
}
