package dispatch

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// AllocateSessionProcess is called by trusted CP discovery after VM readiness.
// The retained epoch is the retry identity; a live or unfenced old process never
// becomes permission to rerun setup. Explicit resume must clear failure holds,
// and physical stop must be confirmed before a replacement can be admitted.
func (a *Allocator) AllocateSessionProcess(ctx context.Context, env, sessionID uuid.UUID, epoch int64) (session.ProcessAllocation, error) {
	if env == uuid.Nil() || sessionID == uuid.Nil() || epoch <= 0 {
		return session.ProcessAllocation{}, ErrInvalidInput
	}
	if r, err := session.ReadProcessAllocation(ctx, a.database, env, sessionID, epoch); err == nil {
		return r, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return session.ProcessAllocation{}, err
	}
	var result session.ProcessAllocation
	err := db.RunTx(ctx, a.database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		var group, host uuid.UUID
		var hostEpoch int64
		var region string
		if err := tx.QueryRow(ctx, `SELECT h.worker_group_id,h.id,l.worker_epoch,g.region_id FROM sessions s JOIN computer_leases l
 ON (l.environment_id,l.computer_id)=(s.environment_id,s.computer_id) JOIN worker_hosts h ON h.id=l.worker_host_id JOIN worker_groups g ON g.id=h.worker_group_id
 WHERE s.environment_id=$1 AND s.id=$2 AND l.fenced_at IS NULL`, env, sessionID).Scan(&group, &host, &hostEpoch, &region); err != nil {
			return err
		}
		admitting, err := workergroup.LockDispatchSupply(ctx, tx, workergroup.DispatchSupply{GroupID: pgvalue.UUID(group), HostID: pgvalue.UUID(host), Epoch: hostEpoch, RegionID: region, Continuation: true})
		if err != nil {
			return err
		}
		result, err = session.AdmitProcess(ctx, a.auditPolicy, tx, env, sessionID, epoch, host, hostEpoch, admitting)
		return err
	})
	if err != nil {
		return session.ProcessAllocation{}, allocationError(err)
	}
	if result.SessionID == uuid.Nil() {
		return session.ProcessAllocation{}, session.ErrNotReady
	}
	return result, nil
}
