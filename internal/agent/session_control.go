package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// SessionControlRequest identifies one intentional control operation. Acceptance
// is durable; it does not assert that native processes have stopped.
type SessionControlRequest struct {
	EnvironmentID uuid.UUID
	SessionID     uuid.UUID
	Kind          string
	RetryKey      string
	HoldID        uuid.UUID
	Reason        string
}

type SessionControlReceipt struct {
	ID        uuid.UUID
	SessionID uuid.UUID
	HoldID    uuid.UUID
}

// ControlSession serializes controls with admission and settlement under the
// ownership root. Retried acceptance never repeats its lifecycle transition.
func ControlSession(ctx context.Context, pool db.TxBeginner, caller Caller, req SessionControlRequest) (SessionControlReceipt, error) {
	var result SessionControlReceipt
	if req.RetryKey == "" || len(req.RetryKey) > 512 || !utf8.ValidString(req.RetryKey) || strings.ContainsRune(req.RetryKey, 0) ||
		len(req.Reason) > 4096 || !utf8.ValidString(req.Reason) || strings.ContainsRune(req.Reason, 0) || caller.ID == uuid.Nil() ||
		(req.Kind != "interrupt" && req.Kind != "resume" && req.Kind != "close" && req.Kind != "cancel") ||
		(req.Kind == "resume") != (req.HoldID != uuid.Nil()) ||
		(req.Kind != "interrupt" && req.Reason != "") {
		return result, ErrInvalidInput
	}
	payload, _ := json.Marshal(struct {
		HoldID uuid.UUID
		Reason string
	}{req.HoldID, req.Reason})
	digest, err := digestJSON(payload)
	if err != nil {
		return result, err
	}
	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if caller.Kind == "session" {
			if caller.Execution.EnvironmentID != req.EnvironmentID || caller.Execution.SessionID != caller.ID {
				return ErrDenied
			}
			if err := lockRuntimeHost(ctx, tx, caller); err != nil {
				return err
			}
		} else if err := authorizeExternalControl(ctx, tx, caller, req.EnvironmentID, req.Kind); err != nil {
			return err
		}
		if caller.Kind == "session" {
			// Ownership ancestry is immutable. Reject foreign targets before
			// taking their root/Computer locks on behalf of workload input.
			if err := requireOwnedTarget(ctx, tx, req.EnvironmentID, caller.ID, req.SessionID); err != nil {
				return err
			}
		}
		ids, locked, err := lockControlTree(ctx, tx, req.EnvironmentID, req.SessionID, caller)
		if err != nil {
			return err
		}
		if caller.Kind == "session" {
			if err = executionReceipt(ctx, tx, caller.Execution, locked[caller.ID]); err != nil {
				return err
			}
		} else if err = authorizeExternalControl(ctx, tx, caller, req.EnvironmentID, req.Kind); err != nil {
			return err
		}
		var prior []byte
		var hold *uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id,hold_id,request_digest FROM session_controls
 WHERE environment_id=$1 AND session_id=$2 AND caller_kind=$3 AND caller_id=$4 AND kind=$5 AND retry_key=$6`,
			req.EnvironmentID, req.SessionID, caller.Kind, caller.ID, req.Kind, req.RetryKey).Scan(&result.ID, &hold, &prior)
		if err == nil {
			if !bytes.Equal(prior, digest[:]) {
				return ErrConflict
			}
			result.SessionID = req.SessionID
			if hold != nil {
				result.HoldID = *hold
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if caller.Kind == "session" {
			// The execution checks are shared with runtime input, but target
			// authority above is deliberately narrower than input authority.
			if err = authorizeEnqueue(ctx, tx, caller, req.EnvironmentID, locked[caller.ID]); err != nil {
				return err
			}
		}
		result = SessionControlReceipt{ID: uuid.NewV7(), SessionID: req.SessionID, HoldID: req.HoldID}
		switch req.Kind {
		case "interrupt":
			if locked[req.SessionID].lifecycle == "closed" || locked[req.SessionID].lifecycle == "cancelled" {
				return ErrNotReady
			}
			result.HoldID = uuid.NewV7()
			if _, err = tx.Exec(ctx, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason,issuer_kind,issuer_id)
 VALUES($1,$2,$3,'subtree',$4,$5,$6)`, req.EnvironmentID, result.HoldID, req.SessionID, req.Reason, caller.Kind, caller.ID); err != nil {
				return err
			}
			if err = settleControlledTurns(ctx, tx, req.EnvironmentID, ids, false); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE session_processes SET status='stopping' WHERE environment_id=$1 AND session_id=ANY($2) AND status='starting' AND fenced_at IS NULL`, req.EnvironmentID, ids); err != nil {
				return err
			}
			if err = advanceControlAuthority(ctx, tx, req.EnvironmentID, ids); err != nil {
				return err
			}
		case "resume":
			var issuer, scope string
			var issuerID *uuid.UUID
			if err = tx.QueryRow(ctx, `SELECT issuer_kind,issuer_id,scope FROM session_holds WHERE environment_id=$1 AND session_id=$2 AND id=$3`, req.EnvironmentID, req.SessionID, req.HoldID).Scan(&issuer, &issuerID, &scope); err != nil {
				return err
			}
			if (issuer == "user" && caller.Kind != "user") ||
				(caller.Kind == "session" && (issuer != "session" || issuerID == nil || *issuerID != caller.ID)) {
				return ErrDenied
			}
			tag, err := tx.Exec(ctx, `UPDATE session_holds SET released_at=clock_timestamp() WHERE environment_id=$1 AND id=$2 AND released_at IS NULL`, req.EnvironmentID, req.HoldID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() > 0 {
				if scope == "local" {
					ids = []uuid.UUID{req.SessionID}
				}
				if err = advanceControlAuthority(ctx, tx, req.EnvironmentID, ids); err != nil {
					return err
				}
			}
		case "cancel":
			if err = settleControlledTurns(ctx, tx, req.EnvironmentID, ids, true); err != nil {
				return err
			}
			changed, err := tx.Query(ctx, `UPDATE sessions SET status='cancelled',authority_generation=authority_generation+1 WHERE environment_id=$1 AND id=ANY($2) AND status NOT IN ('closed','cancelled') RETURNING id`, req.EnvironmentID, ids)
			if err != nil {
				return err
			}
			cancelled, err := pgx.CollectRows(changed, pgx.RowTo[uuid.UUID])
			if err != nil {
				return err
			}
			for _, id := range cancelled {
				if err = event(ctx, tx, req.EnvironmentID, id, uuid.Nil(), "session.cancelled"); err != nil {
					return err
				}
			}
			// Idle processes must also stop on cancellation; independent peers
			// sharing their Computer are not included in this ownership set.
			if _, err = tx.Exec(ctx, `UPDATE session_processes SET status='stopping' WHERE environment_id=$1 AND session_id=ANY($2) AND fenced_at IS NULL AND status IN ('starting','ready')`, req.EnvironmentID, ids); err != nil {
				return err
			}
		case "close":
			if _, err = tx.Exec(ctx, `UPDATE sessions SET status='closing' WHERE environment_id=$1 AND id=$2 AND status='open'`, req.EnvironmentID, req.SessionID); err != nil {
				return err
			}
			if err = drainClosingSessions(ctx, tx, req.EnvironmentID, ids); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `INSERT INTO session_controls(environment_id,id,session_id,kind,caller_kind,caller_id,retry_key,request_digest,hold_id)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, req.EnvironmentID, result.ID, req.SessionID, req.Kind, caller.Kind, caller.ID, req.RetryKey, digest[:], nullableID(result.HoldID)); err != nil {
			return err
		}
		data, err := json.Marshal(struct {
			ControlID uuid.UUID `json:"controlId"`
			HoldID    any       `json:"holdId,omitempty"`
		}{result.ID, nullableID(result.HoldID)})
		if err != nil {
			return err
		}
		return eventData(ctx, tx, req.EnvironmentID, req.SessionID, uuid.Nil(), "session."+req.Kind, data)
	})
	if err != nil {
		return SessionControlReceipt{}, hideMissing(err)
	}
	return result, nil
}

// ReconcileSessionClosure advances an accepted close after Turn settlement.
// It is a control-plane reconciler, not a new externally authorized operation.
func ReconcileSessionClosure(ctx context.Context, pool db.TxBeginner, env, session uuid.UUID) error {
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		// A blocked tree must not exclude other reconciliation candidates.
		// Once acquired, a large tree may finish without a fixed time cutoff.
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
			return err
		}
		var root uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT root_session_id FROM sessions WHERE environment_id=$1 AND id=$2`, env, session).Scan(&root); err != nil {
			return err
		}
		ids, _, err := lockControlTree(ctx, tx, env, root, Caller{})
		if err != nil {
			return err
		}
		return drainClosingSessions(ctx, tx, env, ids)
	})
}

func drainClosingSessions(ctx context.Context, tx pgx.Tx, env uuid.UUID, ids []uuid.UUID) error {
	// A parent seals its children only after its own accepted work drains.
	// Iterate top down so each newly sealed child's work is checked in turn.
	rows, err := tx.Query(ctx, `SELECT id FROM sessions WHERE environment_id=$1 AND id=ANY($2) ORDER BY (parent_session_id IS NOT NULL),id`, env, ids)
	if err != nil {
		return err
	}
	ordered, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, id := range ordered {
		if _, err = tx.Exec(ctx, `UPDATE sessions child SET status='closing'
 FROM sessions parent WHERE child.environment_id=$1 AND parent.environment_id=$1 AND parent.id=$2
 AND child.parent_session_id=parent.id AND parent.status='closing' AND child.status='open'
 AND NOT EXISTS(SELECT 1 FROM turns WHERE environment_id=$1 AND session_id=parent.id AND status IN ('queued','running','finalizing'))`, env, id); err != nil {
			return err
		}
	}
	// Close leaves first. Lifecycle closure requests process stop; it does not
	// release allocation or claim physical convergence.
	slices.Reverse(ordered)
	for _, id := range ordered {
		tag, err := tx.Exec(ctx, `UPDATE sessions s SET status='closed',authority_generation=authority_generation+1
 WHERE environment_id=$1 AND id=$2 AND status='closing'
 AND NOT EXISTS(SELECT 1 FROM turns WHERE environment_id=$1 AND session_id=s.id AND status IN ('queued','running','finalizing'))
 AND NOT EXISTS(SELECT 1 FROM sessions c WHERE c.environment_id=$1 AND c.parent_session_id=s.id AND c.status NOT IN ('closed','cancelled'))`, env, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		if _, err = tx.Exec(ctx, `UPDATE session_processes SET status='stopping' WHERE environment_id=$1 AND session_id=$2 AND status IN ('starting','ready') AND fenced_at IS NULL`, env, id); err != nil {
			return err
		}
		if err = event(ctx, tx, env, id, uuid.Nil(), "session.closed"); err != nil {
			return err
		}
	}
	return nil
}

func authorizeExternalControl(ctx context.Context, tx pgx.Tx, caller Caller, env uuid.UUID, kind string) error {
	var allowed bool
	var err error
	switch caller.Kind {
	case "user":
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM environments e JOIN org_members m ON m.org_id=e.org_id JOIN users u ON u.id=m.user_id
 WHERE e.id=$1 AND e.retired_at IS NULL AND m.user_id=$2 AND m.disabled_at IS NULL AND u.disabled_at IS NULL AND m.role IN ('owner','admin','developer'))`, env, caller.ID).Scan(&allowed)
	case "api_key":
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM api_keys k JOIN environments e ON (e.id,e.project_id,e.org_id)=(k.environment_id,k.project_id,k.org_id)
 WHERE e.id=$1 AND e.retired_at IS NULL AND k.id=$2 AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>clock_timestamp())
 AND k.role IN ('owner','admin','developer') AND $3=ANY(k.permissions))`, env, caller.ID, "sessions."+kind).Scan(&allowed)
	default:
		return ErrDenied
	}
	if err != nil {
		return err
	}
	if !allowed {
		return ErrDenied
	}
	return nil
}

// Lock every involved root before enumerating descendants. Locking the target
// Computer first can deadlock controls of other trees sharing those Computers.
func lockControlTree(ctx context.Context, tx pgx.Tx, env, target uuid.UUID, caller Caller) ([]uuid.UUID, map[uuid.UUID]owners, error) {
	seeds := []uuid.UUID{target}
	if caller.Kind == "session" {
		seeds = append(seeds, caller.ID)
	}
	roots := make([]uuid.UUID, 0, len(seeds))
	for _, id := range seeds {
		var root uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT root_session_id FROM sessions WHERE environment_id=$1 AND id=$2`, env, id).Scan(&root); err != nil {
			return nil, nil, err
		}
		roots = append(roots, root)
	}
	slices.SortFunc(roots, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	for _, root := range slices.Compact(roots) {
		if _, err := tx.Exec(ctx, `SELECT id FROM sessions WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, root); err != nil {
			return nil, nil, err
		}
	}
	rows, err := tx.Query(ctx, `WITH RECURSIVE tree AS (
 SELECT id FROM sessions WHERE environment_id=$1 AND id=$2
 UNION ALL SELECT s.id FROM sessions s JOIN tree t ON s.parent_session_id=t.id WHERE s.environment_id=$1
) SELECT id FROM tree ORDER BY id`, env, target)
	if err != nil {
		return nil, nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, nil, err
	}
	all := slices.Clone(ids)
	if caller.Kind == "session" {
		all = append(all, caller.ID)
	}
	locked, err := lockSessions(ctx, tx, env, all)
	return ids, locked, err
}

func advanceControlAuthority(ctx context.Context, tx pgx.Tx, env uuid.UUID, ids []uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE sessions SET authority_generation=authority_generation+1 WHERE environment_id=$1 AND id=ANY($2)`, env, ids)
	return err
}

// Turn-bound saves remain ordered Computer cuts owned by save reconciliation,
// even after their Turn is interrupted. Never infer capture absence here.
func settleControlledTurns(ctx context.Context, tx pgx.Tx, env uuid.UUID, ids []uuid.UUID, cancel bool) error {
	status := "interrupted"
	if cancel {
		status = "cancelled"
	}
	rows, err := tx.Query(ctx, `UPDATE turns SET status=$3,terminal_at=clock_timestamp(),processing_closed_at=COALESCE(processing_closed_at,clock_timestamp())
 WHERE environment_id=$1 AND session_id=ANY($2) AND (status IN ('running','finalizing') OR ($4 AND status='queued'))
 RETURNING id,session_id,process_epoch`, env, ids, status, cancel)
	if err != nil {
		return err
	}
	type settled struct {
		id, session uuid.UUID
		epoch       *int64
	}
	turns, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (settled, error) {
		var turn settled
		err := row.Scan(&turn.id, &turn.session, &turn.epoch)
		return turn, err
	})
	if err != nil {
		return err
	}
	for _, turn := range turns {
		if turn.epoch != nil {
			if _, err = tx.Exec(ctx, `UPDATE session_processes SET status='stopping' WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 AND fenced_at IS NULL AND status IN ('ready','starting')`, env, turn.session, *turn.epoch); err != nil {
				return err
			}
		}
		if err = cancelTurnAsks(ctx, tx, env, turn.session, turn.id); err != nil {
			return err
		}
		if err = rejectTurnMessages(ctx, tx, env, turn.session, turn.id, true, "turn_terminated"); err != nil {
			return err
		}
		if err = event(ctx, tx, env, turn.session, turn.id, "turn."+status); err != nil {
			return err
		}
	}
	return nil
}
