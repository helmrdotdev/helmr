package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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

	next := scheduledAt.Add(time.Hour)
	if _, err := queries.SkipScheduleInstanceTrigger(ctx, db.SkipScheduleInstanceTriggerParams{
		InstanceID:      instanceID,
		Generation:      created.Generation,
		LastScheduledAt: pgTime(scheduledAt),
		NextScheduledAt: pgTime(next),
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
	if row.TriggerAttemptCount != 0 || row.TriggerErrorMessage != "" || row.RetryAfter.Valid || !row.NextScheduledAt.Time.Equal(next) {
		t.Fatalf("skipped failed schedule row = %+v", row)
	}
}

func TestSchedulePublicDedupUpsertsLogicalScheduleAndSeparatesEnvironmentInstances(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	environmentID := ids.ToPG(ids.New())
	if _, err := queries.CreateEnvironment(ctx, db.CreateEnvironmentParams{
		ID:        environmentID,
		OrgID:     orgID,
		ProjectID: scope.ProjectID,
		Slug:      "preview",
		Name:      "Preview",
	}); err != nil {
		t.Fatal(err)
	}

	userDedupKey := pgtype.Text{String: "daily-report", Valid: true}
	firstScheduledAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	first, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:      ids.ToPG(ids.New()),
		OrgID:           orgID,
		ProjectID:       scope.ProjectID,
		ScheduleType:    db.TaskScheduleTypeImperative,
		TaskID:          "daily",
		DedupKey:        "internal-default",
		UserDedupKey:    userDedupKey,
		Cron:            "0 9 * * *",
		Timezone:        "UTC",
		SecretBindings:  []byte(`{"TOKEN":"vault:default-token"}`),
		Workspace:       []byte(`{"repository":"acme/app","ref":"main"}`),
		RunOptions:      []byte(`{"queue":"default"}`),
		Active:          true,
		InstanceID:      ids.ToPG(ids.New()),
		EnvironmentID:   scope.EnvironmentID,
		NextScheduledAt: pgTime(firstScheduledAt),
	})
	if err != nil {
		t.Fatal(err)
	}

	secondScheduledAt := firstScheduledAt.Add(time.Hour)
	second, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:      ids.ToPG(ids.New()),
		OrgID:           orgID,
		ProjectID:       scope.ProjectID,
		ScheduleType:    db.TaskScheduleTypeImperative,
		TaskID:          "daily",
		DedupKey:        "internal-preview",
		UserDedupKey:    userDedupKey,
		Cron:            "0 10 * * *",
		Timezone:        "America/New_York",
		SecretBindings:  []byte(`{"TOKEN":"vault:preview-token"}`),
		Workspace:       []byte(`{"repository":"acme/app","ref":"preview"}`),
		RunOptions:      []byte(`{"queue":"preview"}`),
		Active:          false,
		InstanceID:      ids.ToPG(ids.New()),
		EnvironmentID:   environmentID,
		NextScheduledAt: pgTime(secondScheduledAt),
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
	if string(defaultSummary.Workspace) != `{"repository":"acme/app","ref":"main"}` {
		t.Fatalf("default workspace = %s", defaultSummary.Workspace)
	}
	if defaultSummary.Cron != "0 10 * * *" || !defaultSummary.NextScheduledAt.Time.Equal(secondScheduledAt) {
		t.Fatalf("default timing after shared schedule update = %+v", defaultSummary)
	}
	if string(previewSummary.Workspace) != `{"repository":"acme/app","ref":"preview"}` {
		t.Fatalf("preview workspace = %s", previewSummary.Workspace)
	}
	if !defaultSummary.InstanceActive || previewSummary.InstanceActive {
		t.Fatalf("instance active states = default %v preview %v", defaultSummary.InstanceActive, previewSummary.InstanceActive)
	}
}

func TestScheduleUpdateOnlyRefreshesSiblingInstancesWhenTimingChanges(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	environmentID := ids.ToPG(ids.New())
	if _, err := queries.CreateEnvironment(ctx, db.CreateEnvironmentParams{
		ID:        environmentID,
		OrgID:     orgID,
		ProjectID: scope.ProjectID,
		Slug:      "staging",
		Name:      "Staging",
	}); err != nil {
		t.Fatal(err)
	}

	userDedupKey := pgtype.Text{String: "shared-schedule", Valid: true}
	scheduledAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	first, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:      ids.ToPG(ids.New()),
		OrgID:           orgID,
		ProjectID:       scope.ProjectID,
		ScheduleType:    db.TaskScheduleTypeImperative,
		TaskID:          "sync",
		DedupKey:        "internal-shared",
		UserDedupKey:    userDedupKey,
		Cron:            "0 9 * * *",
		Timezone:        "UTC",
		SecretBindings:  []byte(`{}`),
		Workspace:       []byte(`{"repository":"acme/app","ref":"main"}`),
		RunOptions:      []byte(`{}`),
		Active:          true,
		InstanceID:      ids.ToPG(ids.New()),
		EnvironmentID:   scope.EnvironmentID,
		NextScheduledAt: pgTime(scheduledAt),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:      ids.ToPG(ids.New()),
		OrgID:           orgID,
		ProjectID:       scope.ProjectID,
		ScheduleType:    db.TaskScheduleTypeImperative,
		TaskID:          "sync",
		DedupKey:        "ignored-internal",
		UserDedupKey:    userDedupKey,
		Cron:            "0 9 * * *",
		Timezone:        "UTC",
		SecretBindings:  []byte(`{}`),
		Workspace:       []byte(`{"repository":"acme/app","ref":"staging"}`),
		RunOptions:      []byte(`{}`),
		Active:          true,
		InstanceID:      ids.ToPG(ids.New()),
		EnvironmentID:   environmentID,
		NextScheduledAt: pgTime(scheduledAt),
	})
	if err != nil {
		t.Fatal(err)
	}
	retryAt := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	if affected, err := queries.MarkScheduleInstanceTriggerFailed(ctx, db.MarkScheduleInstanceTriggerFailedParams{
		InstanceID:   second.InstanceID,
		Generation:   second.Generation,
		ScheduledAt:  pgTime(scheduledAt),
		ErrorMessage: "keep retry",
		RetryAfter:   pgTime(retryAt),
	}); err != nil {
		t.Fatal(err)
	} else if affected != 1 {
		t.Fatalf("mark failed affected = %d", affected)
	}

	if _, err := queries.UpdateSchedule(ctx, db.UpdateScheduleParams{
		TaskID:          "sync",
		ExternalID:      pgtype.Text{},
		Cron:            "0 9 * * *",
		Timezone:        "UTC",
		SecretBindings:  []byte(`{"TOKEN":"vault:default"}`),
		Workspace:       []byte(`{"repository":"acme/app","ref":"release"}`),
		RunOptions:      []byte(`{"queue":"default"}`),
		Active:          true,
		OrgID:           orgID,
		ProjectID:       scope.ProjectID,
		EnvironmentID:   scope.EnvironmentID,
		ScheduleID:      first.ScheduleID,
		NextScheduledAt: pgTime(scheduledAt),
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

	nextScheduledAt := scheduledAt.Add(time.Hour)
	if _, err := queries.UpdateSchedule(ctx, db.UpdateScheduleParams{
		TaskID:          "sync",
		ExternalID:      pgtype.Text{},
		Cron:            "0 10 * * *",
		Timezone:        "UTC",
		SecretBindings:  []byte(`{"TOKEN":"vault:default"}`),
		Workspace:       []byte(`{"repository":"acme/app","ref":"release"}`),
		RunOptions:      []byte(`{"queue":"default"}`),
		Active:          true,
		OrgID:           orgID,
		ProjectID:       scope.ProjectID,
		EnvironmentID:   scope.EnvironmentID,
		ScheduleID:      first.ScheduleID,
		NextScheduledAt: pgTime(nextScheduledAt),
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
	if sibling.Generation != second.Generation+1 || sibling.RetryAfter.Valid || sibling.TriggerAttemptCount != 0 || sibling.TriggerErrorMessage != "" || !sibling.NextScheduledAt.Time.Equal(nextScheduledAt) {
		t.Fatalf("sibling was not refreshed after timing change = %+v", sibling)
	}
}
