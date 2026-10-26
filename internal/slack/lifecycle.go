package slack

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func lifecycleAction(action string) bool {
	switch action {
	case "sleep", "wake", "close", "help", "hibernate", "confirm_close", "mute", "unmute":
		return true
	}
	return false
}

func admitLifecycleMessage(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, id uuid.UUID, p *messageAdmission, action string) error {
	// Input controls retain their timestamp ordering and do not enter execution admission.
	if action == "mute" || action == "unmute" {
		_, err := applyThreadInput(ctx, tx, id, p, action)
		return err
	}
	if p.session == uuid.Nil() {
		return rejectMessage(ctx, tx, id, "command_unbound")
	}
	target := controlTarget{Installation: p.installation, Thread: p.thread, Participant: p.source, Environment: p.route.Target.EnvironmentID, Session: p.session}
	err := db.RunTx(ctx, tx, func(core pgx.Tx) error {
		_, err := applyLifecycleControl(ctx, auditPolicy, core, session.Caller{CredentialKind: "slack", Kind: "user", ID: p.user, SlackThreadID: p.thread}, target, controlEnvelope{Action: action}, id)
		if err != nil {
			return err
		}
		var unexpired bool
		if err := core.QueryRow(ctx, `SELECT expires_at>clock_timestamp() FROM slack_requests WHERE id=$1`, id).Scan(&unexpired); err != nil {
			return err
		}
		if !unexpired {
			return errAdmissionExpired
		}
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, errAdmissionExpired):
			return rejectMessage(ctx, tx, id, "gesture_expired")
		case errors.Is(err, session.ErrDenied), errors.Is(err, computer.ErrDenied):
			return rejectMessage(ctx, tx, id, "permission_denied")
		case errors.Is(err, computer.ErrExecutionSettling):
			return rejectMessage(ctx, tx, id, "execution_settling")
		case errors.Is(err, workergroup.ErrCapacityUnavailable):
			return rejectMessage(ctx, tx, id, "execution_capacity_unavailable")
		case errors.Is(err, workergroup.ErrCapacityIncompatible):
			return rejectMessage(ctx, tx, id, "execution_capacity_incompatible")
		case errors.Is(err, workergroup.ErrCapacityExhausted):
			return rejectMessage(ctx, tx, id, "execution_capacity_exhausted")
		case errors.Is(err, computer.ErrExecutionLimit):
			return rejectMessage(ctx, tx, id, "execution_limit")
		case errors.Is(err, computer.ErrBusy):
			return rejectMessage(ctx, tx, id, "computer_busy")
		case errors.Is(err, session.ErrNotReady), errors.Is(err, session.ErrTerminal), errors.Is(err, pgx.ErrNoRows):
			return rejectMessage(ctx, tx, id, "session_unavailable")
		default:
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE slack_requests SET status='accepted',finished_at=clock_timestamp(),user_id=$2,thread_id=$3,thread_source_id=$4,environment_id=$5,session_id=$6,operation=$7 WHERE id=$1`, id, p.user, p.thread, p.source, target.Environment, target.Session, action)
	return err
}

func applyLifecycleControl(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, caller session.Caller, target controlTarget, c controlEnvelope, id uuid.UUID) (json.RawMessage, error) {
	operation := auth.PermissionSessionsRead
	if c.Action == "mute" || c.Action == "unmute" {
		operation = auth.PermissionSessionsInterrupt
	}
	if err := session.AuthorizeSessionOperation(ctx, tx, caller, target.Environment, target.Session, operation); err != nil {
		return nil, err
	}

	var computerID *uuid.UUID
	var status string
	if err := tx.QueryRow(ctx, `SELECT computer_id,status FROM sessions WHERE environment_id=$1 AND id=$2`, target.Environment, target.Session).Scan(&computerID, &status); err != nil {
		return nil, err
	}
	if status != "open" && c.Action != "help" {
		return nil, session.ErrTerminal
	}
	if c.Action == "sleep" {
		err := access.Check(ctx, tx, caller.Kind, caller.ID, target.Environment, uuid.Nil(), auth.PermissionComputersHibernate)
		if errors.Is(err, access.ErrDenied) {
			return nil, session.ErrDenied
		}
		if err != nil {
			return nil, err
		}
	}
	if c.Action == "close" {
		if _, err := session.SessionCloseScope(ctx, tx, caller, target.Environment, target.Session); err != nil {
			return nil, err
		}
	}
	switch c.Action {
	case "wake", "hibernate":
		if computerID == nil || (c.Action == "hibernate" && *computerID != c.Computer) {
			return nil, session.ErrNotReady
		}
		receipt, err := session.ControlComputerExecution(ctx, auditPolicy, tx, caller, session.ComputerExecutionRequest{EnvironmentID: target.Environment, ComputerID: *computerID, Kind: c.Action, IdempotencyKey: "slack:" + id.String(), ExpectedScope: c.Scope, OpenSessionID: target.Session})
		if err != nil {
			return nil, err
		}
		return json.Marshal(receipt)
	case "confirm_close":
		receipt, err := session.ControlSession(ctx, auditPolicy, tx, caller, session.SessionControlRequest{EnvironmentID: target.Environment, SessionID: target.Session, Kind: "close", IdempotencyKey: "slack:" + id.String(), ExpectedScope: c.Scope})
		if err != nil {
			return nil, err
		}
		return json.Marshal(receipt)
	case "mute", "unmute":
		// Button gestures use the same source-time ordering as typed input controls.
		var changed bool
		err := tx.QueryRow(ctx, `WITH updated AS (UPDATE slack_threads t SET input_muted=$3,input_changed_at=r.source_occurred_at,input_unmuted_at=CASE WHEN t.input_muted AND NOT $3 THEN r.source_occurred_at ELSE t.input_unmuted_at END FROM slack_requests r WHERE t.id=$1 AND r.id=$2 AND (t.input_changed_at IS NULL OR r.source_occurred_at>t.input_changed_at) RETURNING t.id) SELECT EXISTS(SELECT 1 FROM updated)`, target.Thread, id, c.Action == "mute").Scan(&changed)
		if err != nil {
			return nil, err
		}
		if !changed {
			return nil, session.ErrConflict
		}
	}
	return json.RawMessage(`{}`), nil
}
