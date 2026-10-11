package agent

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// Session discovery retains the original caller relationship across process
// reconstruction. It exposes metadata and receipt identity, not target history.
func RuntimeGetSession(ctx context.Context, database db.TxBeginner, caller Caller, id uuid.UUID) (SessionView, error) {
	var result SessionView
	err := runtimeSessionRead(ctx, database, caller, func(tx pgx.Tx) error {
		var allowed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE environment_id=$1 AND id=$2 AND (parent_session_id=$3 OR requester_session_id=$3))`, caller.Execution.EnvironmentID, id, caller.ID).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return ErrDenied
		}
		var err error
		result, err = getSession(ctx, tx, caller.Execution.EnvironmentID, id)
		return err
	})
	return result, err
}

type RuntimeSessionListRequest struct {
	Relation string
	Before   uuid.UUID
	Statuses []string
	Limit    int
}

func RuntimeListSessions(ctx context.Context, database db.TxBeginner, caller Caller, req RuntimeSessionListRequest) (SessionPage, error) {
	result := SessionPage{Sessions: []SessionView{}}
	if req.Relation != "owned" && req.Relation != "requested" {
		return result, ErrInvalidInput
	}
	err := runtimeSessionRead(ctx, database, caller, func(tx pgx.Tx) error {
		query := SessionListRequest{EnvironmentID: caller.Execution.EnvironmentID, Before: req.Before, Statuses: req.Statuses, Limit: req.Limit}
		if req.Relation == "owned" {
			query.ParentSessionID = caller.ID
		} else {
			query.RequesterSessionID = caller.ID
		}
		var err error
		result, err = listSessions(ctx, tx, query)
		return err
	})
	return result, err
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
