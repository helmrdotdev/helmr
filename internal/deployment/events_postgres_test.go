package deployment

import (
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestDeploymentEventsPaginationAndRetention(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	record, err := register(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	// Clear the registration event to make page boundaries explicit.
	dbtest.MustExec(t, t.Context(), f.pool, `DELETE FROM deployment_events WHERE deployment_id=$1`, record.ID)
	for _, kind := range []string{"registered", "preparing", "ready"} {
		if err := appendEvent(t.Context(), f.pool, record.EnvironmentID, record.ID, kind, "message "+kind); err != nil {
			t.Fatal(err)
		}
	}
	first, err := ListEvents(t.Context(), f.pool, f.principal(), f.scope(), record.ID, 0, 2)
	if err != nil || len(first) != 2 {
		t.Fatal(first, err)
	}
	next, err := ListEvents(t.Context(), f.pool, f.principal(), f.scope(), record.ID, first[1].ID, 2)
	if err != nil || len(next) != 1 || next[0].ID <= first[1].ID {
		t.Fatal(next, err)
	}
	for i, event := range append(first, next...) {
		want := []string{"registered", "preparing", "ready"}[i]
		if event.Kind != want || event.Message != "message "+want || event.AcceptedAt.IsZero() {
			t.Fatalf("event: %+v", event)
		}
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE deployment_events SET accepted_at=clock_timestamp()-interval '91 days' WHERE id=$1`, first[0].ID)
	if n, err := sweepEvents(t.Context(), f.pool, 90*24*time.Hour); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	retained, err := ListEvents(t.Context(), f.pool, f.principal(), f.scope(), record.ID, 0, 10)
	if err != nil || len(retained) != 2 || retained[0].ID != first[1].ID {
		t.Fatal(retained, err)
	}
}
