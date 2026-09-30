package computer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

// Deletion requests deletion of one Computer.
type Deletion struct {
	Scope          Scope
	ComputerID     uuid.UUID
	IdempotencyKey string
}

// Deleted is an accepted or replayed deletion.
type Deleted struct {
	ComputerID uuid.UUID
	Replayed   bool
}

type deleteReceipt struct {
	ComputerID string `json:"computerId"`
}

// Delete deletes a Computer in one transaction: ClaimDeletion, then Apply
// unless the claim replays a completed deletion.
func Delete(ctx context.Context, txb db.TxBeginner, deletion Deletion) (Deleted, error) {
	var deleted Deleted
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		claim, err := ClaimDeletion(ctx, tx, deletion)
		if err != nil {
			return err
		}
		if claim.Replayed() {
			deleted, err = claim.Receipt()
			return err
		}
		deleted, err = claim.Apply(ctx, tx)
		return err
	})
	return deleted, err
}

// DeletionClaim is a deletion whose idempotency claim, if it has a key, is
// held in the caller's transaction.
type DeletionClaim struct {
	deletion Deletion
	pending  *db.IdempotencyClaim
	receipt  []byte
	replayed bool
}

// ClaimDeletion acquires the deletion's idempotency claim in the caller's
// transaction before any Computer lock. A deletion without an idempotency
// key takes no claim.
func ClaimDeletion(ctx context.Context, tx pgx.Tx, deletion Deletion) (DeletionClaim, error) {
	claim := DeletionClaim{deletion: deletion}
	if deletion.IdempotencyKey == "" {
		return claim, nil
	}
	request, err := idempotency.NewComputerDeleteRequest(
		deletion.Scope.EnvironmentID, deletion.ComputerID, deletion.IdempotencyKey,
	)
	if err != nil {
		return DeletionClaim{}, err
	}
	claims, err := idempotency.TransactionFor(tx)
	if err != nil {
		return DeletionClaim{}, err
	}
	acquired, err := claims.Acquire(ctx, request)
	if err != nil {
		return DeletionClaim{}, err
	}
	switch acquired.Claim.Status {
	case "completed":
		claim.replayed = true
		claim.receipt = acquired.Claim.Receipt
	case "pending":
		claim.pending = &acquired.Claim
	default:
		return DeletionClaim{}, ErrReceiptInvalid
	}
	return claim, nil
}

// Replayed reports that the claim holds a completed deletion's receipt.
func (c DeletionClaim) Replayed() bool {
	return c.replayed
}

// Receipt decodes the completed deletion a replayed claim holds.
func (c DeletionClaim) Receipt() (Deleted, error) {
	if !c.replayed {
		return Deleted{}, ErrReceiptInvalid
	}
	var receipt deleteReceipt
	if err := json.Unmarshal(c.receipt, &receipt); err != nil {
		return Deleted{}, ErrReceiptInvalid
	}
	computerID, err := ids.Parse(receipt.ComputerID)
	if err != nil {
		return Deleted{}, ErrReceiptInvalid
	}
	return Deleted{ComputerID: computerID, Replayed: true}, nil
}

// Apply deletes the Computer under its lock and completes the claim. With the
// Computer locked it marks the Computer deleting, invalidates its creating
// and ready checkpoints, and only then locks its Instance to request close:
// checkpoint rows precede the Instance under the Computer lock.
func (c DeletionClaim) Apply(ctx context.Context, tx pgx.Tx) (Deleted, error) {
	if c.replayed {
		return Deleted{}, ErrReceiptInvalid
	}
	q := db.New(tx)
	deletion := c.deletion
	authority, err := q.LockComputerForDelete(ctx, db.LockComputerForDeleteParams{
		OrgID:         pgvalue.UUID(deletion.Scope.OrgID),
		ProjectID:     pgvalue.UUID(deletion.Scope.ProjectID),
		EnvironmentID: pgvalue.UUID(deletion.Scope.EnvironmentID),
		ID:            pgvalue.UUID(deletion.ComputerID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Deleted{}, ErrNotFound
	}
	if err != nil {
		return Deleted{}, fmt.Errorf("lock computer for delete: %w", err)
	}
	computerID := pgvalue.MustUUIDValue(authority.ID)
	deleting := authority.Status == db.ComputerStatusDeleting || authority.Status == db.ComputerStatusDeleted
	if !deleting && (!authority.HasMembers.Valid || authority.HasMembers.Bool) {
		return Deleted{}, ErrBusy
	}
	if !deleting {
		if _, err := q.MarkComputerDeleting(ctx, db.MarkComputerDeletingParams{
			EnvironmentID:    pgvalue.UUID(deletion.Scope.EnvironmentID),
			ID:               authority.ID,
			ExpectedRevision: authority.Revision,
		}); errors.Is(err, pgx.ErrNoRows) {
			return Deleted{}, ErrBusy
		} else if err != nil {
			return Deleted{}, fmt.Errorf("mark computer deleting: %w", err)
		}
	}
	// Deleting the Computer retires its memory-resume payloads, including consumed checkpoints. Their
	// artifacts are reclaimed by the existing checkpoint retention owner.
	if _, err := tx.Exec(ctx, `UPDATE computer_checkpoints SET status='invalid',invalidated_at=clock_timestamp(),invalidation_reason_code='computer_deleted'
 WHERE environment_id=$1 AND computer_id=$2 AND status IN ('creating','ready')`, authority.EnvironmentID, authority.ID); err != nil {
		return Deleted{}, fmt.Errorf("retire deleting computer checkpoints: %w", err)
	}
	instance, err := q.LockComputerInstance(ctx, db.LockComputerInstanceParams{
		EnvironmentID: authority.EnvironmentID, ComputerID: authority.ID,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Deleted{}, fmt.Errorf("lock deleting computer instance: %w", err)
	}
	if err == nil && instance.DesiredState != "closed" {
		if _, err := q.RequestComputerInstanceClose(ctx, db.RequestComputerInstanceCloseParams{
			ID: instance.ID, WriterGeneration: instance.WriterGeneration,
			Reason: "computer_deleted", FinalizationAction: pgvalue.Text("discard"),
		}); err != nil {
			return Deleted{}, fmt.Errorf("request computer delete cleanup: %w", err)
		}
	}
	if c.pending != nil {
		receipt, err := json.Marshal(deleteReceipt{ComputerID: computerID.String()})
		if err != nil {
			return Deleted{}, err
		}
		claims, err := idempotency.TransactionFor(tx)
		if err != nil {
			return Deleted{}, err
		}
		if _, err := claims.Complete(ctx, *c.pending, receipt); err != nil {
			return Deleted{}, err
		}
	}
	return Deleted{ComputerID: computerID}, nil
}
