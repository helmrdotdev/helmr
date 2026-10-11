package agent

import (
	"context"
	"errors"
	"uuid"

	"github.com/jackc/pgx/v5"
)

type slackAdmissionBinding struct{ thread uuid.UUID }

// Owned children join their immutable root's conversation, including when its
// external connection is unavailable. They never acquire a route of their own.
func resolveSlackChildBinding(ctx context.Context, tx pgx.Tx, env, parent uuid.UUID) (slackAdmissionBinding, error) {
	var binding slackAdmissionBinding
	err := tx.QueryRow(ctx, `SELECT t.id FROM sessions s JOIN slack_threads t ON (t.environment_id,t.front_session_id)=(s.environment_id,s.root_session_id) WHERE s.environment_id=$1 AND s.id=$2`, env, parent).Scan(&binding.thread)
	if errors.Is(err, pgx.ErrNoRows) {
		return binding, nil
	}
	return binding, err
}

func insertSlackSource(ctx context.Context, tx pgx.Tx, env, session, thread uuid.UUID) error {
	tag, err := tx.Exec(ctx, `INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id)
 SELECT $1,t.id,s.environment_id,s.id FROM sessions s JOIN slack_threads t ON t.environment_id=s.environment_id AND t.front_session_id=s.root_session_id WHERE s.environment_id=$2 AND s.id=$3 AND t.id=$4`, uuid.NewV7(), env, session, thread)
	if err == nil && tag.RowsAffected() != 1 {
		return ErrDenied
	}
	return err
}
