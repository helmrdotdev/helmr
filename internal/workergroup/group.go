package workergroup

import (
	"context"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dbnotify"
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

// ListOrganizationGroups returns up to limit of the worker groups orgID owns,
// only those of regionID when it is not empty.
func ListOrganizationGroups(ctx context.Context, q db.Querier, orgID uuid.UUID, regionID string, limit int32) ([]db.WorkerGroup, error) {
	var filter pgtype.Text
	if regionID != "" {
		if err := region.ValidateID(regionID); err != nil {
			return nil, InputError{message: err.Error()}
		}
		filter = pgtype.Text{String: regionID, Valid: true}
	}
	groups, err := q.ListOrganizationWorkerGroups(ctx, db.ListOrganizationWorkerGroupsParams{OwnerOrgID: pgvalue.UUID(orgID), RegionID: filter, RowLimit: limit})
	if err != nil {
		return nil, fmt.Errorf("list organization worker groups: %w", err)
	}
	return groups, nil
}

// GetOrganizationGroup returns the worker group with the ID that orgID owns.
// Any other group reads exactly like a missing one.
func GetOrganizationGroup(ctx context.Context, q db.Querier, orgID, groupID uuid.UUID) (db.WorkerGroup, error) {
	group, err := GetGroup(ctx, q, groupID)
	if err != nil {
		return db.WorkerGroup{}, err
	}
	if !group.OwnerOrgID.Valid || uuid.UUID(group.OwnerOrgID.Bytes) != orgID {
		return db.WorkerGroup{}, ErrGroupNotFound
	}
	return group, nil
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
// existing region, serialized with other creators in that region. An
// organization operator creates a group its organization owns for life.
func CreateGroup(ctx context.Context, txb db.TxBeginner, policy auditpolicy.Policy, operator Operator, input GroupInput) (CreatedGroup, error) {
	if operator.safety {
		return CreatedGroup{}, errSafetyOperation
	}
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
		actor, err := operator.authorize(ctx, tx)
		if err != nil {
			return err
		}
		if err := q.LockWorkerGroupCreationRegion(ctx, creationLockKey(input.RegionID)); err != nil {
			return fmt.Errorf("lock worker group creation: %w", err)
		}
		if _, err := region.Get(ctx, q, input.RegionID); err != nil {
			return err
		}
		params := db.CreateWorkerGroupParams{
			ID: pgvalue.UUID(uuid.NewV7()), TokenID: pgvalue.UUID(uuid.NewV7()), TokenHash: token.Hash,
			RegionID: input.RegionID, Name: input.Name, Description: description,
		}
		if operator.orgID != uuid.Nil() {
			params.OwnerOrgID = pgvalue.UUID(operator.orgID)
		}
		created, err = q.CreateWorkerGroup(ctx, params)
		if db.IsUniqueViolation(err) {
			return conflict("worker group conflicts with an existing active role or name")
		}
		if err != nil {
			return fmt.Errorf("create worker group: %w", err)
		}
		if err := audit.Append(ctx, tx, policy, audit.Event{Evidence: actor, OrgID: operator.orgID, Action: "worker_group.created", TargetKind: "worker_group", TargetID: pgvalue.MustUUIDValue(created.ID)}); err != nil {
			return err
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
// new raw token. The previous token stops enrolling hosts at once. Hosts that
// already enrolled keep their host secrets and keep running; revoke them with
// RevokeGroupHostCredentials.
func RotateGroupToken(ctx context.Context, database db.TxBeginner, policy auditpolicy.Policy, operator Operator, groupID uuid.UUID) (string, error) {
	if operator.safety {
		return "", errSafetyOperation
	}
	token, err := auth.GenerateEnrollmentToken()
	if err != nil {
		return "", err
	}
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		q := db.New(tx)
		actor, owner, err := operator.admit(ctx, tx, groupID)
		if err != nil {
			return err
		}
		_, err = q.RotateWorkerGroupToken(ctx, db.RotateWorkerGroupTokenParams{
			WorkerGroupID: pgvalue.UUID(groupID), TokenHash: token.Hash,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrGroupNotFound
		}
		if err != nil {
			return fmt.Errorf("rotate worker group token: %w", err)
		}
		return audit.Append(ctx, tx, policy, audit.Event{Evidence: actor, OrgID: owner, Action: "worker_group.token_rotated", TargetKind: "worker_group", TargetID: groupID})
	})
	if err != nil {
		return "", err
	}

	return token.Raw, nil
}

// RevokeGroupHostCredentials fences every registering, active and draining
// host of a worker group as lost and revokes every host secret the group has
// issued, then returns the fenced host IDs. Each host's claim version
// advances, so every later authorization check of their host credentials,
// allocations and Secret proxy grants fails; a request whose authorization
// committed before the revocation may still complete. Their allocations are
// reconciled as host loss. A fenced machine can return only by enrolling again with the
// group's enrollment token, which this operation does not change.
func RevokeGroupHostCredentials(ctx context.Context, database db.TxBeginner, policy auditpolicy.Policy, operator Operator, groupID uuid.UUID) ([]uuid.UUID, error) {
	var fenced []uuid.UUID
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		q := db.New(tx)
		actor, owner, err := operator.admit(ctx, tx, groupID)
		if err != nil {
			return err
		}
		if err := q.LockWorkerGroupMutation(ctx, statusMutationLockKey(groupID)); err != nil {
			return fmt.Errorf("lock worker group lifecycle: %w", err)
		}
		if _, err := q.LockWorkerGroupForPoolMutation(ctx, pgvalue.UUID(groupID)); errors.Is(err, pgx.ErrNoRows) {
			return ErrGroupNotFound
		} else if err != nil {
			return fmt.Errorf("lock worker group: %w", err)
		}
		rows, err := q.RevokeWorkerGroupHostCredentials(ctx, pgvalue.UUID(groupID))
		if err != nil {
			return fmt.Errorf("revoke worker group host credentials: %w", err)
		}
		fenced = make([]uuid.UUID, 0, len(rows))
		for _, row := range rows {
			hostID := pgvalue.MustUUIDValue(row)
			fenced = append(fenced, hostID)
			if err := audit.Append(ctx, tx, policy, audit.Event{Evidence: actor, OrgID: owner, Action: "worker_host.marked_lost", TargetKind: "worker_host", TargetID: hostID}); err != nil {
				return err
			}
		}
		if err := audit.Append(ctx, tx, policy, audit.Event{Evidence: actor, OrgID: owner, Action: "worker_group.host_credentials_revoked", TargetKind: "worker_group", TargetID: groupID}); err != nil {
			return err
		}
		return dbnotify.Notify(ctx, tx, "dispatch", "capacity")
	})
	if err != nil {
		return nil, err
	}
	return fenced, nil
}
