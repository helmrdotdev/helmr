package command

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

// ErrNotFound reports that no Command matches the reference in its scope.
var ErrNotFound = errors.New("command was not found")

// Ref addresses a Command in its organization, project and Environment.
type Ref struct {
	OrgID         uuid.UUID
	ProjectID     uuid.UUID
	EnvironmentID uuid.UUID
	CommandID     uuid.UUID
}

func (r Ref) params() db.GetCommandParams {
	return db.GetCommandParams{
		OrgID: pgvalue.UUID(r.OrgID), ProjectID: pgvalue.UUID(r.ProjectID),
		EnvironmentID: pgvalue.UUID(r.EnvironmentID), CommandID: pgvalue.UUID(r.CommandID),
	}
}

// Get reads the Command in its scope. The read is neutral to result
// retention: a Command whose result payload was pruned is still returned,
// with ResultPrunedAt set, so that scope checks outlive the result.
func Get(ctx context.Context, q db.Querier, ref Ref) (db.ComputerCommand, error) {
	command, err := q.GetCommand(ctx, ref.params())
	if errors.Is(err, pgx.ErrNoRows) {
		return db.ComputerCommand{}, ErrNotFound
	}
	return command, err
}
