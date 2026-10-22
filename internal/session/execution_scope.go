package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// ExecutionScope binds a human confirmation to the exact members and control
// generations it displayed. It is an observation, never an authorization grant.
type ExecutionScope struct {
	Digest   string   `json:"digest"`
	Sessions []string `json:"sessions"`
	Commands []string `json:"commands"`
}

func ReadComputerExecutionScope(ctx context.Context, tx pgx.Tx, env, computerID uuid.UUID) (ExecutionScope, error) {
	rows, err := tx.Query(ctx, `SELECT 'session',id::text,authority_generation::text FROM sessions WHERE environment_id=$1 AND computer_id=$2 AND status IN ('open','closing')
 UNION ALL SELECT 'command',id::text,status FROM commands WHERE environment_id=$1 AND computer_id=$2 AND (status='pending' OR (computer_lease_epoch IS NOT NULL AND process_reconciled_at IS NULL)) ORDER BY 1,2`, env, computerID)
	if err != nil {
		return ExecutionScope{}, err
	}
	return readExecutionScope(rows, computerID)
}

func ReadSessionCloseScope(ctx context.Context, tx pgx.Tx, env, sessionID uuid.UUID) (ExecutionScope, error) {
	rows, err := tx.Query(ctx, `WITH RECURSIVE tree AS (SELECT id,authority_generation FROM sessions WHERE environment_id=$1 AND id=$2
 UNION ALL SELECT s.id,s.authority_generation FROM sessions s JOIN tree t ON s.parent_session_id=t.id WHERE s.environment_id=$1)
 SELECT 'session',id::text,authority_generation::text FROM tree ORDER BY id`, env, sessionID)
	if err != nil {
		return ExecutionScope{}, err
	}
	return readExecutionScope(rows, sessionID)
}

func readExecutionScope(rows pgx.Rows, target uuid.UUID) (ExecutionScope, error) {
	result := ExecutionScope{Sessions: []string{}, Commands: []string{}}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var kind, id, generation string
		if err := rows.Scan(&kind, &id, &generation); err != nil {
			return result, err
		}
		parts = append(parts, kind+":"+id+":"+generation)
		if kind == "session" {
			result.Sessions = append(result.Sessions, id)
		} else {
			result.Commands = append(result.Commands, id)
		}
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	sum := sha256.Sum256([]byte(target.String() + ":" + strings.Join(parts, "|")))
	result.Digest = hex.EncodeToString(sum[:])
	return result, nil
}
