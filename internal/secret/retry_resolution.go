package secret

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

type retryResolutionStore interface {
	LockAttemptSecretResolutionMetadata(
		context.Context,
		db.LockAttemptSecretResolutionMetadataParams,
	) ([]db.LockAttemptSecretResolutionMetadataRow, error)
}

// LockAttemptRetryResolutions locks only Secret identity/version metadata and
// projects the current active versions for a replacement Attempt. It never
// loads encrypted Secret payloads into dispatcher memory.
func LockAttemptRetryResolutions(
	ctx context.Context,
	store retryResolutionStore,
	runID pgtype.UUID,
	attemptNumber int32,
	workspaceID pgtype.UUID,
) ([]Resolution, bool, error) {
	rows, err := store.LockAttemptSecretResolutionMetadata(
		ctx,
		db.LockAttemptSecretResolutionMetadataParams{
			RunID:         runID,
			AttemptNumber: pgtype.Int4{Int32: attemptNumber, Valid: true},
			WorkspaceID:   workspaceID,
		},
	)
	if err != nil {
		return nil, false, err
	}
	if len(rows) > maxResolutions {
		return nil, false, nil
	}
	resolutions := make([]Resolution, 0, len(rows))
	for _, row := range rows {
		if row.PlacementKind == "" || row.PlacementTarget == "" ||
			!row.SecretID.Valid || row.SecretState != "active" ||
			!row.CurrentVersionID.Valid || !row.ResolutionID.Valid ||
			row.ResolutionRunID != runID ||
			!row.ResolutionAttemptNumber.Valid ||
			row.ResolutionAttemptNumber.Int32 != attemptNumber ||
			!row.ResolutionSecretVersionID.Valid ||
			!row.ResolutionRevocationGeneration.Valid ||
			row.ResolutionRevocationGeneration.Int64 != row.RevocationGeneration {
			return nil, false, nil
		}
		resolutions = append(resolutions, Resolution{
			PlacementKind:        row.PlacementKind,
			PlacementTarget:      row.PlacementTarget,
			SecretID:             row.SecretID,
			SecretVersionID:      row.CurrentVersionID,
			RevocationGeneration: row.RevocationGeneration,
		})
	}
	return resolutions, true, nil
}
