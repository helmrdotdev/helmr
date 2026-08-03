package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/actor"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

var (
	errActorCloseConflict  = errors.New("Actor cannot be closed in its current state")
	errActorCloseAuthority = errors.New("Actor close authority is unavailable")
	errActorCloseReceipt   = errors.New("Actor close receipt is invalid")
)

type actorCloseRequest struct {
	EnvironmentID  uuid.UUID
	ActorID        uuid.UUID
	WorkspaceID    uuid.UUID
	IdempotencyKey string
	Authorize      func(context.Context, db.Querier) error
}

func (s *Server) closeActor(
	ctx context.Context,
	request actorCloseRequest,
) (api.ActorOperationReceipt, error) {
	var claimRequest idempotency.Request
	var err error
	if request.IdempotencyKey != "" {
		claimRequest, err = idempotency.NewActorCloseRequest(
			request.EnvironmentID,
			request.ActorID,
			request.IdempotencyKey,
		)
		if err != nil {
			return api.ActorOperationReceipt{}, fmt.Errorf("%w: %v", errActorCloseAuthority, err)
		}
	}

	var receipt api.ActorOperationReceipt
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
				if replayed.ActorID != request.ActorID.String() {
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
			return fmt.Errorf("lock Actor close Workspace Secrets: %w", err)
		}
		lockedActor, err := work.q.LockActorClose(ctx, db.LockActorCloseParams{
			EnvironmentID: pgvalue.UUID(request.EnvironmentID),
			ActorID:       pgvalue.UUID(request.ActorID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errActorCloseAuthority
		}
		if err != nil {
			return fmt.Errorf("lock Actor close authority: %w", err)
		}
		if lockedActor.WorkspaceID != pgvalue.UUID(request.WorkspaceID) {
			return errActorCloseAuthority
		}

		switch lockedActor.State {
		case "open", "closing":
			lockedActor, err = work.q.BeginActorClose(ctx, db.BeginActorCloseParams{
				EnvironmentID: lockedActor.EnvironmentID,
				ActorID:       lockedActor.ID,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return errActorCloseConflict
			}
			if err != nil {
				return fmt.Errorf("begin Actor close: %w", err)
			}
			var deferred bool
			lockedActor, deferred, err = actor.ReconcileClose(ctx, work.q, lockedActor, bindings)
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
		receipt = api.ActorOperationReceipt{
			ActorID:    pgvalue.UUIDString(lockedActor.ID),
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

func actorCloseReceipt(raw []byte) (api.ActorOperationReceipt, error) {
	var receipt api.ActorOperationReceipt
	if err := decodeClosedJSON(raw, &receipt); err != nil {
		return api.ActorOperationReceipt{}, errActorCloseReceipt
	}
	if err := ids.Validate(receipt.ActorID); err != nil ||
		receipt.AcceptedAt.IsZero() {
		return api.ActorOperationReceipt{}, errActorCloseReceipt
	}
	receipt.AcceptedAt = receipt.AcceptedAt.UTC()
	return receipt, nil
}

func createActorCloseReconcileIntent(
	ctx context.Context,
	store db.Querier,
	actor db.Actor,
) error {
	if !actor.CloseSequence.Valid {
		return errActorCloseAuthority
	}
	if err := store.CreateActorCloseReconcileOutbox(
		ctx,
		db.CreateActorCloseReconcileOutboxParams{
			ID:            actor.ID,
			ActorID:       actor.ID,
			EnvironmentID: actor.EnvironmentID,
		},
	); err != nil {
		return fmt.Errorf("enqueue Actor close reconciliation: %w", err)
	}
	return nil
}
