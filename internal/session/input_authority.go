package session

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// ConversationVerifier checks current external participation without holding SQL
// locks. A temporary provider failure differs from a confirmed denial.
type ConversationVerifier interface {
	VerifyInputConversation(context.Context, uuid.UUID, int64, string, string, string) (bool, error)
}

type inputAuthorityCheck struct {
	request uuid.UUID
	allowed bool
}

// Only the next unstarted input needs fresh conversation membership. Already
// dispatched input retains its receipt semantics. Admission checks the exact
// request again after the external read, so a concurrent input cannot borrow it.
func checkInputConversation(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, attachment int64, turn uuid.UUID, verifier ConversationVerifier) (inputAuthorityCheck, error) {
	var check inputAuthorityCheck
	var installation, user, thread uuid.UUID
	var revoked bool
	var revision int64
	var team, channel, actor string
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := lockRuntimeAuthority(ctx, tx, host, e); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT r.id,i.id,i.credential_revision,t.team_id,t.slack_channel_id,r.slack_user_id,r.user_id,t.id
 FROM slack_requests r JOIN slack_threads t ON t.id=r.thread_id JOIN slack_installations i ON i.id=r.installation_id
 WHERE r.environment_id=$1 AND r.session_id=$2 AND r.status='accepted' AND
 (($3::uuid IS NULL AND r.message_id IS NULL AND r.turn_id=(SELECT id FROM turns WHERE environment_id=$1 AND session_id=$2 AND status='queued' ORDER BY seq LIMIT 1) AND NOT EXISTS(SELECT 1 FROM turns WHERE environment_id=$1 AND session_id=$2 AND status='running')) OR
 ($3::uuid IS NOT NULL AND r.message_id=(SELECT id FROM turn_messages WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND status='admitted' ORDER BY seq LIMIT 1) AND NOT EXISTS(SELECT 1 FROM turn_messages WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND status='started')))
 `, e.EnvironmentID, e.SessionID, nullableID(turn)).Scan(&check.request, &installation, &revision, &team, &channel, &actor, &user, &thread)
		if err != nil {
			return err
		}
		// Check dispatchability before consulting Slack. These checks are repeated
		// at admission after the external read; no SQL lock crosses that read.
		if err := currentMessageAttachment(ctx, tx, e, attachment); err != nil {
			return err
		}
		if turn == uuid.Nil() {
			o, err := dispatchReady(ctx, tx, e)
			if err != nil {
				return err
			}
			if err := newTurnReady(ctx, tx, e, o); err != nil {
				return err
			}
		} else {
			if err := lockRuntimeOperationOwner(ctx, tx, host, e); err != nil {
				return err
			}
			if err := lockRuntimeContentTurn(ctx, tx, e, turn); err != nil {
				return err
			}
			if err := runtimeContentReady(ctx, tx, e, turn, false); err != nil {
				return err
			}
		}
		err = authorizeSlackInputActor(ctx, tx, check.request)
		if err == nil {
			err = authorizeRuntimeOrigin(ctx, tx, e.EnvironmentID, e.SessionID)
		}
		if err == nil {
			err = authorizeTarget(ctx, tx, Caller{Kind: "user", ID: user, SlackThreadID: thread}, e.EnvironmentID, e.SessionID, auth.PermissionSessionsSend)
		}
		if errors.Is(err, ErrDenied) {
			revoked = true
			return nil
		}
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return check, nil
	}
	if err != nil {
		return check, err
	}
	if revoked {
		return check, nil
	}
	if verifier == nil {
		return check, ErrNotReady
	}
	check.allowed, err = verifier.VerifyInputConversation(ctx, installation, revision, team, channel, actor)
	if err != nil {
		return check, ErrNotReady
	}
	return check, nil
}

// A retained request is attributable to its exact linked Slack actor, not only
// its Helmr user. Hold the live link through admission so unlink cannot race it.
func authorizeSlackInputActor(ctx context.Context, tx pgx.Tx, request uuid.UUID) error {
	var user uuid.UUID
	err := tx.QueryRow(ctx, `SELECT l.user_id FROM slack_requests r
 JOIN slack_installations i ON i.id=r.installation_id
 JOIN slack_user_links l ON l.team_id=i.team_id AND l.slack_user_id=r.slack_user_id AND l.user_id=r.user_id
 WHERE r.id=$1 AND l.linked_at<=r.source_occurred_at FOR SHARE OF l`, request).Scan(&user)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDenied
	}
	return err
}
