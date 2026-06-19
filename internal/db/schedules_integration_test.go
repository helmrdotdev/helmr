package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDeleteScheduleKeepsParentUntilLastInstance(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)

	secondEnvironmentID := uuid.Must(uuid.NewV7())
	secondEnvironmentSlug := "env-" + shortUUID(secondEnvironmentID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO environments (id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, $4, 'Env 2', '#3366ff')
	`, secondEnvironmentID, ids.orgID, ids.projectID, secondEnvironmentSlug); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (org_id, project_id, environment_id, task_id)
		VALUES ($1, $2, $3, 'approval-task')
	`, ids.orgID, ids.projectID, secondEnvironmentID); err != nil {
		t.Fatal(err)
	}

	scheduleID := uuid.Must(uuid.NewV7())
	firstInstanceID := uuid.Must(uuid.NewV7())
	secondInstanceID := uuid.Must(uuid.NewV7())
	first, err := queries.CreateDeclarativeSchedule(ctx, db.CreateDeclarativeScheduleParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		DedupKey:      "approval-daily",
		TaskID:        "approval-task",
		ExternalID:    pgtype.Text{String: "approval-daily", Valid: true},
		Cron:          "0 9 * * *",
		Timezone:      "UTC",
		ScheduleID:    pgvalue.UUID(scheduleID),
		InstanceID:    pgvalue.UUID(firstInstanceID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunOptions:    []byte(`{}`),
		Active:        true,
		NextFireAt:    pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := pgvalue.MustUUIDValue(first.ScheduleID); got != scheduleID {
		t.Fatalf("schedule id = %s, want %s", got, scheduleID)
	}
	if _, err := queries.CreateDeclarativeSchedule(ctx, db.CreateDeclarativeScheduleParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		DedupKey:      "approval-daily",
		TaskID:        "approval-task",
		ExternalID:    pgtype.Text{String: "approval-daily", Valid: true},
		Cron:          "0 9 * * *",
		Timezone:      "UTC",
		ScheduleID:    pgvalue.UUID(uuid.Must(uuid.NewV7())),
		InstanceID:    pgvalue.UUID(secondInstanceID),
		EnvironmentID: pgvalue.UUID(secondEnvironmentID),
		RunOptions:    []byte(`{}`),
		Active:        true,
		NextFireAt:    pgvalue.Timestamptz(time.Now().Add(2 * time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}

	deleted, err := queries.DeleteSchedule(ctx, db.DeleteScheduleParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		ScheduleID:    pgvalue.UUID(scheduleID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("first delete affected %d instances, want 1", deleted)
	}
	assertScheduleCounts(t, ctx, pool, scheduleID, 1, 1)

	deleted, err = queries.DeleteSchedule(ctx, db.DeleteScheduleParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		ScheduleID:    pgvalue.UUID(scheduleID),
		EnvironmentID: pgvalue.UUID(secondEnvironmentID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("second delete affected %d instances, want 1", deleted)
	}
	assertScheduleCounts(t, ctx, pool, scheduleID, 0, 0)
}

func assertScheduleCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, scheduleID uuid.UUID, wantSchedules int, wantInstances int) {
	t.Helper()
	var schedules int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_schedules WHERE id = $1`, scheduleID).Scan(&schedules); err != nil {
		t.Fatal(err)
	}
	if schedules != wantSchedules {
		t.Fatalf("schedule count = %d, want %d", schedules, wantSchedules)
	}
	var instances int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_schedule_instances WHERE schedule_id = $1`, scheduleID).Scan(&instances); err != nil {
		t.Fatal(err)
	}
	if instances != wantInstances {
		t.Fatalf("schedule instance count = %d, want %d", instances, wantInstances)
	}
}
