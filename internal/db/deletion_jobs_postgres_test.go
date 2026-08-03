package db_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestEnvironmentImageCacheRetirementLifecyclePostgres(t *testing.T) {
	ctx := t.Context()
	pool := newPostgresDB(t, ctx)
	queries := db.New(pool)
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO organizations (id, name, slug)
		VALUES ($1, 'Default', 'default')
	`, dbtest.DefaultOrgID)
	environmentID := uuid.Must(uuid.NewV7())
	job, err := queries.CreateDeletionJob(ctx, db.CreateDeletionJobParams{
		ID:                   pgvalue.NewUUIDv7(),
		OrgID:                pgvalue.UUID(dbtest.DefaultOrgID),
		TargetType:           db.DeletionJobTargetTypeEnvironment,
		TargetID:             pgvalue.UUID(environmentID),
		TargetSlug:           "retired",
		TargetName:           "Retired",
		RequestedByPrincipal: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkDeletionJobRunning(ctx, db.MarkDeletionJobRunningParams{OrgID: job.OrgID, ID: job.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CompleteDeletionJob(ctx, db.CompleteDeletionJobParams{
		OrgID:         job.OrgID,
		ID:            job.ID,
		DeletedCounts: json.RawMessage(`{"environments":1}`),
	}); err != nil {
		t.Fatal(err)
	}
	if rows, err := queries.ListDueEnvironmentImageCacheRetirements(ctx, 10); err != nil || len(rows) != 0 {
		t.Fatalf("fresh candidates = %+v, %v", rows, err)
	}
	dbtest.MustExec(t, ctx, pool, `
		UPDATE deletion_jobs
		   SET completed_at = $2,
		       deleted_counts = deleted_counts || '{"image_cache_repositories":0}'::jsonb
		 WHERE id = $1
	`, job.ID, time.Now().Add(-8*24*time.Hour))
	rows, err := queries.ListDueEnvironmentImageCacheRetirements(ctx, 10)
	if err != nil || len(rows) != 1 || rows[0].ID != job.ID || rows[0].TargetID != pgvalue.UUID(environmentID) {
		t.Fatalf("due candidates = %+v, %v", rows, err)
	}
	marked, err := queries.MarkEnvironmentImageCacheRetired(ctx, db.MarkEnvironmentImageCacheRetiredParams{
		ID: job.ID, EnvironmentID: pgvalue.UUID(environmentID),
	})
	if err != nil || marked != 1 {
		t.Fatalf("mark = %d, %v", marked, err)
	}
	if rows, err := queries.ListDueEnvironmentImageCacheRetirements(ctx, 10); err != nil || len(rows) != 0 {
		t.Fatalf("marked candidates = %+v, %v", rows, err)
	}
	if marked, err := queries.MarkEnvironmentImageCacheRetired(ctx, db.MarkEnvironmentImageCacheRetiredParams{
		ID: job.ID, EnvironmentID: pgvalue.UUID(environmentID),
	}); err != nil || marked != 0 {
		t.Fatalf("replayed mark = %d, %v", marked, err)
	}
}
