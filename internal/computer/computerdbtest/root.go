package computerdbtest

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5/pgconn"
	"testing"
)

// InsertCommittedComputerRoot supplies an adopted initial root for DB fixtures.
// It exercises relational ownership, not encryption or physical initialization.
func InsertCommittedComputerRoot(t *testing.T, ctx context.Context, executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, versionID, environmentID, computerID any) {
	t.Helper()
	dbtest.MustExec(t, ctx, executor, `UPDATE computers SET writer_generation=greatest(writer_generation,1) WHERE environment_id=$1 AND id=$2`, environmentID, computerID)
	dbtest.MustExec(t, ctx, executor, `INSERT INTO computer_disk_versions(id,environment_id,computer_id,status,writer_generation) VALUES($1,$2,$3,'initializing',0)`, versionID, environmentID, computerID)
	InsertComputerVersion(t, ctx, executor, environmentID, computerID, versionID)
	dbtest.MustExec(t, ctx, executor, `UPDATE computer_disk_versions SET status='committed',published_at=clock_timestamp() WHERE id=$1`, versionID)
}
