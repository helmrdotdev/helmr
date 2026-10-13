package slackroute

import (
	"context"
	"errors"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// RootThread returns the Slack thread whose route the Session's root owns, or
// uuid.Nil when the root has no Slack route. Owned children join their
// immutable root's conversation, including when its connection is unavailable;
// they never acquire a route of their own.
func RootThread(ctx context.Context, tx pgx.Tx, env, session uuid.UUID) (uuid.UUID, error) {
	var thread uuid.UUID
	err := tx.QueryRow(ctx, `SELECT t.id FROM sessions s JOIN slack_threads t ON (t.environment_id,t.front_session_id)=(s.environment_id,s.root_session_id) WHERE s.environment_id=$1 AND s.id=$2`, env, session).Scan(&thread)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil(), nil
	}
	return thread, err
}

// JoinRootThread adds a Session to the thread its root owns. It reports false
// when the thread is not the one the Session's root owns.
func JoinRootThread(ctx context.Context, tx pgx.Tx, env, session, thread uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id)
 SELECT $1,t.id,s.environment_id,s.id FROM sessions s JOIN slack_threads t ON t.environment_id=s.environment_id AND t.front_session_id=s.root_session_id WHERE s.environment_id=$2 AND s.id=$3 AND t.id=$4`, uuid.NewV7(), env, session, thread)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// AddMember records the Session as a member of the thread and returns the
// membership.
func AddMember(ctx context.Context, tx pgx.Tx, env, thread, session uuid.UUID) (uuid.UUID, error) {
	id := uuid.NewV7()
	_, err := tx.Exec(ctx, `INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id) VALUES($1,$2,$3,$4)`, id, thread, env, session)
	return id, err
}

// AdvanceProjection records that the member's events up to seq are disposed.
func AdvanceProjection(ctx context.Context, tx pgx.Tx, member uuid.UUID, seq int64) error {
	_, err := tx.Exec(ctx, `UPDATE slack_thread_sources SET projected_event_seq=$2 WHERE id=$1`, member, seq)
	return err
}

// SkipInstallationProjection disposes every unread event of members whose
// thread is routed through the installation.
func SkipInstallationProjection(ctx context.Context, tx pgx.Tx, installation uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE slack_thread_sources p SET projected_event_seq=GREATEST(p.projected_event_seq,s.next_event_seq-1)
 FROM sessions s,slack_threads t,slack_channel_bindings c WHERE p.environment_id=s.environment_id AND p.session_id=s.id
 AND t.id=p.thread_id AND c.id=t.channel_id AND c.installation_id=$1`, installation)
	return err
}

// ChannelOf returns the channel binding of the Slack route a root Session
// owns, or nil when it owns none. The thread row is the route authority.
func ChannelOf(ctx context.Context, tx pgx.Tx, env, session uuid.UUID) (*uuid.UUID, error) {
	var route uuid.UUID
	err := tx.QueryRow(ctx, `SELECT channel_id FROM slack_threads WHERE environment_id=$1 AND front_session_id=$2`, env, session).Scan(&route)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &route, nil
}
