package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type computerCommandPermanentError struct {
	code string
	err  error
}

func (e computerCommandPermanentError) Error() string {
	if e.err == nil {
		return e.code
	}
	return e.err.Error()
}
func (e computerCommandPermanentError) Unwrap() error { return e.err }

type ReadyComputerCommandCandidate struct {
	OrgID, CommandID pgtype.UUID
	ExpectedRevision int64
}
type ComputerCommandPlacement struct {
	WorkerHostID, ComputerInstanceID pgtype.UUID
	WorkerEpoch                      int64
	ProcessBound                     bool
}

func (d *Authority) PlaceComputerCommand(ctx context.Context, candidate ReadyComputerCommandCandidate) (ComputerCommandPlacement, error) {
	tx, err := d.begin(ctx)
	if err != nil {
		return ComputerCommandPlacement{}, err
	}
	defer rollback(ctx, tx)
	q := db.New(tx)
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{OrgID: candidate.OrgID, CommandID: candidate.CommandID})
	if err != nil {
		return ComputerCommandPlacement{}, classifyComputerCommandCandidateError(err)
	}
	if err = lockComputerCommandSecrets(ctx, tx, candidate); err != nil {
		return ComputerCommandPlacement{}, d.finishRejectedComputerCommand(ctx, tx, candidate, err)
	}
	prepared, err := discoverComputerPlacement(ctx, tx, target.EnvironmentID, target.ComputerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ComputerCommandPlacement{}, ErrCapacityUnavailable
	}
	if err != nil {
		return ComputerCommandPlacement{}, err
	}
	prepared, err = lockComputerPlacement(ctx, tx, prepared)
	if err != nil {
		return ComputerCommandPlacement{}, classifyComputerCommandCandidateError(err)
	}
	command, err := q.LockComputerCommand(ctx, db.LockComputerCommandParams{EnvironmentID: target.EnvironmentID, ComputerID: target.ComputerID, CommandID: candidate.CommandID})
	if err != nil {
		return ComputerCommandPlacement{}, err
	}
	if command.Revision != candidate.ExpectedRevision || command.Status != "pending" || command.ComputerInstanceID.Valid {
		return ComputerCommandPlacement{}, ErrCandidateChanged
	}
	instance := prepared.instance
	if !instance.ID.Valid {
		instance, err = d.allocateComputerPlacement(ctx, tx, prepared)
		if err != nil {
			return ComputerCommandPlacement{}, err
		}
	}
	placement := ComputerCommandPlacement{WorkerHostID: instance.WorkerHostID, ComputerInstanceID: instance.ID, WorkerEpoch: instance.WorkerEpoch}
	if instance.ObservedState == "ready" && instance.DesiredState == "ready" && instance.MountState == "mounted" && instance.AdmissionState == "open" {
		_, err = q.BindComputerCommandInstance(ctx, db.BindComputerCommandInstanceParams{EnvironmentID: target.EnvironmentID, ComputerID: target.ComputerID, CommandID: command.ID, ComputerInstanceID: instance.ID, WriterGeneration: instance.WriterGeneration})
		if err != nil {
			return ComputerCommandPlacement{}, classifyComputerCommandCandidateError(err)
		}
		placement.ProcessBound = true
	}
	if err = tx.Commit(ctx); err != nil {
		return ComputerCommandPlacement{}, err
	}
	return placement, nil
}

func lockComputerCommandSecrets(
	ctx context.Context,
	tx pgx.Tx,
	candidate ReadyComputerCommandCandidate,
) error {
	rows, err := tx.Query(ctx, `
SELECT secrets.status = 'active'
       AND secret_resolutions.id IS NOT NULL
       AND secret_resolutions.revocation_generation = secrets.revocation_generation
  FROM computer_commands
  JOIN computer_secrets
    ON computer_secrets.computer_id = computer_commands.computer_id
  JOIN secrets
    ON secrets.id = computer_secrets.secret_id
  LEFT JOIN secret_resolutions
    ON secret_resolutions.computer_id = computer_secrets.computer_id
   AND secret_resolutions.command_id = computer_commands.id
   AND secret_resolutions.placement_kind = computer_secrets.placement_kind
   AND secret_resolutions.placement_target = computer_secrets.placement_target
   AND secret_resolutions.secret_id = computer_secrets.secret_id
 JOIN environments e ON e.id=computer_commands.environment_id
 WHERE e.org_id = $1
   AND computer_commands.id = $2
   AND computer_commands.revision = $3
   AND computer_commands.status = 'pending'
 ORDER BY secrets.id, computer_secrets.placement_kind, computer_secrets.placement_target
 FOR UPDATE OF secrets`,
		candidate.OrgID,
		candidate.CommandID,
		candidate.ExpectedRevision,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var valid bool
		if err := rows.Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return computerCommandPermanentError{
				code: "computer_command_secret_unavailable",
				err:  errors.New("Command secret resolution is revoked or incomplete"),
			}
		}
	}
	return rows.Err()
}

func classifyComputerCommandCandidateError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCandidateChanged
	}
	return err
}

func (d *Authority) finishRejectedComputerCommand(
	ctx context.Context,
	tx pgx.Tx,
	candidate ReadyComputerCommandCandidate,
	cause error,
) error {
	var permanent computerCommandPermanentError
	if !errors.As(cause, &permanent) {
		return classifyComputerCommandCandidateError(cause)
	}
	errorJSON, err := json.Marshal(map[string]string{
		"code":    permanent.code,
		"message": permanent.Error(),
	})
	if err != nil {
		return fmt.Errorf("encode Command rejection: %w", err)
	}
	if err := failPendingComputerCommand(
		ctx,
		tx,
		candidate,
		permanent.code,
		errorJSON,
	); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit rejected Command: %w", err)
	}
	return nil
}

func (d *Authority) FailPendingComputerCommand(
	ctx context.Context,
	candidate ReadyComputerCommandCandidate,
	reasonCode string,
) error {
	tx, err := d.begin(ctx)
	if err != nil {
		return fmt.Errorf("begin pending Command failure: %w", err)
	}
	defer rollback(ctx, tx)
	errorJSON, err := json.Marshal(map[string]string{"code": reasonCode})
	if err != nil {
		return err
	}
	if err := failPendingComputerCommand(
		ctx,
		tx,
		candidate,
		reasonCode,
		errorJSON,
	); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit pending Command failure: %w", err)
	}
	return nil
}

func failPendingComputerCommand(
	ctx context.Context,
	tx pgx.Tx,
	candidate ReadyComputerCommandCandidate,
	reasonCode string,
	errorJSON []byte,
) error {
	q := db.New(tx)
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{OrgID: candidate.OrgID, CommandID: candidate.CommandID})
	if err != nil {
		return classifyComputerCommandCandidateError(err)
	}
	if _, err = q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: target.EnvironmentID, ID: target.ComputerID}); err != nil {
		return err
	}
	command, err := q.LockComputerCommand(ctx, db.LockComputerCommandParams{EnvironmentID: target.EnvironmentID, ComputerID: target.ComputerID, CommandID: candidate.CommandID})
	if err != nil {
		return err
	}
	if command.Revision != candidate.ExpectedRevision {
		return ErrCandidateChanged
	}
	_, err = q.FailPendingComputerCommand(
		ctx,
		db.FailPendingComputerCommandParams{
			ReasonCode:    pgvalue.Text(reasonCode),
			Error:         errorJSON,
			EnvironmentID: target.EnvironmentID,
			CommandID:     candidate.CommandID,
		},
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCandidateChanged
		}
		return fmt.Errorf("fail pending Command: %w", err)
	}
	return nil
}
