package session

import (
	"context"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/jackc/pgx/v5"
	"time"
	"uuid"
)

// RecoveryGap describes a Computer rollback that lost acknowledged file changes.
// HoldID is nil for a notice to a Session without affected completed Turns.
type RecoveryGap struct {
	HoldID     *uuid.UUID `json:"holdId"`
	ComputerID uuid.UUID  `json:"computerId"`
	LeaseEpoch int64      `json:"leaseEpoch"`
	recoveryGapDetails
}

// recoveryGapDetails is the descriptive snapshot; hold and lease identity live in typed columns.
type recoveryGapDetails struct {
	RecoverySaveID  *uuid.UUID  `json:"recoverySaveId"`
	RestoredAt      *time.Time  `json:"restoredAt"`
	AffectedTurnIDs []uuid.UUID `json:"affectedTurnIds"`
}

// recoveryGapNoticedSQL is true when Session $2 already received the recovery
// gap of Computer $3 at lease epoch $4, as a hold or as a notice.
const recoveryGapNoticedSQL = `EXISTS(SELECT 1 FROM session_events WHERE environment_id=$1 AND session_id=$2 AND kind='session.recovery_gap' AND data IS NOT NULL
 AND convert_from(data,'UTF8')::jsonb @> jsonb_build_object('computerId',$3::uuid,'leaseEpoch',$4::bigint))`

// RecordComputerRecoveryGap settles acknowledged file exposure while all open
// Sessions and the Computer are locked. Sessions whose completed Turns lost
// their saves are held; their peers are notified and keep running. Failed rows
// remain historical evidence; newer publications cannot erase the gap.
func RecordComputerRecoveryGap(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, env, computerID uuid.UUID, epoch int64) error {
	if err := computer.RecordLeaseStoragePressure(ctx, tx, env, computerID, epoch, false); err != nil {
		return err
	}
	if err := reconcileStoragePressureTx(ctx, auditPolicy, tx, env, computerID, epoch); err != nil {
		return err
	}
	var gap bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_saves save JOIN turns t ON (t.environment_id,t.id)=(save.environment_id,save.turn_id)
 WHERE save.environment_id=$1 AND save.computer_id=$2 AND save.computer_lease_epoch=$3 AND save.status='failed' AND t.status='completed')`, env, computerID, epoch).Scan(&gap); err != nil {
		return err
	}
	if !gap {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT id FROM sessions WHERE environment_id=$1 AND computer_id=$2 AND status IN ('open','closing') ORDER BY id`, env, computerID)
	if err != nil {
		return err
	}
	sessions, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	var recovery *uuid.UUID
	var restoredAt *time.Time
	if err = tx.QueryRow(ctx, `SELECT c.recovery_save_id,s.captured_at FROM computers c LEFT JOIN computer_saves s ON (s.environment_id,s.id)=(c.environment_id,c.recovery_save_id) WHERE c.environment_id=$1 AND c.id=$2`, env, computerID).Scan(&recovery, &restoredAt); err != nil {
		return err
	}
	for _, id := range sessions {
		// Expiry can expose the loss before physical fencing. The same loss
		// settled later by the worker is recorded once.
		var recorded bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM session_holds WHERE environment_id=$1 AND session_id=$2 AND kind='recovery_gap'
 AND (released_at IS NULL OR (recovery_computer_id=$3 AND recovery_lease_epoch >= $4))) OR `+recoveryGapNoticedSQL, env, id, computerID, epoch).Scan(&recorded); err != nil {
			return err
		}
		if recorded {
			continue
		}
		affectedRows, err := tx.Query(ctx, `SELECT t.id FROM turns t JOIN computer_saves save ON (save.environment_id,save.turn_id)=(t.environment_id,t.id) WHERE t.environment_id=$1 AND t.session_id=$2 AND t.status='completed' AND save.status='failed' AND save.computer_lease_epoch=$3 ORDER BY t.seq`, env, id, epoch)
		if err != nil {
			return err
		}
		affected, err := pgx.CollectRows(affectedRows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		value := RecoveryGap{ComputerID: computerID, LeaseEpoch: epoch, recoveryGapDetails: recoveryGapDetails{recovery, restoredAt, []uuid.UUID{}}}
		if len(affected) > 0 {
			hold := uuid.NewV7()
			value.HoldID, value.AffectedTurnIDs = &hold, affected
			stored, err := json.Marshal(value.recoveryGapDetails)
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind,recovery_computer_id,recovery_lease_epoch,recovery_gap) VALUES($1,$2,$3,'recovery_gap','system',$4,$5,$6)`, env, hold, id, computerID, epoch, stored); err != nil {
				return err
			}
			if err = advanceControlAuthority(ctx, tx, env, []uuid.UUID{id}); err != nil {
				return err
			}
		}
		if err = recoveryGapEvent(ctx, tx, env, id, value); err != nil {
			return err
		}
	}
	return nil
}

func recoveryGapEvent(ctx context.Context, tx pgx.Tx, env, session uuid.UUID, value RecoveryGap) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return eventData(ctx, tx, env, session, uuid.Nil(), "session.recovery_gap", data)
}

// A newly admitted conversation shares the Computer's rolled-back files while
// an affected Session's recovery hold is unreleased. It is notified, not held.
func inheritComputerRecoveryGap(ctx context.Context, tx pgx.Tx, env, id, computerID uuid.UUID) error {
	if computerID == uuid.Nil() {
		return nil
	}
	var data []byte
	var value RecoveryGap
	err := tx.QueryRow(ctx, `SELECT h.recovery_computer_id,h.recovery_lease_epoch,h.recovery_gap FROM session_holds h JOIN sessions s ON (s.environment_id,s.id)=(h.environment_id,h.session_id)
 WHERE h.environment_id=$1 AND s.computer_id=$2 AND h.kind='recovery_gap' AND h.released_at IS NULL ORDER BY h.created_at DESC LIMIT 1`, env, computerID).Scan(&value.ComputerID, &value.LeaseEpoch, &data)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, &value.recoveryGapDetails); err != nil {
		return err
	}
	value.AffectedTurnIDs = []uuid.UUID{}
	return recoveryGapEvent(ctx, tx, env, id, value)
}
