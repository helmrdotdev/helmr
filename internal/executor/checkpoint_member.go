package executor

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func validateRestoredCheckpointMember(point workerapi.CheckpointRecoveryPoint, runID string, attempt int32, waitID, correlationID string) error {
	matched := false
	for _, member := range point.Runs {
		if member.RunID != runID {
			continue
		}
		if matched || member.AttemptNumber != attempt || member.RunWaitID != waitID || member.CorrelationID != correlationID {
			return errors.New("restored program checkpoint member is inconsistent")
		}
		matched = true
	}
	if !matched {
		return errors.New("restored program is absent from checkpoint membership")
	}
	return nil
}
