package run

import (
	"context"
	"errors"
	"slices"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// SourceSecrets are the Secret locks of a worker operation that acts from a
// live source Run on another Session or Computer (send, enqueue, close, event
// reads, run-sourced Actor start): the source Computer's Secrets, plus an
// addressed Computer's but never an addressed Session's Computer's. Only
// LockSourceSecretsForSession and LockSourceSecretsForComputer construct
// them; they are valid only inside the transaction that locked them. The
// caller fences the execution with LockLiveSource and then validates the
// source attempt's deliveries with ValidateSourceDelivery.
type SourceSecrets struct {
	tx      pgx.Tx
	fence   ExecutionFence
	located db.GetLiveRunLeaseLocatorsRow
	target  executionTarget
}

// LockSourceSecretsForSession locks the source Computer's Secrets for an
// operation that addresses the Session target. A missing source lease is
// ErrStaleSource.
func LockSourceSecretsForSession(ctx context.Context, tx pgx.Tx, fence ExecutionFence, target pgtype.UUID) (SourceSecrets, error) {
	return lockSourceSecrets(ctx, tx, fence, executionTarget{session: target})
}

// LockSourceSecretsForComputer locks the source Computer's and the addressed
// Computer target's Secrets in one ordered statement. A missing source lease
// is ErrStaleSource.
func LockSourceSecretsForComputer(ctx context.Context, tx pgx.Tx, fence ExecutionFence, target pgtype.UUID) (SourceSecrets, error) {
	return lockSourceSecrets(ctx, tx, fence, executionTarget{computer: target})
}

func lockSourceSecrets(ctx context.Context, tx pgx.Tx, fence ExecutionFence, target executionTarget) (SourceSecrets, error) {
	q := db.New(tx)
	located, err := q.GetLiveRunLeaseLocators(ctx, fence.liveLocators())
	if err != nil {
		return SourceSecrets{}, staleSource(err)
	}
	computers := []pgtype.UUID{located.ComputerID}
	if target.computer.Valid {
		computers = append(computers, target.computer)
	}
	if _, err = q.LockWorkerControlSecrets(ctx, computers); err != nil {
		return SourceSecrets{}, err
	}
	return SourceSecrets{tx: tx, fence: fence, located: located, target: target}, nil
}

// LockLiveSource fences the execution together with the addressed Session or
// Computer and requires a live source. An addressed target outside the
// source's environment is ErrExecutionTargetNotFound; a missing or
// mismatched execution is ErrStaleSource; other failures, including stale
// worker claims, are returned unchanged.
func (s SourceSecrets) LockLiveSource(ctx context.Context) (Execution, LiveSource, error) {
	if s.target.session.Valid {
		return checkLiveSource(LockLiveExecutionForSession(ctx, s.tx, s.fence, s.target.session))
	}
	return checkLiveSource(LockLiveExecutionForComputer(ctx, s.tx, s.fence, s.target.computer))
}

// ValidateSourceDelivery locks the source attempt's Secret deliveries. Its
// errors are secret.LockAttemptDelivery's.
func (s SourceSecrets) ValidateSourceDelivery(ctx context.Context) error {
	_, err := secret.LockAttemptDelivery(ctx, db.New(s.tx), s.located.RunID, s.located.AttemptNumber, s.located.ComputerID)
	return err
}

// ControlSecrets are the Secret locks of a worker Session control (cancel,
// interrupt, resume): the sorted union of the source Computer's and the target
// Session's Computer's Secrets. Only LockControlSecrets constructs them; they
// are valid only inside the transaction that locked them. The caller fences
// the execution with LockLiveSource or LockInterruptionLiveSource, applies its
// Session checks and Computer lock, and then calls Recheck.
type ControlSecrets struct {
	tx        pgx.Tx
	fence     ExecutionFence
	located   db.GetLiveRunLeaseLocatorsRow
	target    db.Session
	computers []pgtype.UUID
	locked    []db.LockWorkerControlSecretsRow
}

// LockControlSecrets reads, without locking, the source lease and the target
// Session, then locks the union of their Computers' Secrets. A missing source
// lease is ErrStaleSource; a target outside the source's environment is
// ErrExecutionTargetNotFound.
func LockControlSecrets(ctx context.Context, tx pgx.Tx, fence ExecutionFence, target pgtype.UUID) (ControlSecrets, error) {
	q := db.New(tx)
	located, err := q.GetLiveRunLeaseLocators(ctx, fence.liveLocators())
	if err != nil {
		return ControlSecrets{}, staleSource(err)
	}
	session, err := q.GetActor(ctx, db.GetActorParams{EnvironmentID: located.EnvironmentID, ID: target})
	if errors.Is(err, pgx.ErrNoRows) {
		return ControlSecrets{}, ErrExecutionTargetNotFound
	}
	if err != nil {
		return ControlSecrets{}, err
	}
	computers := []pgtype.UUID{located.ComputerID, session.ComputerID}
	locked, err := q.LockWorkerControlSecrets(ctx, computers)
	if err != nil {
		return ControlSecrets{}, err
	}
	return ControlSecrets{tx: tx, fence: fence, located: located, target: session, computers: computers, locked: locked}, nil
}

// Target is the target Session as read before the Secret locks.
func (c ControlSecrets) Target() db.Session { return cloneSession(c.target) }

// LockLiveSource fences the execution together with the target Session, as
// LockLiveExecutionForSession does, and requires a live source, with the
// outcomes of SourceSecrets.LockLiveSource.
func (c ControlSecrets) LockLiveSource(ctx context.Context) (Execution, LiveSource, error) {
	return checkLiveSource(LockLiveExecutionForSession(ctx, c.tx, c.fence, c.target.ID))
}

// LockInterruptionLiveSource fences the execution together with the target
// Session's owned graph, as LockLiveExecutionForSessionInterruption does, and
// requires a live source, with the outcomes of SourceSecrets.LockLiveSource.
func (c ControlSecrets) LockInterruptionLiveSource(ctx context.Context) (Execution, LiveSource, OwnedFinalization, error) {
	execution, graph, err := LockLiveExecutionForSessionInterruption(ctx, c.tx, c.fence, c.target.ID)
	execution, source, err := checkLiveSource(execution, err)
	if err != nil {
		return Execution{}, LiveSource{}, OwnedFinalization{}, err
	}
	return execution, source, graph, nil
}

// Recheck reads the Secret union again after the caller's Session and
// Computer fences and fails with secret.ErrDeliveryUnavailable if a binding
// changed. Those Computer locks block new bindings, so validating deliveries
// afterwards re-locks only the original union: the source attempt's and,
// when interrupting a different current Run of the target, that Run's.
func (c ControlSecrets) Recheck(ctx context.Context, interrupt bool) error {
	q := db.New(c.tx)
	reread, err := q.ReadWorkerControlSecrets(ctx, c.computers)
	if err != nil {
		return err
	}
	if len(reread) != len(c.locked) {
		return secret.ErrDeliveryUnavailable
	}
	for i := range reread {
		if reread[i].ComputerID != c.locked[i].ComputerID || reread[i].SecretID != c.locked[i].SecretID || reread[i].PlacementKind != c.locked[i].PlacementKind || reread[i].PlacementTarget != c.locked[i].PlacementTarget {
			return secret.ErrDeliveryUnavailable
		}
	}
	if _, err = secret.LockAttemptDelivery(ctx, q, c.located.RunID, c.located.AttemptNumber, c.located.ComputerID); err != nil {
		return err
	}
	if interrupt && c.target.CurrentRunID.Valid && c.target.CurrentRunID != c.located.RunID {
		current, err := q.GetRun(ctx, db.GetRunParams{EnvironmentID: c.located.EnvironmentID, ID: c.target.CurrentRunID})
		if err != nil {
			return err
		}
		if _, err = secret.LockAttemptDelivery(ctx, q, current.ID, current.CurrentAttemptNumber, c.target.ComputerID); err != nil {
			return err
		}
	}
	return nil
}

// TargetBindings are the target Session's Computer's locked Secret bindings.
func (c ControlSecrets) TargetBindings() []db.LockComputerSecretsForAdmissionRow {
	var bindings []db.LockComputerSecretsForAdmissionRow
	for _, row := range c.locked {
		if row.ComputerID == c.target.ComputerID {
			row.AllowedOrigins = slices.Clone(row.AllowedOrigins)
			bindings = append(bindings, db.LockComputerSecretsForAdmissionRow(row))
		}
	}
	return bindings
}

func staleSource(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrStaleSource
	}
	return err
}
