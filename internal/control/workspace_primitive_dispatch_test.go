package control

import (
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkspaceMountFromEnsureRowCopiesUnmountedAt(t *testing.T) {
	unmountedAt := pgtype.Timestamptz{Time: time.Unix(123, 0).UTC(), Valid: true}
	mount := workspaceMountFromEnsureRow(db.EnsureWorkspaceMountRequestedRow{
		UnmountedAt: unmountedAt,
	})
	if mount.UnmountedAt != unmountedAt {
		t.Fatalf("unmounted_at = %+v, want %+v", mount.UnmountedAt, unmountedAt)
	}
}
