package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func checkpointCorrelationID(
	checkpoint db.ComputerCheckpoint,
	wait db.RunWait,
) (string, error) {
	var manifest workerapi.CheckpointManifest
	if err := json.Unmarshal(checkpoint.Manifest, &manifest); err != nil {
		return "", fmt.Errorf("decode run checkpoint correlation authority: %w", err)
	}
	if manifest.RecoveryPoint.ID != pgvalue.UUIDString(checkpoint.ID) {
		return "", errors.New("run checkpoint correlation authority is inconsistent")
	}
	correlationID := ""
	for _, member := range manifest.RecoveryPoint.Runs {
		if member.RunID != pgvalue.UUIDString(wait.RunID) {
			continue
		}
		if correlationID != "" || member.AttemptNumber != wait.AttemptNumber || member.RunWaitID != pgvalue.UUIDString(wait.ID) || strings.TrimSpace(member.CorrelationID) == "" {
			return "", errors.New("run checkpoint correlation member is inconsistent")
		}
		correlationID = member.CorrelationID
	}
	if correlationID == "" {
		return "", errors.New("run checkpoint correlation member is missing")
	}
	return correlationID, nil

}
