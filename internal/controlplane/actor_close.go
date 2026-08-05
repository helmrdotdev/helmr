package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

var (
	errActorCloseConflict  = errors.New("actor cannot be closed in its current state")
	errActorCloseAuthority = errors.New("actor close authority is unavailable")
	errActorCloseReceipt   = errors.New("actor close receipt is invalid")
)

type actorCloseRequest struct {
	EnvironmentID  uuid.UUID
	SessionID      uuid.UUID
	WorkspaceID    uuid.UUID
	IdempotencyKey string
	Authorize      func(context.Context, db.Querier) error
}

func (s *Server) closeActor(
	ctx context.Context,
	request actorCloseRequest,
) (api.SessionCloseReceipt, error) {
	var claimRequest idempotency.Request
	var err error
	if request.IdempotencyKey != "" {
		claimRequest, err = idempotency.NewActorCloseRequest(
			request.EnvironmentID,
			request.SessionID,
			request.IdempotencyKey,
		)
		if err != nil {
			return api.SessionCloseReceipt{}, fmt.Errorf("%w: %v", errActorCloseAuthority, err)
		}
	}

	var receipt api.SessionCloseReceipt
	err = s.inTx(ctx, func(work *txWork) error {
		if request.Authorize != nil {
			if err := request.Authorize(ctx, work.q); err != nil {
				return err
			}
		}
		var claim *db.IdempotencyClaim
		if claimRequest != nil {
			claims, err := idempotency.TransactionForQueries(work.q)
			if err != nil {
				return err
			}
			acquired, err := claims.Acquire(ctx, claimRequest)
			if err != nil {
				return err
			}
			if acquired.Claim.State == "completed" {
				replayed, err := actorCloseReceipt(acquired.Claim.Receipt)
				if err != nil {
					return err
				}
				if replayed.SessionID != request.SessionID.String() {
					return errActorCloseReceipt
				}
				receipt = replayed
				return nil
			}
			if acquired.Claim.State != "pending" {
				return errActorCloseReceipt
			}
			claim = &acquired.Claim
		}

		bindings, err := work.q.LockWorkspaceSecretsForAdmission(
			ctx,
			pgvalue.UUID(request.WorkspaceID),
		)
		if err != nil {
			return fmt.Errorf("lock actor close workspace secrets: %w", err)
		}
		lockedActor, err := work.q.LockActorClose(ctx, db.LockActorCloseParams{
			EnvironmentID: pgvalue.UUID(request.EnvironmentID),
			SessionID:     pgvalue.UUID(request.SessionID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errActorCloseAuthority
		}
		if err != nil {
			return fmt.Errorf("lock actor close authority: %w", err)
		}
		if lockedActor.WorkspaceID != pgvalue.UUID(request.WorkspaceID) {
			return errActorCloseAuthority
		}

		switch lockedActor.State {
		case "open", "closing":
			lockedActor, err = work.q.BeginActorClose(ctx, db.BeginActorCloseParams{
				EnvironmentID: lockedActor.EnvironmentID,
				SessionID:     lockedActor.ID,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return errActorCloseConflict
			}
			if err != nil {
				return fmt.Errorf("begin actor close: %w", err)
			}
			var deferred bool
			lockedActor, deferred, err = session.ReconcileClose(ctx, work.q, lockedActor, bindings)
			if err != nil {
				return err
			}
			if deferred || lockedActor.State == "closing" {
				if err := createActorCloseReconcileIntent(ctx, work.q, lockedActor); err != nil {
					return err
				}
			}
		case "closed":
		case "failed", "cancelling", "cancelled":
			return errActorCloseConflict
		default:
			return errActorCloseAuthority
		}

		acceptedAt, err := actorCloseAcceptedAt(ctx, work.q, claim)
		if err != nil {
			return err
		}
		receipt = api.SessionCloseReceipt{
			SessionID:  pgvalue.UUIDString(lockedActor.ID),
			AcceptedAt: acceptedAt,
		}
		if claim != nil {
			encoded, err := json.Marshal(receipt)
			if err != nil {
				return err
			}
			claims, err := idempotency.TransactionForQueries(work.q)
			if err != nil {
				return err
			}
			if _, err := claims.Complete(ctx, *claim, encoded); err != nil {
				return err
			}
		}
		return nil
	})
	return receipt, err
}

func actorCloseAcceptedAt(
	ctx context.Context,
	store db.Querier,
	claim *db.IdempotencyClaim,
) (time.Time, error) {
	if claim != nil && claim.AcceptedAt.Valid {
		return claim.AcceptedAt.Time.UTC(), nil
	}
	now, err := store.GetRunLeaseRenewalTime(ctx)
	if err != nil || !now.Valid {
		if err == nil {
			err = errActorCloseAuthority
		}
		return time.Time{}, err
	}
	return now.Time.UTC(), nil
}

func actorCloseReceipt(raw []byte) (api.SessionCloseReceipt, error) {
	var receipt api.SessionCloseReceipt
	if err := decodeClosedJSON(raw, &receipt); err != nil {
		return api.SessionCloseReceipt{}, errActorCloseReceipt
	}
	if err := ids.Validate(receipt.SessionID); err != nil ||
		receipt.AcceptedAt.IsZero() {
		return api.SessionCloseReceipt{}, errActorCloseReceipt
	}
	receipt.AcceptedAt = receipt.AcceptedAt.UTC()
	return receipt, nil
}

func createActorCloseReconcileIntent(
	ctx context.Context,
	store db.Querier,
	actor db.Session,
) error {
	if !actor.CloseSequence.Valid {
		return errActorCloseAuthority
	}
	if err := store.CreateActorCloseReconcileOutbox(
		ctx,
		db.CreateActorCloseReconcileOutboxParams{
			ID:            actor.ID,
			SessionID:     actor.ID,
			EnvironmentID: actor.EnvironmentID,
		},
	); err != nil {
		return fmt.Errorf("enqueue actor close reconciliation: %w", err)
	}
	return nil
}
