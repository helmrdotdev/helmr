package computer

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestComputerResidencyWithoutInitialRoot(t *testing.T) {
	for _, test := range []struct {
		name      string
		deleted   bool
		status    Status
		residency Residency
	}{
		{"unprepared", false, StatusAvailable, "starting"},
		{"deleted", true, StatusDeleted, "cold"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := sessiontest.New(t)
			ctx := t.Context()
			scope := Scope{EnvironmentID: f.Environment}
			if err := f.Pool.QueryRow(ctx, `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&scope.OrgID, &scope.ProjectID); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, ctx, f.Pool, `UPDATE sessions SET status='closed',close_how='close' WHERE id=$1`, f.Session)
			dbtest.MustExec(t, ctx, f.Pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE session_id=$1`, f.Session)
			dbtest.MustExec(t, ctx, f.Pool, `UPDATE computer_leases SET status='lost',fenced_at=clock_timestamp(),fence_evidence='fixture physical stop' WHERE environment_id=$1 AND computer_id=$2`, f.Environment, f.Computer)
			if test.deleted {
				dbtest.MustExec(t, ctx, f.Pool, `UPDATE computers SET storage_reservation_bytes=NULL,deleted_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, f.Environment, f.Computer)
				if err := retireDeletedComputerRoots(ctx, f.Pool); err != nil {
					t.Fatal(err)
				}
			} else {
				dbtest.MustExec(t, ctx, f.Pool, `UPDATE computers SET initial_root_id=NULL,initial_root_digest=NULL,image_spec_id=$3,image_id=NULL,preparation_deadline_at=clock_timestamp()+interval '5 minutes' WHERE environment_id=$1 AND id=$2`, f.Environment, f.Computer, f.Deployment)
			}

			var missingRoot bool
			if err := f.Pool.QueryRow(ctx, `SELECT initial_root_id IS NULL FROM computers WHERE environment_id=$1 AND id=$2`, f.Environment, f.Computer).Scan(&missingRoot); err != nil || !missingRoot {
				t.Fatalf("initial root was not removed: %v", err)
			}

			snapshot, err := Read(ctx, f.Pool, scope, f.Computer)
			if err != nil || snapshot.Status != test.status || snapshot.Residency != test.residency || snapshot.ExecutionWait != nil {
				t.Fatalf("Computer snapshot: %+v, error: %v", snapshot, err)
			}
			listing, err := List(ctx, f.Pool, scope, ListPage{Limit: 100})
			if err != nil || len(listing.Items) != 1 {
				t.Fatalf("Computer list: %+v, error: %v", listing, err)
			}
			item := listing.Items[0]
			if item.Status != test.status || item.Residency != test.residency || item.ExecutionWait != nil {
				t.Fatalf("Computer list item: %+v", item)
			}
		})
	}
}
