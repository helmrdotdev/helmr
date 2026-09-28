package controlplane

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

var (
	errComputerNotFound      = errors.New("computer was not found")
	errComputerBusy          = errors.New("computer is busy")
	errComputerDeleteReceipt = errors.New("computer delete idempotency receipt is invalid")
)

type computerDeleteRequest struct {
	OrgID          uuid.UUID
	ProjectID      uuid.UUID
	EnvironmentID  uuid.UUID
	ComputerID     uuid.UUID
	IdempotencyKey string
	Authorize      func(context.Context, pgx.Tx) error
}

type computerDeleteResult struct {
	ComputerID uuid.UUID
	Replayed   bool
}

type computerDeleteReceipt struct {
	ComputerID string `json:"computerId"`
}

func (s *Server) deleteComputer(ctx context.Context, request computerDeleteRequest) (computerDeleteResult, error) {
	var result computerDeleteResult
	err := s.inTx(ctx, func(work *txWork) error {
		var claim *db.IdempotencyClaim
		if request.IdempotencyKey != "" {
			claimRequest, err := idempotency.NewComputerDeleteRequest(
				request.EnvironmentID,
				request.ComputerID,
				request.IdempotencyKey,
			)
			if err != nil {
				return err
			}
			claims, err := idempotency.TransactionForQueries(work.q)
			if err != nil {
				return err
			}
			acquired, err := claims.Acquire(ctx, claimRequest)
			if err != nil {
				return err
			}
			if acquired.Claim.Status == "completed" {
				if request.Authorize != nil {
					if err := request.Authorize(ctx, work.tx); err != nil {
						return err
					}
				}

				replayed, err := computerDeleteResultFromReceipt(acquired.Claim.Receipt)
				if err != nil {
					return err
				}
				replayed.Replayed = true
				result = replayed
				return nil
			}
			if acquired.Claim.Status != "pending" {
				return errComputerDeleteReceipt
			}
			claim = &acquired.Claim
		}
		if request.Authorize != nil {
			if err := request.Authorize(ctx, work.tx); err != nil {
				return err
			}
		}
		authority, err := work.q.LockComputerForDelete(ctx, db.LockComputerForDeleteParams{
			OrgID:         pgvalue.UUID(request.OrgID),
			ProjectID:     pgvalue.UUID(request.ProjectID),
			EnvironmentID: pgvalue.UUID(request.EnvironmentID),
			ID:            pgvalue.UUID(request.ComputerID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errComputerNotFound
		}
		if err != nil {
			return fmt.Errorf("lock computer for delete: %w", err)
		}
		computerID := pgvalue.MustUUIDValue(authority.ID)
		if authority.Status != db.ComputerStatusDeleting && authority.Status != db.ComputerStatusDeleted &&
			(!authority.HasMembers.Valid || authority.HasMembers.Bool) {
			return errComputerBusy
		}

		if authority.Status != db.ComputerStatusDeleting && authority.Status != db.ComputerStatusDeleted {
			if _, err := work.q.MarkComputerDeleting(ctx, db.MarkComputerDeletingParams{
				EnvironmentID:    pgvalue.UUID(request.EnvironmentID),
				ID:               authority.ID,
				ExpectedRevision: authority.Revision,
			}); errors.Is(err, pgx.ErrNoRows) {
				return errComputerBusy
			} else if err != nil {
				return fmt.Errorf("mark computer deleting: %w", err)
			}
		}
		// Deleting the Computer retires its memory-resume payloads, including consumed checkpoints. Their
		// artifacts are reclaimed by the existing checkpoint retention owner.
		if _, err := work.tx.Exec(ctx, `UPDATE computer_checkpoints SET status='invalid',invalidated_at=clock_timestamp(),invalidation_reason_code='computer_deleted'
 WHERE environment_id=$1 AND computer_id=$2 AND status IN ('creating','ready')`, authority.EnvironmentID, authority.ID); err != nil {
			return fmt.Errorf("retire deleting computer checkpoints: %w", err)
		}
		instance, err := work.q.LockComputerInstance(ctx, db.LockComputerInstanceParams{
			EnvironmentID: authority.EnvironmentID, ComputerID: authority.ID,
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("lock deleting computer instance: %w", err)
		}
		if err == nil && instance.DesiredState != "closed" {
			if _, err := work.q.RequestComputerInstanceClose(ctx, db.RequestComputerInstanceCloseParams{
				ID: instance.ID, WriterGeneration: instance.WriterGeneration,
				Reason: "computer_deleted", FinalizationAction: pgvalue.Text("discard"),
			}); err != nil {
				return fmt.Errorf("request computer delete cleanup: %w", err)
			}
		}
		result = computerDeleteResult{ComputerID: computerID}
		if claim != nil {
			receipt, err := json.Marshal(computerDeleteReceipt{ComputerID: computerID.String()})
			if err != nil {
				return err
			}
			claims, err := idempotency.TransactionForQueries(work.q)
			if err != nil {
				return err
			}
			if _, err := claims.Complete(ctx, *claim, receipt); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

func computerDeleteResultFromReceipt(raw []byte) (computerDeleteResult, error) {
	var receipt computerDeleteReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return computerDeleteResult{}, errComputerDeleteReceipt
	}
	computerID, err := ids.Parse(receipt.ComputerID)
	if err != nil {
		return computerDeleteResult{}, errComputerDeleteReceipt
	}
	return computerDeleteResult{ComputerID: computerID}, nil
}
