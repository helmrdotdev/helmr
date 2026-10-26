package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// Allow ten seconds each for an in-flight dispatch query, the control query,
// guest convergence, and delivery/acknowledgment before requiring reconstruction.
const processStopTimeout = 40 * time.Second

// SessionControlRequest identifies one intentional control operation. Acceptance
// is durable; it does not assert that native processes have stopped.
type SessionControlRequest struct {
	EnvironmentID  uuid.UUID
	SessionID      uuid.UUID
	TurnID         uuid.UUID
	Kind           string
	IdempotencyKey string
	ExpectedScope  string
	HoldID         uuid.UUID
	Reason         string
	// Observed binds delayed control delivery to the execution generation it saw.
	Observed *ControlObservation
	// InterruptQueued discards instructions admitted before this interrupt.
	// Native Slack Stop uses it; ordinary holds retain their existing queue.
	InterruptQueued bool
}

type ControlObservation struct {
	AuthorityGeneration int64
	NextTurnSequence    int64
}

type SessionControlReceipt struct {
	ID        uuid.UUID
	SessionID uuid.UUID
	HoldID    uuid.UUID
}

// ControlSession serializes controls with admission and settlement under the
// ownership root. Retried acceptance never repeats its lifecycle transition.
func ControlSession(ctx context.Context, auditPolicy auditpolicy.Policy, pool db.TxBeginner, caller Caller, req SessionControlRequest) (SessionControlReceipt, error) {
	var result SessionControlReceipt
	if (req.ExpectedScope != "" && req.Kind != "close") || req.IdempotencyKey == "" || len(req.IdempotencyKey) > 512 || !utf8.ValidString(req.IdempotencyKey) || strings.ContainsRune(req.IdempotencyKey, 0) ||
		len(req.Reason) > 4096 || !utf8.ValidString(req.Reason) || strings.ContainsRune(req.Reason, 0) || caller.ID == uuid.Nil() ||
		(req.Kind != "interrupt" && req.Kind != "turn_interrupt" && req.Kind != "resume" && req.Kind != "close" && req.Kind != "cancel") ||
		(req.Kind == "turn_interrupt") != (req.TurnID != uuid.Nil()) ||
		(req.Kind == "resume") != (req.HoldID != uuid.Nil()) ||
		(req.Kind != "interrupt" && (req.Reason != "" || req.InterruptQueued || req.Observed != nil)) ||
		(req.Observed != nil && (req.Observed.AuthorityGeneration < 1 || req.Observed.NextTurnSequence < 1)) {
		return result, ErrInvalidInput
	}
	payload, _ := json.Marshal(struct {
		HoldID          uuid.UUID
		Reason          string
		Observed        *ControlObservation
		InterruptQueued bool
		ExpectedScope   string `json:",omitempty"`
	}{req.HoldID, req.Reason, req.Observed, req.InterruptQueued, req.ExpectedScope})
	digest, err := digestJSON(payload)
	if err != nil {
		return result, err
	}
	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if req.Kind == "resume" {
			if err := workergroup.LockExecutionSupply(ctx, tx, req.EnvironmentID); err != nil {
				return err
			}
		}
		if caller.Kind == "session" {
			if caller.Execution.EnvironmentID != req.EnvironmentID || caller.Execution.SessionID != caller.ID {
				return ErrDenied
			}
			if err := lockRuntimeHost(ctx, tx, caller); err != nil {
				return err
			}
		} else if err := authorizeExternalControl(ctx, tx, caller, req.EnvironmentID, req.SessionID, req.Kind); err != nil {
			return err
		}
		if caller.Kind == "session" {
			// Ownership ancestry is immutable. Reject foreign targets before
			// taking their root/Computer locks on behalf of workload input.
			if err := requireOwnedTarget(ctx, tx, req.EnvironmentID, caller.ID, req.SessionID); err != nil {
				return err
			}
		}
		if _, err := org.LockAdmission(ctx, tx, req.EnvironmentID); err != nil {
			return err
		}
		var ids []uuid.UUID
		var locked map[uuid.UUID]owners
		var err error
		if req.Kind == "turn_interrupt" {
			ids = []uuid.UUID{req.SessionID}
			targets := slices.Clone(ids)
			if caller.Kind == "session" {
				targets = append(targets, caller.ID)
			}
			locked, err = lockSessions(ctx, tx, req.EnvironmentID, targets)
		} else {
			ids, locked, err = lockControlTree(ctx, tx, req.EnvironmentID, req.SessionID, caller)
		}
		if err != nil {
			return err
		}
		if caller.Kind == "session" {
			if err = executionReceipt(ctx, tx, caller.Execution, locked[caller.ID]); err != nil {
				return err
			}
		} else if err = authorizeExternalControl(ctx, tx, caller, req.EnvironmentID, req.SessionID, req.Kind); err != nil {
			return err
		}
		for _, id := range ids {
			authorize := authorizeTarget
			if req.Kind == "resume" && id != req.SessionID && (locked[id].lifecycle == "closed" || locked[id].lifecycle == "cancelled") {
				// Settled descendants cannot resume execution. Retain authority
				// checks without making their deleted Agent block the live tree.
				authorize = authorizeTargetAuthority
			}
			if err := authorize(ctx, tx, caller, req.EnvironmentID, id, controlPermission(req.Kind)); err != nil {
				return err
			}
		}
		var prior []byte
		var hold *uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id,hold_id,request_digest FROM session_controls
 WHERE environment_id=$1 AND session_id=$2 AND caller_kind=$3 AND caller_id=$4 AND kind=$5 AND idempotency_key=$6 AND turn_id IS NOT DISTINCT FROM $7`,
			req.EnvironmentID, req.SessionID, caller.Kind, caller.ID, req.Kind, req.IdempotencyKey, nullableID(req.TurnID)).Scan(&result.ID, &hold, &prior)
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
		if req.ExpectedScope != "" {
			scope, err := ReadSessionCloseScope(ctx, tx, req.EnvironmentID, req.SessionID)
			if err != nil {
				return err
			}
			if scope.Digest != req.ExpectedScope {
				return ErrControlScopeChanged
			}
		}
		if req.Observed != nil {
			var matches bool
			if err = tx.QueryRow(ctx, `SELECT authority_generation=$3 AND next_turn_seq=$4 FROM sessions WHERE environment_id=$1 AND id=$2`, req.EnvironmentID, req.SessionID, req.Observed.AuthorityGeneration, req.Observed.NextTurnSequence).Scan(&matches); err != nil {
				return err
			}
			if !matches {
				return ErrNotReady
			}
		}
		result = SessionControlReceipt{ID: uuid.NewV7(), SessionID: req.SessionID, HoldID: req.HoldID}
		switch req.Kind {
		case "turn_interrupt":
			var state string
			var epoch *int64
			if err = tx.QueryRow(ctx, `SELECT status,process_epoch FROM turns WHERE environment_id=$1 AND session_id=$2 AND id=$3 FOR NO KEY UPDATE`, req.EnvironmentID, req.SessionID, req.TurnID).Scan(&state, &epoch); err != nil {
				return err
			}
			if state != "running" {
				return TurnNotInterruptibleError{Status: state}
			}
			if epoch == nil {
				return ErrNotReady
			}
			if err = interruptControlledTurn(ctx, auditPolicy, tx, req.EnvironmentID, req.SessionID, req.TurnID, *epoch); err != nil {
				return err
			}
			if err = auditOperation(ctx, tx, auditPolicy, caller, auth.PermissionSessionsInterrupt, audit.Event{EnvironmentID: req.EnvironmentID, SessionID: req.SessionID, TurnID: req.TurnID, Action: "turn.interrupted", TargetKind: "turn", TargetID: req.TurnID, Details: audit.LifecycleDetails{RequestID: result.ID}}); err != nil {
				return err
			}
			if err = advanceControlAuthority(ctx, tx, req.EnvironmentID, []uuid.UUID{req.SessionID}); err != nil {
				return err
			}
		case "interrupt":
			if locked[req.SessionID].lifecycle == "closed" || locked[req.SessionID].lifecycle == "cancelled" {
				return ErrNotReady
			}
			result.HoldID = uuid.NewV7()
			if _, err = tx.Exec(ctx, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason,issuer_kind,issuer_id)
 VALUES($1,$2,$3,'subtree',$4,$5,$6)`, req.EnvironmentID, result.HoldID, req.SessionID, req.Reason, caller.Kind, caller.ID); err != nil {
				return err
			}
			if err = settleControlledTurns(ctx, auditPolicy, tx, caller, req.EnvironmentID, ids, false, req.InterruptQueued); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE session_processes SET stop_sequence=stop_sequence+1,stop_deadline_at=clock_timestamp()+$3::bigint*interval '1 millisecond',control_acknowledged_at=NULL WHERE environment_id=$1 AND session_id=ANY($2) AND status='starting' AND fenced_at IS NULL AND stop_deadline_at IS NULL`, req.EnvironmentID, ids, processStopTimeout.Milliseconds()); err != nil {
				return err
			}
			if err = advanceControlAuthority(ctx, tx, req.EnvironmentID, ids); err != nil {
				return err
			}
		case "resume":
			for _, o := range locked {
				if o.computer != uuid.Nil() {
					if err = computer.RetireUnusedExecutionAdmission(ctx, tx, req.EnvironmentID, o.computer); err != nil {
						return err
					}
				}
			}
			var issuer, scope, holdKind string
			var issuerID *uuid.UUID
			if err = tx.QueryRow(ctx, `SELECT issuer_kind,issuer_id,scope,kind FROM session_holds WHERE environment_id=$1 AND session_id=$2 AND id=$3`, req.EnvironmentID, req.SessionID, req.HoldID).Scan(&issuer, &issuerID, &scope, &holdKind); err != nil {
				return err
			}
			if holdKind == "storage_pressure" {
				return ErrNotReady
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
			if err = settleControlledTurns(ctx, auditPolicy, tx, caller, req.EnvironmentID, ids, true, false); err != nil {
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
				if err = auditOperation(ctx, tx, auditPolicy, caller, auth.PermissionSessionsCancel, audit.Event{EnvironmentID: req.EnvironmentID, SessionID: id, Action: "session.cancelled", Outcome: "cancelled", TargetKind: "session", TargetID: id, Details: audit.LifecycleDetails{RequestID: result.ID}}); err != nil {
					return err
				}
				if err = event(ctx, tx, req.EnvironmentID, id, uuid.Nil(), "session.cancelled"); err != nil {
					return err
				}
			}
			// Idle processes must also stop on cancellation; independent peers
			// sharing their Computer are not included in this ownership set.
			stoppingRows, err := tx.Query(ctx, `UPDATE session_processes SET status='stopping' WHERE environment_id=$1 AND session_id=ANY($2) AND fenced_at IS NULL AND status IN ('starting','ready') RETURNING session_id`, req.EnvironmentID, ids)
			if err != nil {
				return err
			}
			stopping, err := pgx.CollectRows(stoppingRows, pgx.RowTo[uuid.UUID])
			if err != nil {
				return err
			}
			if len(stopping) > 0 {
				if err = notifySessionsChanged(ctx, tx, req.EnvironmentID, stopping...); err != nil {
					return err
				}
			}
		case "close":
			if _, err = tx.Exec(ctx, `UPDATE sessions SET status='closing' WHERE environment_id=$1 AND id=$2 AND status='open'`, req.EnvironmentID, req.SessionID); err != nil {
				return err
			}
			if err = drainClosingSessions(ctx, auditPolicy, tx, req.EnvironmentID, ids); err != nil {
				return err
			}
		}
		// All control-tree Computers are locked. Resume only requests capacity for
		// work actually released by this operation; overlapping holds remain effective.
		seen := map[uuid.UUID]bool{}
		for _, sessionID := range ids {
			id := locked[sessionID].computer
			if id == uuid.Nil() || seen[id] {
				continue
			}
			seen[id] = true
			if req.Kind == "resume" {
				var runnable bool
				if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM turns t JOIN sessions s ON (s.environment_id,s.id)=(t.environment_id,t.session_id) WHERE s.environment_id=$1 AND s.id=ANY($2) AND s.computer_id=$3 AND t.status='queued' AND `+db.NoSessionHoldsSQL+`)`, req.EnvironmentID, ids, id).Scan(&runnable); err != nil {
					return err
				}
				if runnable {
					if err = computer.NoteExecutionDemand(ctx, auditPolicy, tx, req.EnvironmentID, id); err != nil {
						return err
					}
				}
			} else if err = computer.RetireUnusedExecutionAdmission(ctx, tx, req.EnvironmentID, id); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `INSERT INTO session_controls(environment_id,id,session_id,kind,caller_kind,caller_id,idempotency_key,request_digest,hold_id,turn_id)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, req.EnvironmentID, result.ID, req.SessionID, req.Kind, caller.Kind, caller.ID, req.IdempotencyKey, digest[:], nullableID(result.HoldID), nullableID(req.TurnID)); err != nil {
			return err
		}
		action, targetKind, targetID := "session."+req.Kind+"_requested", "session", req.SessionID
		if req.Kind == "turn_interrupt" {
			action, targetKind, targetID = "turn.interrupt_requested", "turn", req.TurnID
		}
		if err = auditOperation(ctx, tx, auditPolicy, caller, controlPermission(req.Kind), audit.Event{EnvironmentID: req.EnvironmentID, SessionID: req.SessionID, TurnID: req.TurnID, Action: action, Outcome: "accepted", TargetKind: targetKind, TargetID: targetID, Details: audit.SessionControlDetails{RequestID: result.ID, HoldID: result.HoldID}}); err != nil {
			return err
		}
		data, err := json.Marshal(struct {
			ControlID uuid.UUID `json:"controlId"`
			HoldID    any       `json:"holdId,omitempty"`
		}{result.ID, nullableID(result.HoldID)})
		if err != nil {
			return err
		}
		if req.Kind == "turn_interrupt" {
			return eventData(ctx, tx, req.EnvironmentID, req.SessionID, req.TurnID, "turn.interrupt", data)
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
func ReconcileSessionClosure(ctx context.Context, auditPolicy auditpolicy.Policy, pool db.TxBeginner, env, session uuid.UUID) error {
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
		return drainClosingSessions(ctx, auditPolicy, tx, env, ids)
	})
}

func drainClosingSessions(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, env uuid.UUID, ids []uuid.UUID) error {
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
		rows, err := tx.Query(ctx, `UPDATE sessions child SET status='closing'
 FROM sessions parent WHERE child.environment_id=$1 AND parent.environment_id=$1 AND parent.id=$2
 AND child.parent_session_id=parent.id AND parent.status='closing' AND child.status='open'
 AND NOT EXISTS(SELECT 1 FROM turns WHERE environment_id=$1 AND session_id=parent.id AND status IN ('queued','running')) RETURNING child.id`, env, id)
		if err != nil {
			return err
		}
		changed, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		if len(changed) > 0 {
			if err := notifySessionsChanged(ctx, tx, env, changed...); err != nil {
				return err
			}
		}
	}
	// Close leaves first. Lifecycle closure requests process stop; it does not
	// release allocation or claim physical convergence.
	slices.Reverse(ordered)
	for _, id := range ordered {
		tag, err := tx.Exec(ctx, `UPDATE sessions s SET status='closed',authority_generation=authority_generation+1
 WHERE environment_id=$1 AND id=$2 AND status='closing'
 AND NOT EXISTS(SELECT 1 FROM turns WHERE environment_id=$1 AND session_id=s.id AND status IN ('queued','running'))
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
		if err = auditServiceFact(ctx, tx, auditPolicy, audit.ControlPlane, audit.Event{EnvironmentID: env, SessionID: id, Action: "session.closed", TargetKind: "session", TargetID: id, Details: audit.LifecycleDetails{}}); err != nil {
			return err
		}
		if err = event(ctx, tx, env, id, uuid.Nil(), "session.closed"); err != nil {
			return err
		}
	}
	return nil
}

func authorizeExternalControl(ctx context.Context, tx pgx.Tx, caller Caller, env, target uuid.UUID, kind string) error {
	return authorizeTarget(ctx, tx, caller, env, target, controlPermission(kind))
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
	_, err := tx.Exec(ctx, `UPDATE sessions SET authority_generation=authority_generation+1,idle_since=NULL WHERE environment_id=$1 AND id=ANY($2)`, env, ids)
	if err != nil {
		return err
	}
	return notifySessionsChanged(ctx, tx, env, ids...)
}

// Turn-bound saves remain ordered Computer cuts owned by save reconciliation,
// even after their Turn is interrupted. Never infer capture absence here.
func settleControlledTurns(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, caller Caller, env uuid.UUID, ids []uuid.UUID, cancel, interruptQueued bool) error {
	status := "interrupted"
	if cancel {
		status = "cancelled"
	}
	rows, err := tx.Query(ctx, `UPDATE turns SET status=$3,terminal_at=clock_timestamp(),processing_closed_at=COALESCE(processing_closed_at,clock_timestamp())
 WHERE environment_id=$1 AND session_id=ANY($2) AND (status='running' OR ($4 AND status IN ('queued')) OR ($5 AND status='queued'))
 RETURNING id,session_id,process_epoch`, env, ids, status, cancel, interruptQueued)
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
		if turn.epoch != nil && !cancel {
			if err = requestProcessStop(ctx, tx, env, turn.session, turn.id, *turn.epoch); err != nil {
				return err
			}
		}
		if turn.epoch != nil && cancel {
			if _, err = tx.Exec(ctx, `UPDATE session_processes SET status='stopping' WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 AND fenced_at IS NULL AND status IN ('ready','starting')`, env, turn.session, *turn.epoch); err != nil {
				return err
			}
		}
		if err = cancelTurnAsks(ctx, auditPolicy, tx, env, turn.session, turn.id); err != nil {
			return err
		}
		if err = rejectTurnMessages(ctx, auditPolicy, tx, env, turn.session, turn.id, true, "turn_terminated"); err != nil {
			return err
		}
		permission, outcome := auth.PermissionSessionsInterrupt, "succeeded"
		if cancel {
			permission, outcome = auth.PermissionSessionsCancel, "cancelled"
		}
		if err = auditOperation(ctx, tx, auditPolicy, caller, permission, audit.Event{EnvironmentID: env, SessionID: turn.session, TurnID: turn.id, Action: "turn." + status, Outcome: outcome, TargetKind: "turn", TargetID: turn.id, Details: audit.LifecycleDetails{}}); err != nil {
			return err
		}
		if err = event(ctx, tx, env, turn.session, turn.id, "turn."+status); err != nil {
			return err
		}
	}
	return nil
}

// The first stop owns its deadline and target until current-epoch convergence.
// Hold release and transport reattachment cannot extend this budget.
func requestProcessStop(ctx context.Context, tx pgx.Tx, env, session, turn uuid.UUID, epoch int64) error {
	_, err := tx.Exec(ctx, `UPDATE session_processes SET stop_sequence=stop_sequence+1,stop_turn_id=$4,stop_deadline_at=clock_timestamp()+$5::bigint*interval '1 millisecond',control_acknowledged_at=NULL
 WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 AND fenced_at IS NULL AND stop_deadline_at IS NULL`, env, session, epoch, nullableID(turn), processStopTimeout.Milliseconds())
	return err
}

func interruptControlledTurn(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, env, session, turn uuid.UUID, epoch int64) error {
	if _, err := tx.Exec(ctx, `UPDATE turns SET status='interrupted',terminal_at=clock_timestamp(),processing_closed_at=COALESCE(processing_closed_at,clock_timestamp()) WHERE environment_id=$1 AND session_id=$2 AND id=$3 AND status='running'`, env, session, turn); err != nil {
		return err
	}
	if err := requestProcessStop(ctx, tx, env, session, turn, epoch); err != nil {
		return err
	}
	if err := cancelTurnAsks(ctx, auditPolicy, tx, env, session, turn); err != nil {
		return err
	}
	if err := rejectTurnMessages(ctx, auditPolicy, tx, env, session, turn, true, "turn_terminated"); err != nil {
		return err
	}
	return event(ctx, tx, env, session, turn, "turn.interrupted")
}

// TurnNotInterruptibleError describes the exact authorized target's cutoff.
type TurnNotInterruptibleError struct{ Status string }

func (e TurnNotInterruptibleError) Reason() string {
	switch e.Status {
	case "queued":
		return "not_started"
	default:
		return "already_terminal"
	}
}
func (e TurnNotInterruptibleError) Error() string     { return "Turn is " + e.Status + ": " + e.Reason() }
func (e TurnNotInterruptibleError) ErrorCode() string { return "turn_not_interruptible" }
func (e TurnNotInterruptibleError) ErrorDetails() map[string]json.RawMessage {
	status, _ := json.Marshal(e.Status)
	reason, _ := json.Marshal(e.Reason())
	return map[string]json.RawMessage{"status": status, "reason": reason}
}

func controlPermission(kind string) auth.Permission {
	switch kind {
	case "close":
		return auth.PermissionSessionsClose
	case "cancel":
		return auth.PermissionSessionsCancel
	case "resume":
		return auth.PermissionSessionsResume
	default:
		return auth.PermissionSessionsInterrupt
	}
}
