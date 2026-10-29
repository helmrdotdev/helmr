package session

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// RuntimeListComputers uses current execution authority to discover the
// caller's workload Computers. Pagination never carries authorization.
func RuntimeListComputers(ctx context.Context, pool db.TxBeginner, caller Caller, page computer.ListPage) (computer.Listing, computer.Scope, error) {
	var result computer.Listing
	var scope computer.Scope
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := LockRuntimeCaller(ctx, tx, caller); err != nil {
			return err
		}
		if err := requireMCPOperation(ctx, tx, caller, "list_computers"); err != nil {
			return err
		}
		scope.EnvironmentID = caller.Execution.EnvironmentID
		if err := tx.QueryRow(ctx, `SELECT org_id,project_id FROM environments WHERE id=$1`, scope.EnvironmentID).Scan(&scope.OrgID, &scope.ProjectID); err != nil {
			return err
		}
		var err error
		result, err = computer.ListForSession(ctx, tx, scope, page, caller.ID)
		return err
	})
	return result, scope, err
}
