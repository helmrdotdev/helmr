package bootstrap

import (
	"context"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestApplyCreatesOneRegionGroupAndToken(t *testing.T) {
	ctx := context.Background()
	database := dbtest.Open(t)
	if err := schema.Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	token, err := workergroup.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Enabled: true, RegionID: "local", RegionProvider: "local", RegionProviderRegion: "local",
		RegionDisplayName: "Local", WorkerGroupName: "default", WorkerToken: token.Raw,
	}
	if err := Apply(ctx, database.Pool, cfg); err != nil {
		t.Fatal(err)
	}
	q := db.New(database.Pool)
	region, err := q.GetRegion(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	group, err := q.GetWorkerGroupByRegionName(ctx, db.GetWorkerGroupByRegionNameParams{RegionID: "local", Name: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if region.DisplayName != "Local" || group.State != db.WorkerGroupStateActive || !group.AllowsRun || !group.AllowsBuild {
		t.Fatalf("region = %+v group = %+v", region, group)
	}
	var tokenCount int
	if err := database.Pool.QueryRow(ctx, `SELECT count(*) FROM worker_group_tokens`).Scan(&tokenCount); err != nil {
		t.Fatal(err)
	}
	if tokenCount != 1 {
		t.Fatalf("token count = %d", tokenCount)
	}
}

func TestApplyPreservesExistingRowsWithoutToken(t *testing.T) {
	ctx := context.Background()
	database := dbtest.Open(t)
	if err := schema.Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	token, err := workergroup.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	initial := Config{
		Enabled: true, RegionID: "local", RegionProvider: "local", RegionProviderRegion: "local",
		RegionDisplayName: "Original", WorkerGroupName: "default", WorkerToken: token.Raw,
	}
	if err := Apply(ctx, database.Pool, initial); err != nil {
		t.Fatal(err)
	}
	restart := initial
	restart.RegionDisplayName = "Changed"
	restart.WorkerToken = ""
	if err := Apply(ctx, database.Pool, restart); err != nil {
		t.Fatal(err)
	}
	region, err := db.New(database.Pool).GetRegion(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if region.DisplayName != "Original" {
		t.Fatalf("display name = %q", region.DisplayName)
	}
}

func TestApplySerializesConcurrentBootstrap(t *testing.T) {
	ctx := context.Background()
	database := dbtest.Open(t)
	if err := schema.Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	token, err := workergroup.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Enabled: true, RegionID: "local", RegionProvider: "local", RegionProviderRegion: "local",
		RegionDisplayName: "Local", WorkerGroupName: "default", WorkerToken: token.Raw,
	}
	errorsByReplica := make([]error, 2)
	var wait sync.WaitGroup
	for index := range errorsByReplica {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsByReplica[index] = Apply(ctx, database.Pool, cfg)
		}()
	}
	wait.Wait()
	for _, err := range errorsByReplica {
		if err != nil {
			t.Fatal(err)
		}
	}
	var regionCount, groupCount, tokenCount int
	if err := database.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM regions),
		       (SELECT count(*) FROM worker_groups),
		       (SELECT count(*) FROM worker_group_tokens)
	`).Scan(&regionCount, &groupCount, &tokenCount); err != nil {
		t.Fatal(err)
	}
	if regionCount != 1 || groupCount != 1 || tokenCount != 1 {
		t.Fatalf("counts = region %d group %d token %d", regionCount, groupCount, tokenCount)
	}
}

func TestApplyDisabledAllowsEmptyDatabase(t *testing.T) {
	if err := Apply(context.Background(), nil, Config{}); err != nil {
		t.Fatal(err)
	}
}
