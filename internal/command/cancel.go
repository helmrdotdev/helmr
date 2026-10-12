package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

// CancelReceipt is the idempotency receipt of a Command cancellation: the
// claim that accepted it, the Command it targets and the accepted status.
type CancelReceipt struct {
	ID       string `json:"id"`
	TargetID string `json:"target_id"`
	Status   string `json:"status"`
}

// Cancel requests cancellation of the Command once: a pending Command is
// cancelled, an assigned one stops until its worker host reports the result,
// and a terminal one is unchanged. A repeated request replays the first
// receipt without changing the Command, also after its result was pruned.
// In one transaction Cancel reads the Command in its scope, acquires the
// idempotency claim and then updates the Command; it takes no Computer or
// Instance lock.
func Cancel(ctx context.Context, txb db.TxBeginner, ref Ref) (CancelReceipt, error) {
	var receipt CancelReceipt
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		var err error
		receipt, err = cancel(ctx, tx, ref)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return CancelReceipt{}, ErrNotFound
	}
	if err != nil {
		return CancelReceipt{}, err
	}
	return receipt, nil
}

func cancel(ctx context.Context, tx pgx.Tx, ref Ref) (CancelReceipt, error) {
	q := db.New(tx)
	if _, err := q.GetCommand(ctx, ref.params()); err != nil {
		return CancelReceipt{}, err
	}
	request, err := idempotency.NewCommandCancelRequest(ref.EnvironmentID, ref.CommandID)
	if err != nil {
		return CancelReceipt{}, err
	}
	var result CancelReceipt
	err = idempotency.Run(ctx, tx, request, func(tx pgx.Tx, acquired idempotency.Result) (idempotency.Completion, error) {
		q := db.New(tx)
		want := CancelReceipt{ID: pgvalue.UUIDString(acquired.Claim.ID), TargetID: ref.CommandID.String(), Status: "accepted"}
		if !acquired.New {
			var replayed CancelReceipt
			if err := json.Unmarshal(acquired.Claim.Receipt, &replayed); err != nil {
				return idempotency.Completion{}, fmt.Errorf("%w: %v", ErrReceiptInvalid, err)
			}
			if replayed != want {
				return idempotency.Completion{}, fmt.Errorf("%w: command cancellation receipt differs from its claim", ErrReceiptInvalid)
			}
			result = replayed
			return idempotency.Completion{}, nil
		}
		if _, err := q.RequestCommandCancellation(ctx, db.RequestCommandCancellationParams{
			EnvironmentID: pgvalue.UUID(ref.EnvironmentID), CommandID: pgvalue.UUID(ref.CommandID),
		}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return idempotency.Completion{}, err
		}
		body, err := json.Marshal(want)
		if err != nil {
			return idempotency.Completion{}, err
		}

		result = want
		return idempotency.Completion{Target: idempotency.Target{CommandID: ref.CommandID}, Receipt: body}, nil
	})
	if err != nil {
		return CancelReceipt{}, err
	}
	return result, nil
}
