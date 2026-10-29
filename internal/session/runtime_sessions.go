package session

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// Session discovery retains the original caller relationship across process
// reconstruction. It exposes metadata and receipt identity, not target history.
func RuntimeGetSession(ctx context.Context, policy auditpolicy.Policy, database db.TxBeginner, caller Caller, id uuid.UUID) (SessionView, error) {
	var result SessionView
	err := runtimeSessionRead(ctx, database, caller, func(tx pgx.Tx) error {
		if err := authorizeTarget(ctx, tx, caller, caller.Execution.EnvironmentID, id, auth.PermissionSessionsRead); err != nil {
			return err
		}
		var allowed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE environment_id=$1 AND id=$2 AND (parent_session_id=$3 OR requester_session_id=$3))`, caller.Execution.EnvironmentID, id, caller.ID).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return ErrDenied
		}
		if err := requireMCPTarget(ctx, tx, caller, "inspect_session", id); err != nil {
			return err
		}
		var err error
		result, err = getSession(ctx, tx, caller.Execution.EnvironmentID, id)
		if err != nil {
			return err
		}
		views := []SessionView{result}
		err = redactSessionRelations(ctx, tx, caller, caller.Execution.EnvironmentID, views)
		result = views[0]
		if err != nil {
			return err
		}
		return recordSessionHoldsRead(ctx, policy, tx, caller, caller.Execution.EnvironmentID, views)
	})
	if err != nil {
		return SessionView{}, err
	}
	return result, nil
}

type RuntimeSessionListRequest struct {
	Relation string
	Before   uuid.UUID
	Statuses []string
	Limit    int
}

func RuntimeListSessions(ctx context.Context, policy auditpolicy.Policy, database db.TxBeginner, caller Caller, req RuntimeSessionListRequest) (SessionPage, error) {
	result := SessionPage{Sessions: []SessionView{}}
	if req.Relation != "owned" && req.Relation != "requested" {
		return result, ErrInvalidInput
	}
	err := runtimeSessionRead(ctx, database, caller, func(tx pgx.Tx) error {
		if err := requireMCPOperation(ctx, tx, caller, "inspect_session"); err != nil {
			return err
		}
		query := SessionListRequest{EnvironmentID: caller.Execution.EnvironmentID, Before: req.Before, Statuses: req.Statuses, Limit: req.Limit}
		if req.Relation == "owned" {
			query.ParentSessionID = caller.ID
		} else {
			query.RequesterSessionID = caller.ID
		}
		var err error
		source, err := loadSessionOrigin(ctx, tx, caller.Execution.EnvironmentID, caller.ID)
		if err != nil {
			return err
		}
		filter := sessionReadScope{All: source.mode() == "environment", Agents: []uuid.UUID{}}
		if source.mode() == "shared" && source.Connection != nil {
			filter.SharedConnection = *source.Connection
		}
		if source.mode() == "scoped" {
			filter, err = readScopeFor(ctx, tx, Caller{Kind: source.kind(), ID: source.id(), Human: source.Human}, caller.Execution.EnvironmentID)
			if err != nil {
				return err
			}
			filter.All = false
			filter.ViewAgents = []uuid.UUID{}
		}
		descriptor, err := RuntimeMCPPolicy(ctx, tx, caller)
		if err != nil {
			return err
		}
		filter.MCPAgentKeys = descriptor.Targets("inspect_session")
		result, err = listSessions(ctx, tx, query, filter)
		if err != nil {
			return err
		}
		if err := redactSessionRelations(ctx, tx, caller, caller.Execution.EnvironmentID, result.Sessions); err != nil {
			return err
		}
		return recordSessionHoldsRead(ctx, policy, tx, caller, caller.Execution.EnvironmentID, result.Sessions)
	})
	if err != nil {
		return SessionPage{}, err
	}
	return result, nil
}

func runtimeSessionRead(ctx context.Context, database db.TxBeginner, caller Caller, read func(pgx.Tx) error) error {
	return hideMissing(db.RunTx(ctx, database, func(tx pgx.Tx) error {
		// Read committed rechecks caller authority after any lock wait. Target
		// metadata and effective holds are read in one statement.
		if err := LockRuntimeCaller(ctx, tx, caller); err != nil {
			return err
		}
		return read(tx)
	}))
}
