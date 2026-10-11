package computer

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// ListRuntime uses current execution authority to discover the caller's
// Environment Computers. Pagination never carries authorization.
func ListRuntime(ctx context.Context, pool db.TxBeginner, caller agent.Caller, page ListPage) (Listing, Scope, error) {
	var result Listing
	var scope Scope
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := agent.LockRuntimeCaller(ctx, tx, caller); err != nil {
			return err
		}
		scope.EnvironmentID = caller.Execution.EnvironmentID
		if err := tx.QueryRow(ctx, `SELECT org_id,project_id FROM environments WHERE id=$1`, scope.EnvironmentID).Scan(&scope.OrgID, &scope.ProjectID); err != nil {
			return err
		}
		var err error
		result, err = List(ctx, tx, scope, page)
		return err
	})
	return result, scope, err
}
