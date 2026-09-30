package workergroup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pglock"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/region"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// GroupInput describes a worker group to create. Description is trimmed.
type GroupInput struct {
	RegionID    string
	Name        string
	Description string
}

// CreatedGroup is a new worker group with the raw enrollment token that is
// returned only once.
type CreatedGroup struct {
	Group           db.WorkerGroup
	EnrollmentToken string
}

// creationLockKey serializes worker group creation within one region, for
// administrators and the bootstrap seed alike.
func creationLockKey(regionID string) int64 {
	return pglock.Key("helmr:worker-group-create:" + regionID)
}

// ListGroups returns up to limit worker groups, only those of regionID when it
// is not empty.
func ListGroups(ctx context.Context, q db.Querier, regionID string, limit int32) ([]db.WorkerGroup, error) {
	var filter pgtype.Text
	if regionID != "" {
		if err := region.ValidateID(regionID); err != nil {
			return nil, InputError{message: err.Error()}
		}
		filter = pgtype.Text{String: regionID, Valid: true}
	}
	groups, err := q.ListWorkerGroups(ctx, db.ListWorkerGroupsParams{RegionID: filter, RowLimit: limit})
	if err != nil {
		return nil, fmt.Errorf("list worker groups: %w", err)
	}
	return groups, nil
}

// GetGroup returns one worker group.
func GetGroup(ctx context.Context, q db.Querier, groupID uuid.UUID) (db.WorkerGroup, error) {
	group, err := q.GetWorkerGroup(ctx, pgvalue.UUID(groupID))
	if errors.Is(err, pgx.ErrNoRows) {
		return db.WorkerGroup{}, ErrGroupNotFound
	}
	if err != nil {
		return db.WorkerGroup{}, fmt.Errorf("get worker group: %w", err)
	}
	return group, nil
}

// CreateGroup creates an active worker group with a new enrollment token in an
// existing region, serialized with other creators in that region.
func CreateGroup(ctx context.Context, txb db.TxBeginner, input GroupInput) (CreatedGroup, error) {
	if err := region.ValidateID(input.RegionID); err != nil {
		return CreatedGroup{}, InputError{message: err.Error()}
	}
	if err := ValidateName(input.Name); err != nil {
		return CreatedGroup{}, InputError{message: err.Error()}
	}
	description := strings.TrimSpace(input.Description)
	token, err := auth.GenerateEnrollmentToken()
	if err != nil {
		return CreatedGroup{}, err
	}
	var created db.WorkerGroup
	err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := q.LockWorkerGroupCreationRegion(ctx, creationLockKey(input.RegionID)); err != nil {
			return fmt.Errorf("lock worker group creation: %w", err)
		}
		if _, err := region.Get(ctx, q, input.RegionID); err != nil {
			return err
		}
		created, err = q.CreateWorkerGroup(ctx, db.CreateWorkerGroupParams{
			ID: pgvalue.UUID(uuid.NewV7()), TokenID: pgvalue.UUID(uuid.NewV7()), TokenHash: token.Hash,
			RegionID: input.RegionID, Name: input.Name, Description: description,
		})
		if db.IsUniqueViolation(err) {
			return conflict("worker group conflicts with an existing active role or name")
		}
		if err != nil {
			return fmt.Errorf("create worker group: %w", err)
		}
		return nil
	})
	if err != nil {
		return CreatedGroup{}, err
	}
	return CreatedGroup{Group: created, EnrollmentToken: token.Raw}, nil
}

// UpdateGroupDescription replaces a worker group's description.
func UpdateGroupDescription(ctx context.Context, q db.Querier, groupID uuid.UUID, description string) (db.WorkerGroup, error) {
	group, err := q.UpdateWorkerGroupDescription(ctx, db.UpdateWorkerGroupDescriptionParams{
		ID: pgvalue.UUID(groupID), Description: strings.TrimSpace(description),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.WorkerGroup{}, ErrGroupNotFound
	}
	if err != nil {
		return db.WorkerGroup{}, fmt.Errorf("update worker group: %w", err)
	}
	return group, nil
}

// RotateGroupToken replaces a worker group's enrollment token and returns the
// new raw token.
func RotateGroupToken(ctx context.Context, q db.Querier, groupID uuid.UUID) (string, error) {
	token, err := auth.GenerateEnrollmentToken()
	if err != nil {
		return "", err
	}
	_, err = q.RotateWorkerGroupToken(ctx, db.RotateWorkerGroupTokenParams{
		WorkerGroupID: pgvalue.UUID(groupID), TokenHash: token.Hash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrGroupNotFound
	}
	if err != nil {
		return "", fmt.Errorf("rotate worker group token: %w", err)
	}
	return token.Raw, nil
}
