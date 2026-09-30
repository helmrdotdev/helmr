package workergroup

import (
	"context"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
)

func TestBootstrapCreatesOneRegionGroupAndToken(t *testing.T) {
	ctx := context.Background()
	database := dbtest.Open(t)
	if err := schema.Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	token, err := auth.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	cfg := BootstrapConfig{
		RegionID:          "local",
		RegionDisplayName: "Local", GroupName: "default", EnrollmentToken: token.Raw,
	}
	if err := Bootstrap(ctx, database.Pool, cfg); err != nil {
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
	if region.DisplayName != "Local" || group.Status != db.WorkerGroupStatusActive {
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

func TestBootstrapPreservesExistingRowsWithoutParsingToken(t *testing.T) {
	ctx := context.Background()
	database := dbtest.Open(t)
	if err := schema.Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	token, err := auth.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	initial := BootstrapConfig{
		RegionID:          "local",
		RegionDisplayName: "Original", GroupName: "default", EnrollmentToken: token.Raw,
	}
	if err := Bootstrap(ctx, database.Pool, initial); err != nil {
		t.Fatal(err)
	}
	restart := initial
	restart.RegionDisplayName = "Changed"
	for _, unusedToken := range []string{"", " invalid "} {
		restart.EnrollmentToken = unusedToken
		if err := Bootstrap(ctx, database.Pool, restart); err != nil {
			t.Fatal(err)
		}
	}
	region, err := db.New(database.Pool).GetRegion(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if region.DisplayName != "Original" {
		t.Fatalf("display name = %q", region.DisplayName)
	}
	var tokenCount int
	if err := database.Pool.QueryRow(ctx, `SELECT count(*) FROM worker_group_tokens`).Scan(&tokenCount); err != nil {
		t.Fatal(err)
	}
	if tokenCount != 1 {
		t.Fatalf("token count = %d, want 1", tokenCount)
	}
}

func TestBootstrapCreatesAnotherSeedWithoutChangingTheExistingSeed(t *testing.T) {
	ctx := context.Background()
	database := dbtest.Open(t)
	if err := schema.Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	firstToken, err := auth.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := Bootstrap(ctx, database.Pool, BootstrapConfig{
		RegionID: "primary", RegionDisplayName: "Primary",
		GroupName: "default", EnrollmentToken: firstToken.Raw,
	}); err != nil {
		t.Fatal(err)
	}
	secondToken, err := auth.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := Bootstrap(ctx, database.Pool, BootstrapConfig{
		RegionID: "secondary", RegionDisplayName: "Secondary",
		GroupName: "default", EnrollmentToken: secondToken.Raw,
	}); err != nil {
		t.Fatal(err)
	}

	var regionCount, groupCount, tokenCount int
	if err := database.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM regions),
		       (SELECT count(*) FROM worker_groups),
		       (SELECT count(*) FROM worker_group_tokens)
	`).Scan(&regionCount, &groupCount, &tokenCount); err != nil {
		t.Fatal(err)
	}
	if regionCount != 2 || groupCount != 2 || tokenCount != 2 {
		t.Fatalf("counts = region %d group %d token %d, want 2/2/2", regionCount, groupCount, tokenCount)
	}
	primary, err := db.New(database.Pool).GetRegion(ctx, "primary")
	if err != nil {
		t.Fatal(err)
	}
	if primary.DisplayName != "Primary" {
		t.Fatalf("primary display name = %q, want Primary", primary.DisplayName)
	}
}

func TestBootstrapSerializesConcurrentBootstrap(t *testing.T) {
	ctx := context.Background()
	database := dbtest.Open(t)
	if err := schema.Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	token, err := auth.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	cfg := BootstrapConfig{
		RegionID:          "local",
		RegionDisplayName: "Local", GroupName: "default", EnrollmentToken: token.Raw,
	}
	errorsByReplica := make([]error, 2)
	var wait sync.WaitGroup
	for index := range errorsByReplica {
		wait.Go(func() {
			errorsByReplica[index] = Bootstrap(ctx, database.Pool, cfg)
		})
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

func TestBootstrapRollsBackRegionWhenMissingGroupTokenIsInvalid(t *testing.T) {
	ctx := context.Background()
	database := dbtest.Open(t)
	if err := schema.Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	err := Bootstrap(ctx, database.Pool, BootstrapConfig{
		RegionID: "local", RegionDisplayName: "Local",
		GroupName: "default", EnrollmentToken: "invalid",
	})
	if err == nil {
		t.Fatal("bootstrap accepted an invalid token")
	}
	var regionCount, groupCount int
	if err := database.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM regions), (SELECT count(*) FROM worker_groups)
	`).Scan(&regionCount, &groupCount); err != nil {
		t.Fatal(err)
	}
	if regionCount != 0 || groupCount != 0 {
		t.Fatalf("counts = region %d group %d, want 0/0", regionCount, groupCount)
	}
}
