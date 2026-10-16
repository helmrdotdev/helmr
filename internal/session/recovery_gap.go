package session

import (
	"context"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/jackc/pgx/v5"
	"time"
	"uuid"
)

// RecoveryGap is retained with its hold independently of event/payload expiry.
type RecoveryGap struct {
	HoldID            uuid.UUID   `json:"holdId"`
	ComputerID        uuid.UUID   `json:"computerId"`
	LeaseEpoch        int64       `json:"leaseEpoch"`
	RecoverySaveID    *uuid.UUID  `json:"recoverySaveId"`
	RestoredAt        *time.Time  `json:"restoredAt"`
	AffectedTurnIDs   []uuid.UUID `json:"affectedTurnIds"`
	AffectedTurnCount int         `json:"affectedTurnCount"`
}

// RecordComputerRecoveryGap settles acknowledged file exposure while all open
// Sessions and the Computer are locked. Failed rows remain historical evidence;
// newer publications cannot erase the gap or release an existing hold.
func RecordComputerRecoveryGap(ctx context.Context, tx pgx.Tx, env, computerID uuid.UUID, epoch int64) error {
	if err := computer.RecordLeaseStoragePressure(ctx, tx, env, computerID, epoch, nil); err != nil {
		return err
	}
	if err := reconcileStoragePressureTx(ctx, tx, env, computerID, epoch); err != nil {
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
		var held bool
		// Expiry can expose the loss before physical fencing. Acknowledgment
		// remains valid when that same loss is later settled by the worker.
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM session_holds WHERE environment_id=$1 AND session_id=$2 AND kind='recovery_gap'
 AND (released_at IS NULL OR (recovery_gap->>'computerId'=$3::text AND (recovery_gap->>'leaseEpoch')::bigint >= $4)))`, env, id, computerID, epoch).Scan(&held); err != nil {
			return err
		}
		if held {
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
		if affected == nil {
			affected = []uuid.UUID{}
		}
		hold := uuid.NewV7()
		data, err := json.Marshal(RecoveryGap{hold, computerID, epoch, recovery, restoredAt, affected, len(affected)})
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason,kind,recovery_gap) VALUES($1,$2,$3,'local','Computer restored with unpreserved file changes','recovery_gap',$4)`, env, hold, id, data); err != nil {
			return err
		}
		if err = advanceControlAuthority(ctx, tx, env, []uuid.UUID{id}); err != nil {
			return err
		}
		if err = eventData(ctx, tx, env, id, uuid.Nil(), "session.recovery_gap", data); err != nil {
			return err
		}
	}
	return nil
}

// A newly admitted conversation shares the Computer's rolled-back files even
// when none of its own requests were acknowledged before the loss.
func inheritComputerRecoveryGap(ctx context.Context, tx pgx.Tx, env, id, computerID uuid.UUID) error {
	if computerID == uuid.Nil() {
		return nil
	}
	var data []byte
	err := tx.QueryRow(ctx, `SELECT h.recovery_gap FROM session_holds h JOIN sessions s ON (s.environment_id,s.id)=(h.environment_id,h.session_id)
 WHERE h.environment_id=$1 AND s.computer_id=$2 AND h.kind='recovery_gap' AND h.released_at IS NULL ORDER BY h.created_at DESC LIMIT 1`, env, computerID).Scan(&data)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var value RecoveryGap
	if err = json.Unmarshal(data, &value); err != nil {
		return err
	}
	hold := uuid.NewV7()
	value.HoldID = hold
	value.AffectedTurnIDs = []uuid.UUID{}
	value.AffectedTurnCount = 0
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason,kind,recovery_gap) VALUES($1,$2,$3,'local','Computer restored with unpreserved file changes','recovery_gap',$4)`, env, hold, id, encoded); err != nil {
		return err
	}
	return eventData(ctx, tx, env, id, uuid.Nil(), "session.recovery_gap", encoded)
}
