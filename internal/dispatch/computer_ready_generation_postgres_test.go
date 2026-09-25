package dispatch

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
	"testing"
)

func TestComputerReadyRequiresRetainedGenerationSource(t *testing.T) {
	f := newRunPlacementFixture(t)
	reserved, err := f.authority.PlaceReadyRun(f.ctx, f.candidate())
	if err != nil {
		t.Fatal(err)
	}
	params := runPlacementRuntimeReadyParams(t, f, reserved.RuntimeInstanceID)
	dbtest.MustExec(t, f.ctx, f.pool, `UPDATE runtime_instances SET computer_source_version_id=NULL WHERE id=$1`, reserved.RuntimeInstanceID)
	if _, err := db.New(f.pool).MarkRuntimeInstanceReady(f.ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unretained source ready: %v", err)
	}
	dbtest.MustExec(t, f.ctx, f.pool, `UPDATE runtime_instances SET computer_source_version_id=reserved_workspace_version_id WHERE id=$1`, reserved.RuntimeInstanceID)
	if _, err := db.New(f.pool).MarkRuntimeInstanceReady(f.ctx, params); err != nil {
		t.Fatalf("retained generation ready: %v", err)
	}
}
