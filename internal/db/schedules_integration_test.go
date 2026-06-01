package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
)

func TestScheduleDueClaimAndFireLease(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	scheduleID := ids.ToPG(ids.New())
	instanceID := ids.ToPG(ids.New())
	dueAt := time.Now().UTC().Add(-time.Minute)

	created, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:          scheduleID,
		OrgID:               orgID,
		ProjectID:           scope.ProjectID,
		TaskID:              "nightly",
		DedupKey:            "nightly",
		GeneratorExpression: "0 2 * * *",
		Timezone:            "UTC",
		Payload:             []byte(`{"kind":"nightly"}`),
		SecretBindings:      []byte(`{}`),
		Workspace:           []byte(`{"repository":"acme/app","ref":"main"}`),
		RunOptions:          []byte(`{}`),
		Active:              true,
		InstanceID:          instanceID,
		EnvironmentID:       scope.EnvironmentID,
		NextScheduledAt:     pgTime(dueAt),
		NextDueAt:           pgTime(dueAt),
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
	due, err := txq.ClaimDueScheduleInstances(ctx, db.ClaimDueScheduleInstancesParams{
		RowLimit:       10,
		LeaseID:        ids.ToPG(ids.New()),
		LeaseExpiresAt: pgTime(time.Now().UTC().Add(time.Minute)),
	})
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
		TaskID:             due[0].TaskID,
		Payload:            due[0].Payload,
		SecretBindings:     due[0].SecretBindings,
		Workspace:          due[0].Workspace,
		RunOptions:         due[0].RunOptions,
		MaterializeLeaseID: due[0].MaterializeLeaseID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 1 {
		t.Fatalf("inserted fires = %d", inserted)
	}
	advanced, err := txq.AdvanceScheduleInstance(ctx, db.AdvanceScheduleInstanceParams{
		NextScheduledAt:    pgTime(time.Now().UTC().Add(time.Hour)),
		NextDueAt:          pgTime(time.Now().UTC().Add(time.Hour)),
		LastScheduledAt:    pgTime(dueAt),
		InstanceID:         instanceID,
		Generation:         due[0].Generation,
		MaterializeLeaseID: due[0].MaterializeLeaseID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if advanced != 1 {
		t.Fatalf("advanced instances = %d", advanced)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	leaseID := ids.ToPG(ids.New())
	claimed, err := queries.ClaimDueScheduleFires(ctx, db.ClaimDueScheduleFiresParams{
		RowLimit:       10,
		LeaseID:        leaseID,
		LeaseExpiresAt: pgTime(time.Now().UTC().Add(time.Minute)),
		MaxAttempts:    10,
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
		MaxAttempts:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimedAgain) != 0 {
		t.Fatalf("claimed leased fire again = %+v", claimedAgain)
	}
}

func TestClaimDueScheduleInstancesLeasesInstance(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	scheduleID := ids.ToPG(ids.New())
	instanceID := ids.ToPG(ids.New())
	dueAt := time.Now().UTC().Add(-time.Minute)
	claimUntil := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)

	if _, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:          scheduleID,
		OrgID:               orgID,
		ProjectID:           scope.ProjectID,
		TaskID:              "lease-instance",
		DedupKey:            "lease-instance",
		GeneratorExpression: "0 2 * * *",
		Timezone:            "UTC",
		Payload:             []byte(`{}`),
		SecretBindings:      []byte(`{}`),
		Workspace:           []byte(`{"repository":"acme/app","ref":"main"}`),
		RunOptions:          []byte(`{}`),
		Active:              true,
		InstanceID:          instanceID,
		EnvironmentID:       scope.EnvironmentID,
		NextScheduledAt:     pgTime(dueAt),
		NextDueAt:           pgTime(dueAt),
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := queries.ClaimDueScheduleInstances(ctx, db.ClaimDueScheduleInstancesParams{
		RowLimit:       10,
		LeaseID:        ids.ToPG(ids.New()),
		LeaseExpiresAt: pgTime(claimUntil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed instances = %+v", claimed)
	}
	claimedAgain, err := queries.ClaimDueScheduleInstances(ctx, db.ClaimDueScheduleInstancesParams{
		RowLimit:       10,
		LeaseID:        ids.ToPG(ids.New()),
		LeaseExpiresAt: pgTime(time.Now().UTC().Add(time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimedAgain) != 0 {
		t.Fatalf("claimed leased instance again = %+v", claimedAgain)
	}
	var leaseExpiresAt time.Time
	if err := pool.QueryRow(ctx, `
SELECT materialize_lease_expires_at
  FROM task_schedule_instances
 WHERE id = $1
`, instanceID).Scan(&leaseExpiresAt); err != nil {
		t.Fatal(err)
	}
	if !leaseExpiresAt.Equal(claimUntil) {
		t.Fatalf("materialize_lease_expires_at = %s, want %s", leaseExpiresAt, claimUntil)
	}
}

func TestMarkScheduleInstanceMaterializationFailedRequiresCurrentScheduledTime(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	scheduleID := ids.ToPG(ids.New())
	instanceID := ids.ToPG(ids.New())
	originalScheduledAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	advancedScheduledAt := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	advancedDueAt := advancedScheduledAt.Add(30 * time.Second)

	if _, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:          scheduleID,
		OrgID:               orgID,
		ProjectID:           scope.ProjectID,
		TaskID:              "delay-race",
		DedupKey:            "delay-race",
		GeneratorExpression: "0 2 * * *",
		Timezone:            "UTC",
		Payload:             []byte(`{}`),
		SecretBindings:      []byte(`{}`),
		Workspace:           []byte(`{"repository":"acme/app","ref":"main"}`),
		RunOptions:          []byte(`{}`),
		Active:              true,
		InstanceID:          instanceID,
		EnvironmentID:       scope.EnvironmentID,
		NextScheduledAt:     pgTime(originalScheduledAt),
		NextDueAt:           pgTime(originalScheduledAt),
	}); err != nil {
		t.Fatal(err)
	}
	oldLeaseID := ids.ToPG(ids.New())
	currentLeaseID := ids.ToPG(ids.New())
	if _, err := pool.Exec(ctx, `
UPDATE task_schedule_instances
   SET next_scheduled_at = $1,
       next_due_at = $2,
       last_scheduled_at = $3,
       materialize_lease_id = $4,
       materialize_lease_expires_at = now() + interval '1 minute'
 WHERE id = $5
`, pgTime(advancedScheduledAt), pgTime(advancedDueAt), pgTime(originalScheduledAt), currentLeaseID, instanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkScheduleInstanceMaterializationFailed(ctx, db.MarkScheduleInstanceMaterializationFailedParams{
		ErrorMessage:       "snapshot failed",
		MaxAttempts:        10,
		NextDueAt:          pgTime(originalScheduledAt.Add(time.Minute)),
		InstanceID:         instanceID,
		Generation:         1,
		MaterializeLeaseID: oldLeaseID,
		NextScheduledAt:    pgTime(originalScheduledAt),
	}); err != nil {
		t.Fatal(err)
	}
	var nextDueAt time.Time
	if err := pool.QueryRow(ctx, `
SELECT next_due_at
  FROM task_schedule_instances
 WHERE id = $1
`, instanceID).Scan(&nextDueAt); err != nil {
		t.Fatal(err)
	}
	if !nextDueAt.Equal(advancedDueAt) {
		t.Fatalf("stale delay next_due_at = %s, want %s", nextDueAt, advancedDueAt)
	}
	delayedDueAt := advancedScheduledAt.Add(time.Minute)
	if _, err := queries.MarkScheduleInstanceMaterializationFailed(ctx, db.MarkScheduleInstanceMaterializationFailedParams{
		ErrorMessage:       "snapshot failed",
		MaxAttempts:        10,
		NextDueAt:          pgTime(delayedDueAt),
		InstanceID:         instanceID,
		Generation:         1,
		MaterializeLeaseID: currentLeaseID,
		NextScheduledAt:    pgTime(advancedScheduledAt),
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
SELECT next_due_at
  FROM task_schedule_instances
 WHERE id = $1
`, instanceID).Scan(&nextDueAt); err != nil {
		t.Fatal(err)
	}
	if !nextDueAt.Equal(delayedDueAt) {
		t.Fatalf("current delay next_due_at = %s, want %s", nextDueAt, delayedDueAt)
	}
}

func TestCreateScheduleAttachesExistingDefinitionToEnvironment(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	environmentID := ids.ToPG(ids.New())
	if _, err := queries.CreateEnvironment(ctx, db.CreateEnvironmentParams{
		ID:        environmentID,
		OrgID:     orgID,
		ProjectID: scope.ProjectID,
		Slug:      "qa",
		Name:      "QA",
	}); err != nil {
		t.Fatal(err)
	}
	first, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:          ids.ToPG(ids.New()),
		OrgID:               orgID,
		ProjectID:           scope.ProjectID,
		TaskID:              "nightly",
		DedupKey:            "nightly",
		GeneratorExpression: "0 2 * * *",
		Timezone:            "UTC",
		Payload:             []byte(`{}`),
		SecretBindings:      []byte(`{}`),
		Workspace:           []byte(`{"repository":"acme/app","ref":"main"}`),
		RunOptions:          []byte(`{}`),
		Active:              true,
		InstanceID:          ids.ToPG(ids.New()),
		EnvironmentID:       scope.EnvironmentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondInstanceID := ids.ToPG(ids.New())
	second, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:          ids.ToPG(ids.New()),
		OrgID:               orgID,
		ProjectID:           scope.ProjectID,
		TaskID:              "nightly",
		DedupKey:            "nightly",
		GeneratorExpression: "0 2 * * *",
		Timezone:            "UTC",
		Payload:             []byte(`{}`),
		SecretBindings:      []byte(`{}`),
		Workspace:           []byte(`{"repository":"acme/app","ref":"main"}`),
		RunOptions:          []byte(`{}`),
		Active:              true,
		InstanceID:          secondInstanceID,
		EnvironmentID:       environmentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ScheduleID != first.ScheduleID {
		t.Fatalf("second schedule id = %v, want %v", second.ScheduleID, first.ScheduleID)
	}
	if second.InstanceID != secondInstanceID {
		t.Fatalf("second instance id = %v, want %v", second.InstanceID, secondInstanceID)
	}
}

func TestScheduleFireClaimRequiresCurrentGeneration(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	scheduleID := ids.ToPG(ids.New())
	instanceID := ids.ToPG(ids.New())
	dueAt := time.Now().UTC().Add(-time.Minute)

	if _, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:          scheduleID,
		OrgID:               orgID,
		ProjectID:           scope.ProjectID,
		TaskID:              "stale-fire",
		DedupKey:            "stale-fire",
		GeneratorExpression: "0 2 * * *",
		Timezone:            "UTC",
		Payload:             []byte(`{"version":1}`),
		SecretBindings:      []byte(`{}`),
		Workspace:           []byte(`{"repository":"acme/app","ref":"main"}`),
		RunOptions:          []byte(`{}`),
		Active:              true,
		InstanceID:          instanceID,
		EnvironmentID:       scope.EnvironmentID,
		NextScheduledAt:     pgTime(dueAt),
		NextDueAt:           pgTime(dueAt),
	}); err != nil {
		t.Fatal(err)
	}
	due, err := queries.ClaimDueScheduleInstances(ctx, db.ClaimDueScheduleInstancesParams{
		RowLimit:       10,
		LeaseID:        ids.ToPG(ids.New()),
		LeaseExpiresAt: pgTime(time.Now().UTC().Add(time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("due schedules = %+v", due)
	}
	if _, err := queries.InsertScheduleFire(ctx, db.InsertScheduleFireParams{
		ScheduleInstanceID: instanceID,
		ScheduledAt:        pgTime(dueAt),
		ScheduleID:         scheduleID,
		OrgID:              orgID,
		ProjectID:          scope.ProjectID,
		EnvironmentID:      scope.EnvironmentID,
		Generation:         due[0].Generation,
		TaskID:             due[0].TaskID,
		Payload:            due[0].Payload,
		SecretBindings:     due[0].SecretBindings,
		Workspace:          due[0].Workspace,
		RunOptions:         due[0].RunOptions,
		MaterializeLeaseID: due[0].MaterializeLeaseID,
	}); err != nil {
		t.Fatal(err)
	}
	leaseID := ids.ToPG(ids.New())
	leased, err := queries.ClaimDueScheduleFires(ctx, db.ClaimDueScheduleFiresParams{
		RowLimit:       10,
		LeaseID:        leaseID,
		LeaseExpiresAt: pgTime(time.Now().UTC().Add(time.Minute)),
		MaxAttempts:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leased) != 1 {
		t.Fatalf("leased fire = %+v", leased)
	}
	if _, err := queries.UpdateScheduleState(ctx, db.UpdateScheduleStateParams{
		Active:        false,
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		ScheduleID:    scheduleID,
		EnvironmentID: scope.EnvironmentID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpdateScheduleState(ctx, db.UpdateScheduleStateParams{
		Active:          true,
		OrgID:           orgID,
		ProjectID:       scope.ProjectID,
		ScheduleID:      scheduleID,
		EnvironmentID:   scope.EnvironmentID,
		NextScheduledAt: pgTime(time.Now().UTC().Add(time.Hour)),
		JitterSeconds:   1,
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := queries.ClaimDueScheduleFires(ctx, db.ClaimDueScheduleFiresParams{
		RowLimit:       10,
		LeaseID:        ids.ToPG(ids.New()),
		LeaseExpiresAt: pgTime(time.Now().UTC().Add(time.Minute)),
		MaxAttempts:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed stale generation fire = %+v", claimed)
	}
	if _, err := queries.SupersedeScheduleInstanceFires(ctx, db.SupersedeScheduleInstanceFiresParams{
		ScheduleInstanceID: instanceID,
		Generation:         3,
	}); err != nil {
		t.Fatal(err)
	}
	var status db.TaskScheduleFireStatus
	if err := pool.QueryRow(ctx, `
SELECT status
  FROM task_schedule_fires
 WHERE schedule_instance_id = $1
   AND scheduled_at = $2
`, instanceID, pgTime(dueAt)).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != db.TaskScheduleFireStatusSuperseded {
		t.Fatalf("leased stale fire status = %s", status)
	}
	inserted, err := queries.InsertScheduleFire(ctx, db.InsertScheduleFireParams{
		ScheduleInstanceID: instanceID,
		ScheduledAt:        pgTime(dueAt.Add(time.Minute)),
		ScheduleID:         scheduleID,
		OrgID:              orgID,
		ProjectID:          scope.ProjectID,
		EnvironmentID:      scope.EnvironmentID,
		Generation:         1,
		TaskID:             "stale-fire",
		Payload:            []byte(`{}`),
		SecretBindings:     []byte(`{}`),
		Workspace:          []byte(`{"repository":"acme/app","ref":"main"}`),
		RunOptions:         []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 0 {
		t.Fatalf("inserted stale generation fire = %d", inserted)
	}
}

func TestScheduleFireClaimStopsAtMaxAttempts(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	scheduleID := ids.ToPG(ids.New())
	instanceID := ids.ToPG(ids.New())
	dueAt := time.Now().UTC().Add(-time.Minute)

	if _, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:          scheduleID,
		OrgID:               orgID,
		ProjectID:           scope.ProjectID,
		TaskID:              "poison-fire",
		DedupKey:            "poison-fire",
		GeneratorExpression: "0 2 * * *",
		Timezone:            "UTC",
		Payload:             []byte(`{}`),
		SecretBindings:      []byte(`{}`),
		Workspace:           []byte(`{"repository":"acme/app","ref":"main"}`),
		RunOptions:          []byte(`{}`),
		Active:              true,
		InstanceID:          instanceID,
		EnvironmentID:       scope.EnvironmentID,
		NextScheduledAt:     pgTime(dueAt),
		NextDueAt:           pgTime(dueAt),
	}); err != nil {
		t.Fatal(err)
	}
	due, err := queries.ClaimDueScheduleInstances(ctx, db.ClaimDueScheduleInstancesParams{
		RowLimit:       10,
		LeaseID:        ids.ToPG(ids.New()),
		LeaseExpiresAt: pgTime(time.Now().UTC().Add(time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("due schedules = %+v", due)
	}
	if _, err := queries.InsertScheduleFire(ctx, db.InsertScheduleFireParams{
		ScheduleInstanceID: instanceID,
		ScheduledAt:        pgTime(dueAt),
		ScheduleID:         scheduleID,
		OrgID:              orgID,
		ProjectID:          scope.ProjectID,
		EnvironmentID:      scope.EnvironmentID,
		Generation:         due[0].Generation,
		TaskID:             due[0].TaskID,
		Payload:            due[0].Payload,
		SecretBindings:     due[0].SecretBindings,
		Workspace:          due[0].Workspace,
		RunOptions:         due[0].RunOptions,
		MaterializeLeaseID: due[0].MaterializeLeaseID,
	}); err != nil {
		t.Fatal(err)
	}
	leaseID := ids.ToPG(ids.New())
	claimed, err := queries.ClaimDueScheduleFires(ctx, db.ClaimDueScheduleFiresParams{
		RowLimit:       10,
		LeaseID:        leaseID,
		LeaseExpiresAt: pgTime(time.Now().UTC().Add(time.Minute)),
		MaxAttempts:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].AttemptCount != 1 {
		t.Fatalf("claimed fire = %+v", claimed)
	}
	if _, err := pool.Exec(ctx, `
UPDATE task_schedule_fires
   SET lease_expires_at = now() - interval '1 second'
 WHERE schedule_instance_id = $1
   AND scheduled_at = $2
`, instanceID, pgTime(dueAt)); err != nil {
		t.Fatal(err)
	}
	leaseID = ids.ToPG(ids.New())
	claimed, err = queries.ClaimDueScheduleFires(ctx, db.ClaimDueScheduleFiresParams{
		RowLimit:       10,
		LeaseID:        leaseID,
		LeaseExpiresAt: pgTime(time.Now().UTC().Add(time.Minute)),
		MaxAttempts:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("reclaimed expired final lease = %+v", claimed)
	}
	var status db.TaskScheduleFireStatus
	if err := pool.QueryRow(ctx, `
SELECT status
  FROM task_schedule_fires
 WHERE schedule_instance_id = $1
   AND scheduled_at = $2
`, instanceID, pgTime(dueAt)).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != db.TaskScheduleFireStatusFailed {
		t.Fatalf("expired final lease status = %s", status)
	}
	claimedAgain, err := queries.ClaimDueScheduleFires(ctx, db.ClaimDueScheduleFiresParams{
		RowLimit:       10,
		LeaseID:        ids.ToPG(ids.New()),
		LeaseExpiresAt: pgTime(time.Now().UTC().Add(time.Minute)),
		MaxAttempts:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimedAgain) != 0 {
		t.Fatalf("claimed fire after max attempts = %+v", claimedAgain)
	}
}

func TestScheduleFireFailedPathMarksAttemptsExhausted(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	scheduleID := ids.ToPG(ids.New())
	instanceID := ids.ToPG(ids.New())
	dueAt := time.Now().UTC().Add(-time.Minute)

	if _, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:          scheduleID,
		OrgID:               orgID,
		ProjectID:           scope.ProjectID,
		TaskID:              "failed-fire",
		DedupKey:            "failed-fire",
		GeneratorExpression: "0 2 * * *",
		Timezone:            "UTC",
		Payload:             []byte(`{}`),
		SecretBindings:      []byte(`{}`),
		Workspace:           []byte(`{"repository":"acme/app","ref":"main"}`),
		RunOptions:          []byte(`{}`),
		Active:              true,
		InstanceID:          instanceID,
		EnvironmentID:       scope.EnvironmentID,
		NextScheduledAt:     pgTime(dueAt),
		NextDueAt:           pgTime(dueAt),
	}); err != nil {
		t.Fatal(err)
	}
	due, err := queries.ClaimDueScheduleInstances(ctx, db.ClaimDueScheduleInstancesParams{
		RowLimit:       10,
		LeaseID:        ids.ToPG(ids.New()),
		LeaseExpiresAt: pgTime(time.Now().UTC().Add(time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("due schedules = %+v", due)
	}
	if _, err := queries.InsertScheduleFire(ctx, db.InsertScheduleFireParams{
		ScheduleInstanceID: instanceID,
		ScheduledAt:        pgTime(dueAt),
		ScheduleID:         scheduleID,
		OrgID:              orgID,
		ProjectID:          scope.ProjectID,
		EnvironmentID:      scope.EnvironmentID,
		Generation:         due[0].Generation,
		TaskID:             due[0].TaskID,
		Payload:            due[0].Payload,
		SecretBindings:     due[0].SecretBindings,
		Workspace:          due[0].Workspace,
		RunOptions:         due[0].RunOptions,
		MaterializeLeaseID: due[0].MaterializeLeaseID,
	}); err != nil {
		t.Fatal(err)
	}
	leaseID := ids.ToPG(ids.New())
	claimed, err := queries.ClaimDueScheduleFires(ctx, db.ClaimDueScheduleFiresParams{
		RowLimit:       10,
		LeaseID:        leaseID,
		LeaseExpiresAt: pgTime(time.Now().UTC().Add(time.Minute)),
		MaxAttempts:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed fire = %+v", claimed)
	}
	if _, err := queries.MarkScheduleFireFailed(ctx, db.MarkScheduleFireFailedParams{
		MaxAttempts:        1,
		ErrorMessage:       "temporary failure",
		NextAttemptAt:      pgTime(time.Now().UTC().Add(-time.Second)),
		ScheduleInstanceID: instanceID,
		ScheduledAt:        pgTime(dueAt),
		LeaseID:            leaseID,
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err = queries.ClaimDueScheduleFires(ctx, db.ClaimDueScheduleFiresParams{
		RowLimit:       10,
		LeaseID:        ids.ToPG(ids.New()),
		LeaseExpiresAt: pgTime(time.Now().UTC().Add(time.Minute)),
		MaxAttempts:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed exhausted failed fire = %+v", claimed)
	}
	var message string
	var updatedAt time.Time
	if err := pool.QueryRow(ctx, `
SELECT error_message, updated_at
  FROM task_schedule_fires
 WHERE schedule_instance_id = $1
   AND scheduled_at = $2
`, instanceID, pgTime(dueAt)).Scan(&message, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if message != "schedule fire attempts exhausted: temporary failure" {
		t.Fatalf("error_message = %q", message)
	}
	time.Sleep(10 * time.Millisecond)
	claimed, err = queries.ClaimDueScheduleFires(ctx, db.ClaimDueScheduleFiresParams{
		RowLimit:       10,
		LeaseID:        ids.ToPG(ids.New()),
		LeaseExpiresAt: pgTime(time.Now().UTC().Add(time.Minute)),
		MaxAttempts:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed exhausted failed fire after annotation = %+v", claimed)
	}
	var updatedAgain time.Time
	if err := pool.QueryRow(ctx, `
SELECT updated_at
  FROM task_schedule_fires
 WHERE schedule_instance_id = $1
   AND scheduled_at = $2
`, instanceID, pgTime(dueAt)).Scan(&updatedAgain); err != nil {
		t.Fatal(err)
	}
	if !updatedAgain.Equal(updatedAt) {
		t.Fatalf("updated_at changed on second terminal sweep: %s -> %s", updatedAt, updatedAgain)
	}
}
