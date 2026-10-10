package agent

import (
	"context"
	"encoding/base64"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
)

type RuntimeAgentDefinition struct {
	ID string `json:"id"`
}

type RuntimeAgentPage struct {
	DeploymentID uuid.UUID                `json:"deploymentId"`
	Agents       []RuntimeAgentDefinition `json:"agents"`
	NextCursor   string                   `json:"nextCursor,omitempty"`
}

// Creation discovery uses one immutable Deployment snapshot. Spawn uses the
// caller's pin; independent start uses the Environment's current promotion.
func RuntimeListAgents(ctx context.Context, pool db.TxBeginner, caller Caller, operation, cursor string) (RuntimeAgentPage, error) {
	result := RuntimeAgentPage{Agents: []RuntimeAgentDefinition{}}
	if operation != "spawn" && operation != "start" {
		return result, ErrInvalidInput
	}
	var expected uuid.UUID
	after := ""
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		parts := strings.Split(string(raw), ":")
		if err != nil || len(cursor) > 1024 || len(parts) != 3 || parts[0] != operation || !definition.ValidDeclaredID(parts[2]) {
			return result, ErrInvalidInput
		}
		expected, err = ids.Parse(parts[1])
		if err != nil || expected == uuid.Nil() {
			return result, ErrInvalidInput
		}
		after = parts[2]
	}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := LockRuntimeCaller(ctx, tx, caller); err != nil {
			return err
		}
		if err := requireRootCaller(ctx, tx, caller); err != nil {
			return err
		}
		var selected *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT CASE WHEN $3='spawn' THEN s.deployment_id ELSE e.current_deployment_id END FROM sessions s JOIN environments e ON e.id=s.environment_id WHERE s.environment_id=$1 AND s.id=$2 AND e.retired_at IS NULL`, caller.Execution.EnvironmentID, caller.ID, operation).Scan(&selected); err != nil {
			return err
		}
		if selected == nil {
			return ErrNotReady
		}
		result.DeploymentID = *selected
		if expected != uuid.Nil() && expected != *selected {
			return ErrInvalidInput
		}
		rows, err := tx.Query(ctx, `SELECT definition_key FROM agent_definitions WHERE environment_id=$1 AND deployment_id=$2 AND definition_key>$3 ORDER BY definition_key LIMIT 51`, caller.Execution.EnvironmentID, *selected, after)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item RuntimeAgentDefinition
			if err := rows.Scan(&item.ID); err != nil {
				return err
			}
			result.Agents = append(result.Agents, item)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(result.Agents) > 50 {
			result.Agents = result.Agents[:50]
			result.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(operation + ":" + selected.String() + ":" + result.Agents[49].ID))
		}
		return nil
	})
	return result, hideMissing(err)
}
