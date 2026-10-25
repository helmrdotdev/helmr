package session

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"uuid"
)

type AgentDeletionBlocker struct {
	Code              string
	Count             int64
	SessionIDs        []string
	SamplesIncomplete bool
}

var ErrAgentDeleted = errors.New("agent is deleted; restore it before execution")

// AgentDeletionBlockers shares execution settlement predicates with history
// retention. Pending Slack delivery is instead suppressed by connection cleanup.
// The caller holds the Environment admission fence; samples never grant access.
func AgentDeletionBlockers(ctx context.Context, tx pgx.Tx, env, agent uuid.UUID, visible bool) ([]AgentDeletionBlocker, error) {
	rows, err := tx.Query(ctx, `WITH blockers AS (
 SELECT s.id,CASE WHEN s.status IN ('open','closing') THEN 'sessions_active' ELSE 'cleanup_pending' END AS code
 FROM sessions s WHERE s.environment_id=$1 AND s.agent_id=$2 AND NOT (`+executionReleased+`)
 ), ranked AS (SELECT id,code,row_number() OVER(PARTITION BY code ORDER BY id) AS n FROM blockers)
 SELECT code,count(*),COALESCE(array_agg(id ORDER BY id) FILTER(WHERE n<=20),'{}'::uuid[]) FROM ranked GROUP BY code ORDER BY code`, env, agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []AgentDeletionBlocker{}
	for rows.Next() {
		var b AgentDeletionBlocker
		var ids []uuid.UUID
		if err := rows.Scan(&b.Code, &b.Count, &ids); err != nil {
			return nil, err
		}
		if visible {
			for _, id := range ids {
				b.SessionIDs = append(b.SessionIDs, id.String())
			}
		}
		b.SamplesIncomplete = b.Count > int64(len(b.SessionIDs))
		result = append(result, b)
	}
	return result, rows.Err()
}

func requireAgentAvailable(ctx context.Context, q db.DBTX, env, agent uuid.UUID) error {
	var deleted bool
	if err := q.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM agents WHERE environment_id=$1 AND id=$2`, env, agent).Scan(&deleted); err != nil {
		return err
	}
	if deleted {
		return ErrAgentDeleted
	}
	return nil
}
