package agent

import (
	"context"
	"slices"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// ObserveProviderAbsentHostComputers consumes confirmed physical absence in the
// transaction recording it. Failure rolls back the complete confirmation; retry
// cannot leave only part of the host physically reconciled.
func ObserveProviderAbsentHostComputers(ctx context.Context, absence workergroup.ConfirmedProviderAbsence) error {
	tx, host := absence.Transaction(), absence.HostID()
	if tx == nil || host == uuid.Nil() {
		return ErrInvalidInput
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
		return err
	}
	preparations, err := lockAbsentHostPreparations(ctx, tx, host)
	if err != nil {
		return err
	}
	type allocation struct {
		identity  ComputerLeaseIdentity
		hostEpoch int64
	}
	rows, err := tx.Query(ctx, `SELECT environment_id,computer_id,computer_instance_id,epoch,worker_epoch FROM computer_leases WHERE worker_host_id=$1 AND fenced_at IS NULL ORDER BY environment_id,computer_id,epoch`, host)
	if err != nil {
		return err
	}
	allocations, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (allocation, error) {
		var a allocation
		err := row.Scan(&a.identity.EnvironmentID, &a.identity.ComputerID, &a.identity.InstanceID, &a.identity.Epoch, &a.hostEpoch)
		return a, err
	})
	if err != nil {
		return err
	}
	// One host may contain several ownership trees placed in different orders.
	// Lock the whole affected set roots -> Computers -> Sessions, never one tree
	// per Computer while retaining earlier Computer locks.
	for start := 0; start < len(allocations); {
		end := start + 1
		env := allocations[start].identity.EnvironmentID
		for end < len(allocations) && allocations[end].identity.EnvironmentID == env {
			end++
		}
		computers := make([]uuid.UUID, 0, end-start)
		for _, a := range allocations[start:end] {
			computers = append(computers, a.identity.ComputerID)
		}
		type member struct {
			session, computer uuid.UUID
			epoch, leaseEpoch int64
		}
		read := func() ([]member, error) {
			rows, err := tx.Query(ctx, `SELECT session_id,computer_id,epoch,computer_lease_epoch FROM session_processes WHERE environment_id=$1 AND computer_id=ANY($2::uuid[]) AND fenced_at IS NULL ORDER BY session_id`, env, computers)
			if err != nil {
				return nil, err
			}
			return pgx.CollectRows(rows, func(row pgx.CollectableRow) (member, error) {
				var m member
				err := row.Scan(&m.session, &m.computer, &m.epoch, &m.leaseEpoch)
				return m, err
			})
		}
		before, err := read()
		if err != nil {
			return err
		}
		sessions := make([]uuid.UUID, len(before))
		for i, m := range before {
			sessions[i] = m.session
		}
		if _, err = lockSessionsAndComputers(ctx, tx, env, sessions, computers); err != nil {
			return err
		}
		after, err := read()
		if err != nil {
			return err
		}
		if !slices.Equal(before, after) {
			return ErrNotReady
		}
		start = end
	}
	for _, a := range allocations {
		if err := recordComputerStopped(ctx, tx, host, a.hostEpoch, a.identity, "provider confirmed physical host absence", uuid.Nil()); err != nil {
			return err
		}
	}
	for _, p := range preparations {
		if err := recordPreparationStopped(ctx, tx, p.env, p.id, "provider confirmed physical host absence"); err != nil {
			return err
		}
	}
	return nil
}
