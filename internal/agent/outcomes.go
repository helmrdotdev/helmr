// Package agent owns Agents' runtime facts beside their Deployment
// definitions: trigger activations and occurrences, trigger evaluation, and
// runtime Agent listing. A firing trigger admits through the Session owner,
// and through the Slack owner when the trigger has a Slack destination.
package agent

import (
	"errors"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// Agent outcomes belong to this owner; lower-owner errors pass through unchanged.
var (
	ErrInvalidInput = errors.New("invalid operation input")
	ErrDenied       = errors.New("operation not authorized")
	ErrNotReady     = errors.New("execution is not ready")
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
