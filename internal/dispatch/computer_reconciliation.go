package dispatch

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// reconcileComputerPreparations settles each failed preparation candidate in
// its own transaction.
func (d *Authority) reconcileComputerPreparations(ctx context.Context, limit int32) error {
	candidates, err := db.New(d.pool).ListFailedComputerPreparations(ctx, limit)
	if err != nil {
		return err
	}
	var failures []error
	for _, candidate := range candidates {
		if err := d.settleComputerPreparation(ctx, candidate); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
func (d *Authority) settleComputerPreparation(ctx context.Context, candidate db.ListFailedComputerPreparationsRow) error {
	tx, err := d.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	settled, err := computer.SettlePreparation(ctx, tx, candidate)
	if err != nil || !settled {
		return err
	}
	return tx.Commit(ctx)
}

// Logical settlement follows durable Computer failure in its own transaction.
// Discovery repeats until each member is terminal, including after a crash
// between budget settlement and member notification.
func (d *Authority) failPreparationBlockedMembers(ctx context.Context, limit int32) error {
	rows, err := d.pool.Query(ctx, `SELECT r.org_id,r.project_id,r.environment_id,r.id
 FROM runs r JOIN computers c ON c.id=r.computer_id
 WHERE (c.preparation_failure IS NOT NULL OR c.recovery_failure IS NOT NULL) AND (
 (r.status IN ('queued','retry_delayed') AND r.current_run_lease_id IS NULL)
 OR (r.status='waiting' AND EXISTS(SELECT 1 FROM run_waits w JOIN computer_checkpoints cp ON cp.id=w.suspend_checkpoint_id
 JOIN computer_checkpoint_runs m ON m.checkpoint_id=cp.id AND m.run_wait_id=w.id AND m.run_id=r.id AND m.attempt_number=r.current_attempt_number
 WHERE w.run_id=r.id AND w.attempt_number=r.current_attempt_number AND w.suspension_status IN ('parked','resume_pending','resuming')
 AND cp.status='invalid' AND cp.invalidation_reason_code='computer_preparation_exhausted')))
 ORDER BY r.id LIMIT $1`, limit)
	if err != nil {
		return err
	}
	var requests []run.OwnedFinalizationRequest
	for rows.Next() {
		var org, project, environment, id pgtype.UUID
		if err = rows.Scan(&org, &project, &environment, &id); err != nil {
			rows.Close()
			return err
		}
		requests = append(requests, run.OwnedFinalizationRequest{OrgID: pgvalue.MustUUIDValue(org), ProjectID: pgvalue.MustUUIDValue(project), EnvironmentID: pgvalue.MustUUIDValue(environment), RunID: pgvalue.MustUUIDValue(id)})
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	var failures []error
	for _, request := range requests {
		if err = d.failPreparationBlockedRun(ctx, request); err != nil {
			failures = append(failures, err)
		}
	}
	rows, err = d.pool.Query(ctx, `SELECT e.org_id,p.id,p.revision FROM computer_commands p JOIN computers c ON c.id=p.computer_id JOIN environments e ON e.id=p.environment_id
 WHERE (c.preparation_failure IS NOT NULL OR c.recovery_failure IS NOT NULL) AND p.status='pending' ORDER BY p.id LIMIT $1`, limit)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	var commands []CommandCandidate
	for rows.Next() {
		var p CommandCandidate
		if err = rows.Scan(&p.OrgID, &p.CommandID, &p.ExpectedRevision); err != nil {
			rows.Close()
			return errors.Join(append(failures, err)...)
		}
		commands = append(commands, p)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return errors.Join(append(failures, err)...)
	}
	for _, p := range commands {
		if err = d.failPreparationBlockedCommand(ctx, p); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
func (d *Authority) failPreparationBlockedRun(ctx context.Context, request run.OwnedFinalizationRequest) error {
	tx, err := d.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	graph, err := run.LockOwnedFinalization(ctx, tx, request)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = graph.FailComputerPreparation(ctx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (d *Authority) failPreparationBlockedCommand(ctx context.Context, p CommandCandidate) error {
	tx, err := d.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	q := db.New(tx)
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{OrgID: p.OrgID, CommandID: p.CommandID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	c, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: target.EnvironmentID, ID: target.ComputerID})
	if err != nil {
		return err
	}
	failure, reason := c.PreparationFailure, "computer_preparation_exhausted"
	if len(c.RecoveryFailure) != 0 {
		failure, reason = c.RecoveryFailure, "computer_source_unavailable"
	}
	if len(failure) == 0 {
		return nil
	}
	err = failPendingComputerCommand(ctx, tx, p, reason, failure)
	if errors.Is(err, ErrCandidateChanged) {
		return nil
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
