package secret

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

const maxResolutions = 64

type Resolution struct {
	PlacementKind        string
	PlacementTarget      string
	SecretID             pgtype.UUID
	SecretVersionID      pgtype.UUID
	RevocationGeneration int64
}

type AttemptResolutionStore interface {
	CreateAttemptSecretResolutions(context.Context, db.CreateAttemptSecretResolutionsParams) (int64, error)
}

type ProcessResolutionStore interface {
	CreateProcessSecretResolutions(context.Context, db.CreateProcessSecretResolutionsParams) (int64, error)
}

func CreateAttemptResolutions(
	ctx context.Context,
	store AttemptResolutionStore,
	workspaceID pgtype.UUID,
	runID pgtype.UUID,
	attemptNumber int32,
	resolutions []Resolution,
) error {
	if len(resolutions) == 0 {
		return nil
	}
	if len(resolutions) > maxResolutions {
		return fmt.Errorf("Secret resolution count %d exceeds %d", len(resolutions), maxResolutions)
	}
	ids, kinds, targets, secretIDs, versionIDs, generations := resolutionColumns(resolutions)
	count, err := store.CreateAttemptSecretResolutions(ctx, db.CreateAttemptSecretResolutionsParams{
		WorkspaceID: workspaceID, RunID: runID,
		AttemptNumber: pgtype.Int4{Int32: attemptNumber, Valid: true},
		Ids:           ids, PlacementKinds: kinds, PlacementTargets: targets,
		SecretIds: secretIDs, SecretVersionIds: versionIDs,
		RevocationGenerations: generations,
	})
	return checkResolutionInsert(count, len(resolutions), err)
}

func CreateProcessResolutions(
	ctx context.Context,
	store ProcessResolutionStore,
	workspaceID pgtype.UUID,
	processID pgtype.UUID,
	resolutions []Resolution,
) error {
	if len(resolutions) == 0 {
		return nil
	}
	if len(resolutions) > maxResolutions {
		return fmt.Errorf("Secret resolution count %d exceeds %d", len(resolutions), maxResolutions)
	}
	ids, kinds, targets, secretIDs, versionIDs, generations := resolutionColumns(resolutions)
	count, err := store.CreateProcessSecretResolutions(ctx, db.CreateProcessSecretResolutionsParams{
		WorkspaceID: workspaceID, ProcessID: processID,
		Ids: ids, PlacementKinds: kinds, PlacementTargets: targets,
		SecretIds: secretIDs, SecretVersionIds: versionIDs,
		RevocationGenerations: generations,
	})
	return checkResolutionInsert(count, len(resolutions), err)
}

func resolutionColumns(resolutions []Resolution) (
	[]pgtype.UUID,
	[]string,
	[]string,
	[]pgtype.UUID,
	[]pgtype.UUID,
	[]int64,
) {
	ids := make([]pgtype.UUID, len(resolutions))
	kinds := make([]string, len(resolutions))
	targets := make([]string, len(resolutions))
	secretIDs := make([]pgtype.UUID, len(resolutions))
	versionIDs := make([]pgtype.UUID, len(resolutions))
	generations := make([]int64, len(resolutions))
	for index, resolution := range resolutions {
		ids[index] = pgvalue.UUID(uuid.Must(uuid.NewV7()))
		kinds[index] = resolution.PlacementKind
		targets[index] = resolution.PlacementTarget
		secretIDs[index] = resolution.SecretID
		versionIDs[index] = resolution.SecretVersionID
		generations[index] = resolution.RevocationGeneration
	}
	return ids, kinds, targets, secretIDs, versionIDs, generations
}

func checkResolutionInsert(count int64, expected int, err error) error {
	if err != nil {
		return err
	}
	if count != int64(expected) {
		return errors.New("Secret resolution batch was rejected")
	}
	return nil
}
