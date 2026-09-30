package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func discoverRunQueueScope(
	ctx context.Context,
	tx pgx.Tx,
	candidate RunCandidate,
) (pgtype.UUID, string, pgtype.Text, error) {
	var environmentID pgtype.UUID
	var queueName string
	var concurrencyKey pgtype.Text
	err := tx.QueryRow(ctx, `
SELECT environment_id, queue_name, concurrency_key
  FROM runs
 WHERE org_id = $1
   AND id = $2
   AND revision = $3`,
		candidate.OrgID,
		candidate.RunID,
		candidate.ExpectedRunRevision,
	).Scan(&environmentID, &queueName, &concurrencyKey)
	if err != nil {
		return pgtype.UUID{}, "", pgtype.Text{}, err
	}
	return environmentID, queueName, concurrencyKey, nil
}

func lockRunQueueScope(
	ctx context.Context,
	tx pgx.Tx,
	candidate RunCandidate,
) (pgtype.UUID, string, pgtype.Text, error) {
	environmentID, queueName, concurrencyKey, err := discoverRunQueueScope(
		ctx,
		tx,
		candidate,
	)
	if err != nil {
		return pgtype.UUID{}, "", pgtype.Text{}, err
	}
	key, err := queueScopeLockKey(environmentID, queueName, concurrencyKey)
	if err != nil {
		return pgtype.UUID{}, "", pgtype.Text{}, err
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", key); err != nil {
		return pgtype.UUID{}, "", pgtype.Text{}, fmt.Errorf("lock run queue scope: %w", err)
	}
	return environmentID, queueName, concurrencyKey, nil
}

func lockRunSecrets(
	ctx context.Context,
	tx pgx.Tx,
	candidate RunCandidate,
) error {
	secretRows, err := tx.Query(ctx, `
SELECT secrets.status = 'active'
       AND secret_resolutions.id IS NOT NULL
       AND secret_resolutions.revocation_generation = secrets.revocation_generation
  FROM runs
  JOIN computer_secrets ON computer_secrets.computer_id = runs.computer_id
  JOIN secrets ON secrets.id = computer_secrets.secret_id
  LEFT JOIN secret_resolutions
    ON secret_resolutions.computer_id = computer_secrets.computer_id
   AND secret_resolutions.run_id = runs.id
   AND secret_resolutions.attempt_number = runs.current_attempt_number
   AND secret_resolutions.placement_kind = computer_secrets.placement_kind
   AND secret_resolutions.placement_target = computer_secrets.placement_target
   AND secret_resolutions.secret_id = computer_secrets.secret_id
 WHERE runs.org_id = $1
   AND runs.id = $2
   AND runs.revision = $3
   AND runs.status = 'queued'
   AND runs.current_run_lease_id IS NULL
 ORDER BY secrets.id, computer_secrets.placement_kind, computer_secrets.placement_target
 FOR UPDATE OF secrets`,
		candidate.OrgID,
		candidate.RunID,
		candidate.ExpectedRunRevision,
	)
	if err != nil {
		return err
	}
	for secretRows.Next() {
		var valid bool
		if err := secretRows.Scan(&valid); err != nil {
			secretRows.Close()
			return err
		}
		if !valid {
			secretRows.Close()
			return errors.New("run secret resolution is revoked or incomplete")
		}
	}
	if err := secretRows.Err(); err != nil {
		secretRows.Close()
		return err
	}
	secretRows.Close()
	return nil
}

// Computer and Instance locks must precede Session and Run admission locks.
func lockRunAssignment(ctx context.Context, tx pgx.Tx, candidate RunCandidate, environmentID, computerID pgtype.UUID) (db.Run, error) {
	var sessionID pgtype.UUID
	err := tx.QueryRow(ctx, `SELECT session_id FROM runs WHERE org_id=$1 AND id=$2 AND revision=$3 AND environment_id=$4 AND computer_id=$5`, candidate.OrgID, candidate.RunID, candidate.ExpectedRunRevision, environmentID, computerID).Scan(&sessionID)
	if err != nil {
		return db.Run{}, err
	}
	if sessionID.Valid {
		var id pgtype.UUID
		err = tx.QueryRow(ctx, `SELECT id FROM sessions WHERE id=$1 AND current_run_id=$2 AND computer_id=$3 AND status IN ('open','closing') AND cancel_requested_at IS NULL AND dispatch_hold_id IS NULL FOR UPDATE`, sessionID, candidate.RunID, computerID).Scan(&id)
		if err != nil {
			return db.Run{}, err
		}
	}
	var valid bool
	err = tx.QueryRow(ctx, `SELECT r.status='queued' AND r.current_run_lease_id IS NULL
 AND r.revision=$3 AND r.environment_id=$4 AND r.computer_id=$5
 AND r.active_elapsed_ms<r.max_active_duration_ms
 AND (r.next_instance_preparation_at IS NULL OR r.next_instance_preparation_at<=clock_timestamp())
 AND (r.first_lease_at IS NOT NULL OR r.queued_expires_at IS NULL OR r.queued_expires_at>clock_timestamp())
 AND EXISTS(SELECT 1 FROM run_attempts a WHERE a.run_id=r.id AND a.number=r.current_attempt_number AND a.terminal_at IS NULL)
 AND (r.parent_owns_lifecycle IS NOT TRUE OR EXISTS(SELECT 1 FROM runs parent WHERE parent.id=r.parent_run_id AND parent.status IN ('queued','running','waiting','retry_delayed')))
 AND EXISTS(SELECT 1 FROM deployments d JOIN deployment_definitions def ON def.environment_id=d.environment_id AND def.deployment_id=d.id
 WHERE d.id=r.deployment_id AND d.environment_id=r.environment_id AND def.id=r.deployment_definition_id AND def.kind::text=r.entrypoint_kind::text
 AND d.program_artifact_id IS NOT NULL AND d.runtime_artifact_digest IS NOT NULL AND d.program_index_digest IS NOT NULL)
 FROM runs r WHERE r.id=$2 AND r.org_id=$1 FOR UPDATE OF r`, candidate.OrgID, candidate.RunID, candidate.ExpectedRunRevision, environmentID, computerID).Scan(&valid)
	if err != nil {
		return db.Run{}, err
	}
	if !valid {
		return db.Run{}, ErrCandidateChanged
	}
	return db.New(tx).GetRun(ctx, db.GetRunParams{EnvironmentID: environmentID, ID: candidate.RunID})
}
