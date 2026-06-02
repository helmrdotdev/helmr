package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
)

func TestScheduleIndexEntriesAndCursorAdvance(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	scheduleID := ids.ToPG(ids.New())
	instanceID := ids.ToPG(ids.New())
	scheduledAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)

	created, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:      scheduleID,
		OrgID:           orgID,
		ProjectID:       scope.ProjectID,
		ScheduleType:    db.TaskScheduleTypeImperative,
		TaskID:          "nightly",
		DedupKey:        "nightly",
		Cron:            "0 2 * * *",
		Timezone:        "UTC",
		SecretBindings:  []byte(`{}`),
		Workspace:       []byte(`{"repository":"acme/app","ref":"main"}`),
		RunOptions:      []byte(`{}`),
		Active:          true,
		InstanceID:      instanceID,
		EnvironmentID:   scope.EnvironmentID,
		NextScheduledAt: pgTime(scheduledAt),
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ScheduleID != scheduleID || created.InstanceID != instanceID {
		t.Fatalf("created schedule = %+v", created)
	}

	indexRows, err := queries.ListScheduleIndexEntries(ctx, db.ListScheduleIndexEntriesParams{
		AvailableBefore: pgTime(time.Now().UTC().Add(time.Hour)),
		RowLimit:        10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(indexRows) != 1 || indexRows[0].InstanceID != instanceID || indexRows[0].NextScheduledAt.Time.UTC() != scheduledAt {
		t.Fatalf("index rows = %+v", indexRows)
	}

	candidate, err := queries.GetScheduleTriggerCandidate(ctx, db.GetScheduleTriggerCandidateParams{
		InstanceID:  instanceID,
		Generation:  created.Generation,
		ScheduledAt: pgTime(scheduledAt),
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.TaskID != "nightly" {
		t.Fatalf("candidate task = %q", candidate.TaskID)
	}

	next := scheduledAt.Add(24 * time.Hour)
	advanced, err := queries.AdvanceScheduleInstance(ctx, db.AdvanceScheduleInstanceParams{
		InstanceID:      instanceID,
		Generation:      created.Generation,
		LastScheduledAt: pgTime(scheduledAt),
		NextScheduledAt: pgTime(next),
	})
	if err != nil {
		t.Fatal(err)
	}
	if advanced.InstanceID != instanceID || !advanced.NextScheduledAt.Valid || !advanced.NextScheduledAt.Time.Equal(next) {
		t.Fatalf("advanced = %+v", advanced)
	}

	_, err = queries.GetScheduleTriggerCandidate(ctx, db.GetScheduleTriggerCandidateParams{
		InstanceID:  instanceID,
		Generation:  created.Generation,
		ScheduledAt: pgTime(scheduledAt),
	})
	if err == nil {
		t.Fatal("stale scheduled_at candidate still matched")
	}
	if err != pgx.ErrNoRows {
		t.Fatal(err)
	}
}

func TestScheduleTriggerFailurePersistsRetryAndExhausts(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	scheduleID := ids.ToPG(ids.New())
	instanceID := ids.ToPG(ids.New())
	scheduledAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)

	created, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:      scheduleID,
		OrgID:           orgID,
		ProjectID:       scope.ProjectID,
		ScheduleType:    db.TaskScheduleTypeImperative,
		TaskID:          "retry-me",
		DedupKey:        "retry-me",
		Cron:            "* * * * *",
		Timezone:        "UTC",
		SecretBindings:  []byte(`{}`),
		Workspace:       []byte(`{"repository":"acme/app","ref":"main"}`),
		RunOptions:      []byte(`{}`),
		Active:          true,
		InstanceID:      instanceID,
		EnvironmentID:   scope.EnvironmentID,
		NextScheduledAt: pgTime(scheduledAt),
	})
	if err != nil {
		t.Fatal(err)
	}

	retryAt := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	if affected, err := queries.MarkScheduleInstanceTriggerFailed(ctx, db.MarkScheduleInstanceTriggerFailedParams{
		InstanceID:   instanceID,
		Generation:   created.Generation,
		ScheduledAt:  pgTime(scheduledAt),
		ErrorMessage: "transient",
		RetryAfter:   pgTime(retryAt),
	}); err != nil {
		t.Fatal(err)
	} else if affected != 1 {
		t.Fatalf("failed affected = %d", affected)
	}

	row, err := queries.GetScheduleSummary(ctx, db.GetScheduleSummaryParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		ScheduleID:    scheduleID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !row.InstanceActive || row.TriggerAttemptCount != 1 || row.TriggerErrorMessage != "transient" || !row.RetryAfter.Time.Equal(retryAt) {
		t.Fatalf("failed schedule row = %+v", row)
	}
	if _, err := queries.GetScheduleTriggerCandidate(ctx, db.GetScheduleTriggerCandidateParams{
		InstanceID:  instanceID,
		Generation:  created.Generation,
		ScheduledAt: pgTime(scheduledAt),
	}); err == nil {
		t.Fatal("candidate matched before retry_after")
	} else if err != pgx.ErrNoRows {
		t.Fatal(err)
	}

	if affected, err := queries.MarkScheduleInstanceTriggerFailed(ctx, db.MarkScheduleInstanceTriggerFailedParams{
		InstanceID:   instanceID,
		Generation:   created.Generation,
		ScheduledAt:  pgTime(scheduledAt),
		ErrorMessage: "exhausted",
		RetryAfter:   pgTime(time.Now().UTC().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	} else if affected != 1 {
		t.Fatalf("exhaust affected = %d", affected)
	}

	row, err = queries.GetScheduleSummary(ctx, db.GetScheduleSummaryParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		ScheduleID:    scheduleID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !row.InstanceActive || !row.RetryAfter.Valid || row.TriggerAttemptCount != 2 || row.TriggerErrorMessage != "exhausted" {
		t.Fatalf("second failed schedule row = %+v", row)
	}
}
