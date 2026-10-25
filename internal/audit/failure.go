package audit

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

var ErrCapture = errors.New("mandatory audit capture failed")

func Anonymous(credentialKind string) Actor {
	return Actor{Kind: "anonymous", CredentialKind: credentialKind, Authorization: Authorization{Basis: "unauthenticated", Decision: "denied"}}
}

// RecordFailure is called only after an owner's failed transaction has returned.
// Cancellation does not erase an observed refusal; capture has its own short bound.
// Targets and scope must come from trusted caller/owner data, never tenant claims.
func RecordFailure(ctx context.Context, database db.TxBeginner, policy auditpolicy.Policy, event Event, reason string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	detail := FailureDetails{Reason: reason}
	if supplied, ok := event.Details.(FailureDetails); ok {
		detail.Operation = supplied.Operation
	}
	event.Details = detail
	if event.Outcome == "" {
		event.Outcome = "denied"
	}
	event.Actor.Authorization.Decision = "denied"
	event.Actor.Authorization.Reason = reason
	if err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout='1500ms'`); err != nil {
			return err
		}
		return Append(ctx, tx, policy, event)
	}); err != nil {
		return errors.Join(ErrCapture, err)
	}
	return nil
}
