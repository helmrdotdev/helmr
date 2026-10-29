package command

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"time"
	"uuid"
)

// RelatedCommand is a retained direct request, not authority inherited from its requester.
type RelatedCommand struct {
	ID         uuid.UUID `json:"id"`
	ComputerID uuid.UUID `json:"computer_id"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}
type RelatedCommands struct {
	Commands   []RelatedCommand `json:"commands"`
	NextCursor *uuid.UUID       `json:"next_cursor,omitempty"`
	CanCancel  bool             `json:"can_cancel"`
}

func ListRelated(ctx context.Context, pool db.TxBeginner, principal auth.Principal, scope auth.Scope, user, before uuid.UUID) (RelatedCommands, error) {
	result := RelatedCommands{Commands: []RelatedCommand{}}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		current, err := access.Current(ctx, tx, principal, scope)
		if err != nil {
			return err
		}
		if err = auth.Require(current, auth.PermissionComputersRead, scope, access.ErrDenied); err != nil {
			return err
		}
		result.CanCancel = current.HasPermission(auth.PermissionComputersCancelCommand, scope)
		rows, err := tx.Query(ctx, `SELECT id,computer_id,status,created_at FROM commands WHERE environment_id=$1 AND caller_kind='user' AND caller_id=$2 AND (terminal_at IS NULL OR (computer_lease_epoch IS NOT NULL AND process_reconciled_at IS NULL)) AND ($3::uuid='00000000-0000-0000-0000-000000000000' OR id<$3) ORDER BY id DESC LIMIT 101`, scope.EnvironmentID, user, before)
		if err != nil {
			return err
		}
		result.Commands, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (RelatedCommand, error) {
			var v RelatedCommand
			err := row.Scan(&v.ID, &v.ComputerID, &v.Status, &v.CreatedAt)
			return v, err
		})
		if result.Commands == nil {
			result.Commands = []RelatedCommand{}
		}
		if len(result.Commands) > 100 {
			result.Commands = result.Commands[:100]
			id := result.Commands[99].ID
			result.NextCursor = &id
		}
		return err
	})
	return result, err
}
