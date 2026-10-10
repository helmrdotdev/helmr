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
	claims, err := idempotency.TransactionFor(tx)
	if err != nil {
		return CancelReceipt{}, err
	}
	acquired, err := claims.Acquire(ctx, request)
	if err != nil {
		return CancelReceipt{}, err
	}
	want := CancelReceipt{ID: pgvalue.UUIDString(acquired.Claim.ID), TargetID: ref.CommandID.String(), Status: "accepted"}
	if !acquired.New {
		var replayed CancelReceipt
		if err := json.Unmarshal(acquired.Claim.Receipt, &replayed); err != nil {
			return CancelReceipt{}, fmt.Errorf("%w: %v", ErrReceiptInvalid, err)
		}
		if replayed != want {
			return CancelReceipt{}, fmt.Errorf("%w: command cancellation receipt differs from its claim", ErrReceiptInvalid)
		}
		return replayed, nil
	}
	if _, err := q.RequestComputerCommandCancellation(ctx, db.RequestComputerCommandCancellationParams{
		EnvironmentID: pgvalue.UUID(ref.EnvironmentID), CommandID: pgvalue.UUID(ref.CommandID),
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return CancelReceipt{}, err
	}
	body, err := json.Marshal(want)
	if err != nil {
		return CancelReceipt{}, err
	}
	if _, err := claims.Complete(ctx, acquired.Claim, idempotency.Target{CommandID: ref.CommandID}, body); err != nil {
		return CancelReceipt{}, err
	}
	return want, nil
}
