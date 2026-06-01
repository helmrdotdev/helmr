package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestScheduleDueClaimAndFireLease(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	scheduleID := ids.ToPG(ids.New())
	instanceID := ids.ToPG(ids.New())
	dueAt := time.Now().UTC().Add(-time.Minute)

	created, err := queries.CreateImperativeSchedule(ctx, db.CreateImperativeScheduleParams{
		ScheduleID:      scheduleID,
		OrgID:           orgID,
		ProjectID:       scope.ProjectID,
		TaskID:          "nightly",
		DedupKey:        "nightly",
		ExternalID:      pgtype.Text{},
		CronExpression:  "0 2 * * *",
		Timezone:        "UTC",
		Payload:         []byte(`{"kind":"nightly"}`),
		SecretBindings:  []byte(`{}`),
		Workspace:       []byte(`{"repository":"acme/app","ref":"main"}`),
		RunOptions:      []byte(`{}`),
		Active:          true,
		InstanceID:      instanceID,
		EnvironmentID:   scope.EnvironmentID,
		NextScheduledAt: pgTime(dueAt),
		NextDueAt:       pgTime(dueAt),
		CatchUpPolicy:   db.TaskScheduleCatchUpPolicySkipToNext,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ScheduleID != scheduleID || created.InstanceID != instanceID {
		t.Fatalf("created schedule = %+v", created)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	txq := queries.WithTx(tx)
	due, err := txq.ClaimDueScheduleInstances(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].InstanceID != instanceID {
		t.Fatalf("due schedules = %+v", due)
	}
	inserted, err := txq.InsertScheduleFire(ctx, db.InsertScheduleFireParams{
		ScheduleInstanceID: instanceID,
		ScheduledAt:        pgTime(dueAt),
		ScheduleID:         scheduleID,
		OrgID:              orgID,
		ProjectID:          scope.ProjectID,
		EnvironmentID:      scope.EnvironmentID,
		Generation:         due[0].Generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 1 {
		t.Fatalf("inserted fires = %d", inserted)
	}
	if err := txq.AdvanceScheduleInstance(ctx, db.AdvanceScheduleInstanceParams{
		NextScheduledAt: pgTime(time.Now().UTC().Add(time.Hour)),
		NextDueAt:       pgTime(time.Now().UTC().Add(time.Hour)),
		LastScheduledAt: pgTime(dueAt),
		InstanceID:      instanceID,
		Generation:      due[0].Generation,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	leaseID := ids.ToPG(ids.New())
	claimed, err := queries.ClaimDueScheduleFires(ctx, db.ClaimDueScheduleFiresParams{
		RowLimit:       10,
		LeaseID:        leaseID,
		LeaseExpiresAt: pgTime(time.Now().UTC().Add(time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].ScheduleInstanceID != instanceID || claimed[0].LeaseID != leaseID {
		t.Fatalf("claimed fires = %+v", claimed)
	}
	claimedAgain, err := queries.ClaimDueScheduleFires(ctx, db.ClaimDueScheduleFiresParams{
		RowLimit:       10,
		LeaseID:        ids.ToPG(ids.New()),
		LeaseExpiresAt: pgTime(time.Now().UTC().Add(time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimedAgain) != 0 {
		t.Fatalf("claimed leased fire again = %+v", claimedAgain)
	}
}
