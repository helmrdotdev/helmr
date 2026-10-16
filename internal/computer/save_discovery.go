package computer

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// NextSave inspects the oldest unresolved request for this exact writer. New
// coverage freezes only on admission; an already claimed prefix is always returned
// for retry. Unclaimed metadata cannot authorize capture or publication.
// Checkpoint cuts remain owned by coherent capture; an earlier unresolved one
// also blocks a later live cut. A due owner may admit one optional disk save
// only after rechecking pending work under the Computer lock.
func NextSave(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, identity LeaseIdentity, admit, backgroundDue bool, retirement *SaveRetirement) (*SaveRequest, error) {
	if !identity.Valid() {
		return nil, ErrInvalidInput
	}
	var result *SaveRequest
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := shortLockTimeout(ctx, tx); err != nil {
			return err
		}
		if err := LockHost(ctx, tx, host); err != nil {
			return err
		}
		var valid bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND allocation_id=$4 AND worker_host_id=$5 AND worker_epoch=$6 AND status IN ('active','releasing') AND fenced_at IS NULL AND expires_at>clock_timestamp())`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, identity.InstanceID, host.HostID, host.Epoch).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return ErrDenied
		}
		// Selection and its immutable pre-flush prefix serialize with completion
		// and checkpoint admission. A lost discovery reply reuses this receipt.
		var computer uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, identity.EnvironmentID, identity.ComputerID).Scan(&computer); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND allocation_id=$4 AND worker_host_id=$5 AND worker_epoch=$6 AND status IN ('active','releasing') AND fenced_at IS NULL AND expires_at>clock_timestamp())`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, identity.InstanceID, host.HostID, host.Epoch).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return ErrDenied
		}
		if retirement != nil {
			if err := advanceSaveRetirement(ctx, tx, identity, *retirement); err != nil {
				return err
			}
		}
		pending, checkpoint, err := oldestComputerSave(ctx, tx, identity)
		if err != nil || checkpoint {
			return err
		}
		if pending != nil {
			if !admit && !pending.Claimed {
				result = pending
				return nil
			}
			if err = freezeSaveCoverage(ctx, tx, identity, *pending); err != nil {
				return err
			}
			pending.Claimed = true
			pending.AgeMillis = 0
			result = pending
			return nil
		}
		if !admit || !backgroundDue {
			return nil
		}
		if err := DispatchAvailable(ctx, tx, identity.EnvironmentID, identity.ComputerID); err != nil {
			if errors.Is(err, ErrNotReady) {
				return nil
			}
			return err
		}
		// Group/Host locks exclude retirement, including Pool retirement which
		// takes the Group lock. Draining owners may finish existing requests but
		// must not create new optional work.
		var eligible bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM computer_leases l
 JOIN worker_hosts h ON h.id=l.worker_host_id AND h.status='active'
 JOIN worker_groups g ON g.id=h.worker_group_id AND g.status='active'
 JOIN worker_pools p ON p.id=h.worker_pool_id AND p.worker_group_id=g.id AND p.status='active'
 WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3 AND l.allocation_id=$4
 AND l.worker_host_id=$5 AND l.worker_epoch=$6 AND l.status='active'
 AND l.fenced_at IS NULL AND l.expires_at>clock_timestamp())`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, identity.InstanceID, host.HostID, host.Epoch).Scan(&eligible); err != nil {
			return err
		}
		if !eligible {
			return nil
		}
		save := SaveRequest{ID: uuid.NewV7(), ComputerID: identity.ComputerID, LeaseEpoch: identity.Epoch}
		err = tx.QueryRow(ctx, `UPDATE computers SET next_save_seq=next_save_seq+1 WHERE environment_id=$1 AND id=$2 AND integrity_fault_at IS NULL AND deleted_at IS NULL RETURNING next_save_seq-1`, identity.EnvironmentID, identity.ComputerID).Scan(&save.Sequence)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq) VALUES($1,$2,$3,$4,$5) RETURNING requested_at`, identity.EnvironmentID, save.ID, save.ComputerID, save.LeaseEpoch, save.Sequence).Scan(&save.RequestedAt); err != nil {
			return err
		}
		if err = freezeSaveCoverage(ctx, tx, identity, save); err != nil {
			return err
		}
		save.Claimed = true
		result = &save
		return nil
	})
	return result, notReadyOnLock(err)
}

// The oldest request controls ordering even when a checkpoint owns its cut.
func oldestComputerSave(ctx context.Context, tx pgx.Tx, identity LeaseIdentity) (*SaveRequest, bool, error) {
	var save SaveRequest
	var checkpoint bool
	err := tx.QueryRow(ctx, `SELECT s.id,s.computer_id,s.computer_lease_epoch,s.seq,s.requested_at,
 s.coverage_max_seq IS NOT NULL, GREATEST(0,EXTRACT(EPOCH FROM clock_timestamp()-s.requested_at)*1000)::bigint,
 EXISTS(SELECT 1 FROM checkpoints p WHERE p.environment_id=s.environment_id AND p.disk_save_id=s.id)
 FROM computer_saves s WHERE s.environment_id=$1 AND s.computer_id=$2 AND s.computer_lease_epoch=$3
 AND s.status IN ('requested','captured') ORDER BY s.seq LIMIT 1`, identity.EnvironmentID, identity.ComputerID, identity.Epoch).Scan(&save.ID, &save.ComputerID, &save.LeaseEpoch, &save.Sequence, &save.RequestedAt, &save.Claimed, &save.AgeMillis, &checkpoint)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil || checkpoint {
		return nil, checkpoint, err
	}
	return &save, false, nil
}

// SaveRequest is durable before the worker may flush/capture. An uncertain reply
// is reconciled with this identity, never replaced by another cut.
type SaveRequest struct {
	ID          uuid.UUID
	ComputerID  uuid.UUID
	LeaseEpoch  int64
	Sequence    int64
	RequestedAt time.Time
	Claimed     bool
	AgeMillis   int64
}

// Checkpoints are strict barriers. A retry never expands an already selected
// cut to include results that committed after its first pre-flush discovery.
func freezeSaveCoverage(ctx context.Context, tx pgx.Tx, identity LeaseIdentity, save SaveRequest) error {
	_, err := tx.Exec(ctx, `UPDATE computer_saves chosen SET coverage_max_seq=(
 SELECT COALESCE(max(s.seq),chosen.seq) FROM computer_saves s
 WHERE s.environment_id=chosen.environment_id AND s.computer_id=chosen.computer_id
 AND s.computer_lease_epoch=chosen.computer_lease_epoch AND s.status IN ('requested','captured')
 AND s.seq>=chosen.seq AND s.seq<COALESCE((SELECT min(barrier.seq) FROM computer_saves barrier JOIN checkpoints cp ON (cp.environment_id,cp.disk_save_id)=(barrier.environment_id,barrier.id)
 WHERE barrier.environment_id=chosen.environment_id AND barrier.computer_id=chosen.computer_id AND barrier.seq>chosen.seq),9223372036854775807))
 WHERE chosen.environment_id=$1 AND chosen.id=$2 AND chosen.coverage_max_seq IS NULL`, identity.EnvironmentID, save.ID)
	return err
}

// SaveRetirement is the exact durable adoption reported by this physical writer.
// The Computer lock serializes its monotonic floor with publication/retention.
type SaveRetirement struct {
	SaveID uuid.UUID
	Root   disk.VersionRoot
}

func advanceSaveRetirement(ctx context.Context, tx pgx.Tx, identity LeaseIdentity, retirement SaveRetirement) error {
	digest, err := retirement.Root.Digest()
	if err != nil || retirement.SaveID == uuid.Nil() {
		return ErrInvalidInput
	}
	var seq int64
	err = tx.QueryRow(ctx, `SELECT seq FROM computer_saves WHERE environment_id=$1 AND computer_id=$2 AND computer_lease_epoch=$3 AND id=$4 AND status='published' AND captured_root_digest=decode(substring($5::text from 8),'hex')`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, retirement.SaveID, digest).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE computer_leases SET save_retirement_seq=GREATEST(COALESCE(save_retirement_seq,0),$4) WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, seq)
	return err
}
