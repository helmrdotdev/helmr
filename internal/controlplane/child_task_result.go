package controlplane

import (
	"encoding/json"
	"errors"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

// childTaskResult projects a terminal child Task Run's result for its
// parent's call.
func childTaskResult(run db.Run) (json.RawMessage, error) {
	runID := pgvalue.UUIDString(run.ID)
	if err := ids.Validate(runID); err != nil {
		return nil, err
	}
	if run.Status == db.RunStatusSucceeded {
		if run.Output == nil || !json.Valid(run.Output) {
			return nil, errors.New("succeeded child task has invalid output")
		}
		return json.Marshal(struct {
			OK     bool            `json:"ok"`
			Output json.RawMessage `json:"output"`
			Run    struct {
				ID string `json:"id"`
			} `json:"run"`
		}{OK: true, Output: run.Output, Run: struct {
			ID string `json:"id"`
		}{ID: runID}})
	}
	if len(run.Failure) == 0 {
		return nil, errors.New("failed child task has no failure")
	}
	failure, err := projectRunFailure(run.Failure)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		OK      bool                   `json:"ok"`
		Failure api.RunFailureResponse `json:"failure"`
		Run     struct {
			ID string `json:"id"`
		} `json:"run"`
	}{OK: false, Failure: failure, Run: struct {
		ID string `json:"id"`
	}{ID: runID}})
}
