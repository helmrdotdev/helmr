package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

var (
	errWorkspaceNotFound      = errors.New("Workspace was not found")
	errWorkspaceBusy          = errors.New("Workspace is busy")
	errWorkspaceDeleteReceipt = errors.New("Workspace delete idempotency receipt is invalid")
)

type workspaceDeleteRequest struct {
	OrgID          uuid.UUID
	ProjectID      uuid.UUID
	EnvironmentID  uuid.UUID
	WorkspaceID    uuid.UUID
	IdempotencyKey string
	Authorize      func(context.Context, db.Querier) error
}

type workspaceDeleteResult struct {
	WorkspaceID uuid.UUID
	Replayed    bool
}

type workspaceDeleteReceipt struct {
	WorkspaceID string `json:"workspaceId"`
}

func (s *Server) deleteWorkspace(ctx context.Context, request workspaceDeleteRequest) (workspaceDeleteResult, error) {
	var result workspaceDeleteResult
	err := s.inTx(ctx, func(work *txWork) error {
		if request.Authorize != nil {
			if err := request.Authorize(ctx, work.q); err != nil {
				return err
			}
		}
		authority, err := work.q.LockWorkspaceForDelete(ctx, db.LockWorkspaceForDeleteParams{
			OrgID:         pgvalue.UUID(request.OrgID),
			ProjectID:     pgvalue.UUID(request.ProjectID),
			EnvironmentID: pgvalue.UUID(request.EnvironmentID),
			ID:            pgvalue.UUID(request.WorkspaceID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errWorkspaceNotFound
		}
		if err != nil {
			return fmt.Errorf("lock Workspace for delete: %w", err)
		}
		workspaceID := pgvalue.MustUUIDValue(authority.ID)
		if authority.State != db.WorkspaceStateDeleting &&
			(authority.OwnerActorID.Valid || authority.OwnerRunID.Valid ||
				authority.HasActiveLease || authority.HasActiveProcess) {
			return errWorkspaceBusy
		}

		var claim *db.IdempotencyClaim
		if request.IdempotencyKey != "" {
			claimRequest, err := idempotency.NewWorkspaceDeleteRequest(
				request.EnvironmentID,
				workspaceID,
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
			if acquired.Claim.State == "completed" {
				replayed, err := workspaceDeleteResultFromReceipt(acquired.Claim.Receipt)
				if err != nil {
					return err
				}
				replayed.Replayed = true
				result = replayed
				return nil
			}
			if acquired.Claim.State != "pending" {
				return errWorkspaceDeleteReceipt
			}
			claim = &acquired.Claim
		}

		if authority.State != db.WorkspaceStateDeleting {
			if _, err := work.q.MarkWorkspaceDeleting(ctx, db.MarkWorkspaceDeletingParams{
				EnvironmentID:        pgvalue.UUID(request.EnvironmentID),
				ID:                   authority.ID,
				ExpectedStateVersion: authority.StateVersion,
			}); errors.Is(err, pgx.ErrNoRows) {
				return errWorkspaceBusy
			} else if err != nil {
				return fmt.Errorf("mark Workspace deleting: %w", err)
			}
		}
		result = workspaceDeleteResult{WorkspaceID: workspaceID}
		if claim != nil {
			receipt, err := json.Marshal(workspaceDeleteReceipt{WorkspaceID: workspaceID.String()})
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

func workspaceDeleteResultFromReceipt(raw []byte) (workspaceDeleteResult, error) {
	var receipt workspaceDeleteReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return workspaceDeleteResult{}, errWorkspaceDeleteReceipt
	}
	workspaceID, err := ids.Parse(receipt.WorkspaceID)
	if err != nil {
		return workspaceDeleteResult{}, errWorkspaceDeleteReceipt
	}
	return workspaceDeleteResult{WorkspaceID: workspaceID}, nil
}
