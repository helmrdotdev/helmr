// Package agent owns Agents' runtime facts beside their Deployment
// definitions: trigger activations and occurrences, trigger evaluation, and
// runtime Agent listing. A firing trigger admits through the Session owner,
// and through the Slack owner when the trigger has a Slack destination.
package agent

import (
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

// Agent outcomes are the Session outcomes, so a result keeps its meaning
// through every caller.
var (
	ErrInvalidInput = session.ErrInvalidInput
	ErrDenied       = session.ErrDenied
	ErrNotReady     = session.ErrNotReady
)

func hideMissing(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDenied
	}
	return err
}

func nullableID(id uuid.UUID) any {
	if id == uuid.Nil() {
		return nil
	}
	return id
}
