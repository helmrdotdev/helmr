package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestScheduleRepairEntriesAndCursorAdvance(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)
	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	scheduleID := ids.ToPG(ids.New())
	instanceID := ids.ToPG(ids.New())
	scheduledAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)

	created, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:    scheduleID,
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		ScheduleType:  db.TaskScheduleTypeImperative,
		TaskID:        "nightly",
		DedupKey:      "nightly",
		Cron:          "0 2 * * *",
		Timezone:      "UTC",
		RunOptions:    []byte(`{}`),
		Active:        true,
		InstanceID:    instanceID,
		EnvironmentID: scope.EnvironmentID,
		NextFireAt:    pgvalue.Timestamptz(scheduledAt),
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ScheduleID != scheduleID || created.InstanceID != instanceID {
		t.Fatalf("created schedule = %+v", created)
	}

	indexRows, err := queries.ListScheduleRepairEntries(ctx, db.ListScheduleRepairEntriesParams{
		AvailableBefore: pgvalue.Timestamptz(time.Now().UTC().Add(time.Hour)),
		RowLimit:        10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(indexRows) != 1 || indexRows[0].InstanceID != instanceID || indexRows[0].NextFireAt.Time.UTC() != scheduledAt {
		t.Fatalf("index rows = %+v", indexRows)
	}

	candidate, err := queries.GetScheduleTriggerCandidate(ctx, db.GetScheduleTriggerCandidateParams{
		InstanceID:  instanceID,
		Generation:  created.Generation,
		ScheduledAt: pgvalue.Timestamptz(scheduledAt),
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.TaskID != "nightly" {
		t.Fatalf("candidate task = %q", candidate.TaskID)
	}

	next := scheduledAt.Add(24 * time.Hour)
	advanced, err := queries.AdvanceScheduleInstance(ctx, db.AdvanceScheduleInstanceParams{
		InstanceID:       instanceID,
		Generation:       created.Generation,
		LastFireAt:       pgvalue.Timestamptz(scheduledAt),
		NextFireAt:       pgvalue.Timestamptz(next),
		LastTriggerRunID: ids.ToPG(ids.New()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if advanced.InstanceID != instanceID || !advanced.NextFireAt.Valid || !advanced.NextFireAt.Time.Equal(next) {
		t.Fatalf("advanced = %+v", advanced)
	}

	_, err = queries.GetScheduleTriggerCandidate(ctx, db.GetScheduleTriggerCandidateParams{
		InstanceID:  instanceID,
		Generation:  created.Generation,
		ScheduledAt: pgvalue.Timestamptz(scheduledAt),
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
	orgID := ids.ToPG(dbtest.DefaultOrgID)
	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	scheduleID := ids.ToPG(ids.New())
	instanceID := ids.ToPG(ids.New())
	scheduledAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)

	created, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:    scheduleID,
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		ScheduleType:  db.TaskScheduleTypeImperative,
		TaskID:        "retry-me",
		DedupKey:      "retry-me",
		Cron:          "* * * * *",
		Timezone:      "UTC",
		RunOptions:    []byte(`{}`),
		Active:        true,
		InstanceID:    instanceID,
		EnvironmentID: scope.EnvironmentID,
		NextFireAt:    pgvalue.Timestamptz(scheduledAt),
	})
	if err != nil {
		t.Fatal(err)
	}

	retryAt := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	if affected, err := queries.MarkScheduleInstanceTriggerFailed(ctx, db.MarkScheduleInstanceTriggerFailedParams{
		InstanceID:   instanceID,
		Generation:   created.Generation,
		ScheduledAt:  pgvalue.Timestamptz(scheduledAt),
		ErrorKind:    "trigger_failed",
		ErrorMessage: "transient",
		RetryAfter:   pgvalue.Timestamptz(retryAt),
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
		ScheduledAt: pgvalue.Timestamptz(scheduledAt),
	}); err == nil {
		t.Fatal("candidate matched before retry_after")
	} else if err != pgx.ErrNoRows {
		t.Fatal(err)
	}

	if affected, err := queries.MarkScheduleInstanceTriggerFailed(ctx, db.MarkScheduleInstanceTriggerFailedParams{
		InstanceID:   instanceID,
		Generation:   created.Generation,
		ScheduledAt:  pgvalue.Timestamptz(scheduledAt),
		ErrorKind:    "trigger_failed",
		ErrorMessage: "exhausted",
		RetryAfter:   pgvalue.Timestamptz(time.Now().UTC().Add(time.Minute)),
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

	next := scheduledAt.Add(time.Hour)
	if _, err := queries.SkipScheduleInstanceTrigger(ctx, db.SkipScheduleInstanceTriggerParams{
		InstanceID: instanceID,
		Generation: created.Generation,
		LastFireAt: pgvalue.Timestamptz(scheduledAt),
		NextFireAt: pgvalue.Timestamptz(next),
	}); err != nil {
		t.Fatal(err)
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
	if row.TriggerAttemptCount != 0 || row.TriggerErrorMessage != "" || row.RetryAfter.Valid || !row.NextFireAt.Time.Equal(next) || row.LastFireAt.Valid {
		t.Fatalf("skipped failed schedule row = %+v", row)
	}
}

func TestStopScheduleInstanceTriggerClearsCursorAndKeepsError(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)
	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	scheduleID := ids.ToPG(ids.New())
	instanceID := ids.ToPG(ids.New())
	scheduledAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	retryAt := time.Now().UTC().Add(time.Minute).Truncate(time.Second)

	created, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:    scheduleID,
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		ScheduleType:  db.TaskScheduleTypeImperative,
		TaskID:        "stop-me",
		DedupKey:      "stop-me",
		Cron:          "* * * * *",
		Timezone:      "UTC",
		RunOptions:    []byte(`{}`),
		Active:        true,
		InstanceID:    instanceID,
		EnvironmentID: scope.EnvironmentID,
		NextFireAt:    pgvalue.Timestamptz(scheduledAt),
	})
	if err != nil {
		t.Fatal(err)
	}
	if affected, err := queries.MarkScheduleInstanceTriggerFailed(ctx, db.MarkScheduleInstanceTriggerFailedParams{
		InstanceID:   instanceID,
		Generation:   created.Generation,
		ScheduledAt:  pgvalue.Timestamptz(scheduledAt),
		ErrorKind:    "trigger_failed",
		ErrorMessage: "cron has no future occurrences",
		RetryAfter:   pgvalue.Timestamptz(retryAt),
	}); err != nil {
		t.Fatal(err)
	} else if affected != 1 {
		t.Fatalf("mark failed affected = %d", affected)
	}
	if affected, err := queries.StopScheduleInstanceTrigger(ctx, db.StopScheduleInstanceTriggerParams{
		InstanceID:  instanceID,
		Generation:  created.Generation,
		ScheduledAt: pgvalue.Timestamptz(scheduledAt),
	}); err != nil {
		t.Fatal(err)
	} else if affected != 1 {
		t.Fatalf("stop affected = %d", affected)
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
	if row.NextFireAt.Valid || row.RetryAfter.Valid || row.TriggerAttemptCount != 1 || row.TriggerErrorMessage != "cron has no future occurrences" {
		t.Fatalf("stopped schedule row = %+v", row)
	}
}

func TestDeleteScheduleHardDeletesLastInstanceOnly(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)
	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	scheduledAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)

	single, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:    ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		ScheduleType:  db.TaskScheduleTypeImperative,
		TaskID:        "single",
		DedupKey:      "single",
		Cron:          "0 1 * * *",
		Timezone:      "UTC",
		RunOptions:    []byte(`{}`),
		Active:        true,
		InstanceID:    ids.ToPG(ids.New()),
		EnvironmentID: scope.EnvironmentID,
		NextFireAt:    pgvalue.Timestamptz(scheduledAt),
	})
	if err != nil {
		t.Fatal(err)
	}
	affected, err := queries.DeleteSchedule(ctx, db.DeleteScheduleParams{
		ScheduleID:    single.ScheduleID,
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if affected != 1 {
		t.Fatalf("single delete affected = %d", affected)
	}
	var scheduleCount int
	if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM task_schedules WHERE id = $1`, single.ScheduleID).Scan(&scheduleCount); err != nil {
		t.Fatal(err)
	}
	if scheduleCount != 0 {
		t.Fatalf("single schedule row count = %d, want 0", scheduleCount)
	}

	environmentID := ids.ToPG(ids.New())
	if _, err := queries.CreateEnvironment(ctx, db.CreateEnvironmentParams{
		ID:        environmentID,
		OrgID:     orgID,
		ProjectID: scope.ProjectID,
		Slug:      "dev",
		Name:      "Dev",
		ColorHex:  "#22C55E",
	}); err != nil {
		t.Fatal(err)
	}
	first, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:    ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		ScheduleType:  db.TaskScheduleTypeImperative,
		TaskID:        "multi",
		DedupKey:      "multi-internal",
		UserDedupKey:  pgtype.Text{String: "multi", Valid: true},
		Cron:          "0 2 * * *",
		Timezone:      "UTC",
		RunOptions:    []byte(`{}`),
		Active:        true,
		InstanceID:    ids.ToPG(ids.New()),
		EnvironmentID: scope.EnvironmentID,
		NextFireAt:    pgvalue.Timestamptz(scheduledAt),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:    ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		ScheduleType:  db.TaskScheduleTypeImperative,
		TaskID:        "multi",
		DedupKey:      "multi-ignored",
		UserDedupKey:  pgtype.Text{String: "multi", Valid: true},
		Cron:          "0 2 * * *",
		Timezone:      "UTC",
		RunOptions:    []byte(`{}`),
		Active:        true,
		InstanceID:    ids.ToPG(ids.New()),
		EnvironmentID: environmentID,
		NextFireAt:    pgvalue.Timestamptz(scheduledAt),
	}); err != nil {
		t.Fatal(err)
	}
	affected, err = queries.DeleteSchedule(ctx, db.DeleteScheduleParams{
		ScheduleID:    first.ScheduleID,
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if affected != 1 {
		t.Fatalf("multi delete affected = %d", affected)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM task_schedules WHERE id = $1`, first.ScheduleID).Scan(&scheduleCount); err != nil {
		t.Fatal(err)
	}
	if scheduleCount != 1 {
		t.Fatalf("multi schedule row count = %d, want 1", scheduleCount)
	}
	if _, err := queries.GetScheduleSummary(ctx, db.GetScheduleSummaryParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: environmentID,
		ScheduleID:    first.ScheduleID,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = queries.GetScheduleSummary(ctx, db.GetScheduleSummaryParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		ScheduleID:    first.ScheduleID,
	})
	if err != pgx.ErrNoRows {
		t.Fatalf("deleted instance lookup error = %v, want no rows", err)
	}
}

func TestSchedulePublicDedupUpsertsLogicalScheduleAndSeparatesEnvironmentInstances(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)
	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	environmentID := ids.ToPG(ids.New())
	if _, err := queries.CreateEnvironment(ctx, db.CreateEnvironmentParams{
		ID:        environmentID,
		OrgID:     orgID,
		ProjectID: scope.ProjectID,
		Slug:      "preview",
		Name:      "Preview",
		ColorHex:  "#06B6D4",
	}); err != nil {
		t.Fatal(err)
	}

	userDedupKey := pgtype.Text{String: "daily-report", Valid: true}
	firstScheduledAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	first, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:    ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		ScheduleType:  db.TaskScheduleTypeImperative,
		TaskID:        "daily",
		DedupKey:      "internal-default",
		UserDedupKey:  userDedupKey,
		Cron:          "0 9 * * *",
		Timezone:      "UTC",
		RunOptions:    []byte(`{"queue":"default"}`),
		Active:        true,
		InstanceID:    ids.ToPG(ids.New()),
		EnvironmentID: scope.EnvironmentID,
		NextFireAt:    pgvalue.Timestamptz(firstScheduledAt),
	})
	if err != nil {
		t.Fatal(err)
	}

	secondScheduledAt := firstScheduledAt.Add(time.Hour)
	second, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:    ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		ScheduleType:  db.TaskScheduleTypeImperative,
		TaskID:        "daily",
		DedupKey:      "internal-preview",
		UserDedupKey:  userDedupKey,
		Cron:          "0 10 * * *",
		Timezone:      "America/New_York",
		RunOptions:    []byte(`{"queue":"preview"}`),
		Active:        false,
		InstanceID:    ids.ToPG(ids.New()),
		EnvironmentID: environmentID,
		NextFireAt:    pgvalue.Timestamptz(secondScheduledAt),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ScheduleID != first.ScheduleID {
		t.Fatalf("schedule id = %v, want %v", second.ScheduleID, first.ScheduleID)
	}
	if second.InstanceID == first.InstanceID {
		t.Fatalf("instance ids should differ: %v", second.InstanceID)
	}
	if second.DedupKey != first.DedupKey {
		t.Fatalf("internal dedup key changed: %q -> %q", first.DedupKey, second.DedupKey)
	}
	if second.Cron != "0 10 * * *" || second.Timezone != "America/New_York" || second.InstanceActive {
		t.Fatalf("second summary = %+v", second)
	}

	defaultSummary, err := queries.GetScheduleSummary(ctx, db.GetScheduleSummaryParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		ScheduleID:    first.ScheduleID,
	})
	if err != nil {
		t.Fatal(err)
	}
	previewSummary, err := queries.GetScheduleSummary(ctx, db.GetScheduleSummaryParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: environmentID,
		ScheduleID:    first.ScheduleID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if defaultSummary.Cron != "0 10 * * *" || !defaultSummary.NextFireAt.Time.Equal(secondScheduledAt) {
		t.Fatalf("default timing after shared schedule update = %+v", defaultSummary)
	}
	if !defaultSummary.InstanceActive || previewSummary.InstanceActive {
		t.Fatalf("instance active states = default %v preview %v", defaultSummary.InstanceActive, previewSummary.InstanceActive)
	}
}

func TestScheduleDedupKeysAreNamespacedByScheduleType(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)
	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	scheduledAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)

	declarative, err := queries.CreateDeclarativeSchedule(ctx, db.CreateDeclarativeScheduleParams{
		ScheduleID:    ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		TaskID:        "shared-key",
		DedupKey:      "shared-key",
		ExternalID:    pgtype.Text{String: "shared-key", Valid: true},
		Cron:          "0 8 * * *",
		Timezone:      "UTC",
		RunOptions:    []byte(`{}`),
		Active:        true,
		InstanceID:    ids.ToPG(ids.New()),
		EnvironmentID: scope.EnvironmentID,
		NextFireAt:    pgvalue.Timestamptz(scheduledAt),
	})
	if err != nil {
		t.Fatal(err)
	}
	imperative, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:    ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		ScheduleType:  db.TaskScheduleTypeImperative,
		TaskID:        "shared-key",
		DedupKey:      "shared-key",
		UserDedupKey:  pgtype.Text{String: "shared-key", Valid: true},
		Cron:          "0 9 * * *",
		Timezone:      "UTC",
		RunOptions:    []byte(`{}`),
		Active:        true,
		InstanceID:    ids.ToPG(ids.New()),
		EnvironmentID: scope.EnvironmentID,
		NextFireAt:    pgvalue.Timestamptz(scheduledAt.Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if imperative.ScheduleID == declarative.ScheduleID {
		t.Fatalf("imperative schedule reused declarative id %v", imperative.ScheduleID)
	}
	if imperative.ScheduleType != db.TaskScheduleTypeImperative || declarative.ScheduleType != db.TaskScheduleTypeDeclarative {
		t.Fatalf("schedule types = imperative %s declarative %s", imperative.ScheduleType, declarative.ScheduleType)
	}
}

func TestScheduleUpdateOnlyRefreshesSiblingInstancesWhenTimingChanges(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)
	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	staging, err := queries.GetEnvironmentBySlug(ctx, db.GetEnvironmentBySlugParams{
		OrgID:     orgID,
		ProjectID: scope.ProjectID,
		Slug:      "staging",
	})
	if err != nil {
		t.Fatal(err)
	}
	environmentID := staging.ID

	userDedupKey := pgtype.Text{String: "shared-schedule", Valid: true}
	scheduledAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	first, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:    ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		ScheduleType:  db.TaskScheduleTypeImperative,
		TaskID:        "sync",
		DedupKey:      "internal-shared",
		UserDedupKey:  userDedupKey,
		Cron:          "0 9 * * *",
		Timezone:      "UTC",
		RunOptions:    []byte(`{}`),
		Active:        true,
		InstanceID:    ids.ToPG(ids.New()),
		EnvironmentID: scope.EnvironmentID,
		NextFireAt:    pgvalue.Timestamptz(scheduledAt),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:    ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		ScheduleType:  db.TaskScheduleTypeImperative,
		TaskID:        "sync",
		DedupKey:      "ignored-internal",
		UserDedupKey:  userDedupKey,
		Cron:          "0 9 * * *",
		Timezone:      "UTC",
		RunOptions:    []byte(`{}`),
		Active:        true,
		InstanceID:    ids.ToPG(ids.New()),
		EnvironmentID: environmentID,
		NextFireAt:    pgvalue.Timestamptz(scheduledAt),
	})
	if err != nil {
		t.Fatal(err)
	}
	retryAt := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	if affected, err := queries.MarkScheduleInstanceTriggerFailed(ctx, db.MarkScheduleInstanceTriggerFailedParams{
		InstanceID:   second.InstanceID,
		Generation:   second.Generation,
		ScheduledAt:  pgvalue.Timestamptz(scheduledAt),
		ErrorKind:    "trigger_failed",
		ErrorMessage: "keep retry",
		RetryAfter:   pgvalue.Timestamptz(retryAt),
	}); err != nil {
		t.Fatal(err)
	} else if affected != 1 {
		t.Fatalf("mark failed affected = %d", affected)
	}

	if _, err := queries.UpdateSchedule(ctx, db.UpdateScheduleParams{
		TaskID:        "sync",
		ExternalID:    pgtype.Text{},
		Cron:          "0 9 * * *",
		Timezone:      "UTC",
		RunOptions:    []byte(`{"queue":"default"}`),
		Active:        true,
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		ScheduleID:    first.ScheduleID,
		NextFireAt:    pgvalue.Timestamptz(scheduledAt),
	}); err != nil {
		t.Fatal(err)
	}
	sibling, err := queries.GetScheduleSummary(ctx, db.GetScheduleSummaryParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: environmentID,
		ScheduleID:    first.ScheduleID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sibling.Generation != second.Generation || !sibling.RetryAfter.Time.Equal(retryAt) || sibling.TriggerErrorMessage != "keep retry" {
		t.Fatalf("sibling changed without timing change = %+v", sibling)
	}

	nextFireAt := scheduledAt.Add(time.Hour)
	if _, err := queries.UpdateSchedule(ctx, db.UpdateScheduleParams{
		TaskID:        "sync",
		ExternalID:    pgtype.Text{},
		Cron:          "0 10 * * *",
		Timezone:      "UTC",
		RunOptions:    []byte(`{"queue":"default"}`),
		Active:        true,
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		ScheduleID:    first.ScheduleID,
		NextFireAt:    pgvalue.Timestamptz(nextFireAt),
	}); err != nil {
		t.Fatal(err)
	}
	sibling, err = queries.GetScheduleSummary(ctx, db.GetScheduleSummaryParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: environmentID,
		ScheduleID:    first.ScheduleID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sibling.Generation != second.Generation+1 || sibling.RetryAfter.Valid || sibling.TriggerAttemptCount != 0 || sibling.TriggerErrorMessage != "" || !sibling.NextFireAt.Time.Equal(nextFireAt) {
		t.Fatalf("sibling was not refreshed after timing change = %+v", sibling)
	}
	registrationRows, err := queries.ListScheduleInstancesForRegistration(ctx, db.ListScheduleInstancesForRegistrationParams{
		OrgID:      orgID,
		ProjectID:  scope.ProjectID,
		ScheduleID: first.ScheduleID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(registrationRows) != 2 {
		t.Fatalf("registration rows = %d, want 2", len(registrationRows))
	}
	seenSibling := false
	for _, row := range registrationRows {
		if row.InstanceID == sibling.InstanceID {
			seenSibling = true
			if row.Generation != second.Generation+1 || !row.NextFireAt.Time.Equal(nextFireAt) || !row.ScheduleActive || !row.InstanceActive {
				t.Fatalf("sibling registration row = %+v", row)
			}
		}
	}
	if !seenSibling {
		t.Fatalf("registration rows did not include sibling instance: %+v", registrationRows)
	}
}
