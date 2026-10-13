package main

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
)

func TestDemoEnvironmentSeedWithFreshPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := dbtest.Open(t).Pool
	var serverVersion int
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		t.Fatal(err)
	}
	if serverVersion < 180000 {
		t.Skipf("Postgres %d is older than the Helmr PostgreSQL 18 schema baseline; skipping demo environment seed integration test", serverVersion)
	}
	freshInit, err := migrate(ctx, pool)
	if err != nil {
		t.Fatalf("migrate fresh database: %v", err)
	}
	if !freshInit {
		t.Fatal("expected fresh database initialization")
	}
	q := db.New(pool)
	if _, err := q.CreateRegion(ctx, db.CreateRegionParams{ID: "dev-local", DisplayName: "Local"}); err != nil {
		t.Fatalf("bootstrap local region: %v", err)
	}
	if _, err := q.CreateWorkerGroup(ctx, db.CreateWorkerGroupParams{
		ID: pgvalue.NewUUIDv7(), TokenID: pgvalue.NewUUIDv7(), TokenHash: make([]byte, 32),
		RegionID: "dev-local", Name: "default",
	}); err != nil {
		t.Fatalf("bootstrap local worker group: %v", err)
	}
	cfg := devConfig{environmentExecutionLimits: devTestExecutionLimits(), bootstrap: config.Bootstrap{RegionID: "dev-local"}}
	if err := seedDevData(ctx, pool, cfg); err != nil {
		t.Fatalf("seed dev data: %v", err)
	}

	var definitions, triggers, computers, sessions, turns, runnable, events int
	if err := pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM agent_definitions WHERE environment_id=$1),
 (SELECT count(*) FROM trigger_activations WHERE environment_id=$1 AND active_until IS NOT NULL),
 (SELECT count(*) FROM computers WHERE environment_id=$1 AND preparation_failed_at IS NOT NULL),
 (SELECT count(*) FROM sessions WHERE environment_id=$1 AND status='closed'),
 (SELECT count(*) FROM turns WHERE environment_id=$1 AND status='failed'),
 (SELECT count(*) FROM turns WHERE environment_id=$1 AND status IN ('queued','running','finalizing'))+
 (SELECT count(*) FROM trigger_activations WHERE environment_id=$1 AND active_until IS NULL)+
 (SELECT count(*) FROM image_builds WHERE environment_id=$1)+
 (SELECT count(*) FROM computer_leases WHERE environment_id=$1)+
 (SELECT count(*) FROM session_processes WHERE environment_id=$1),
 (SELECT count(*) FROM session_events WHERE environment_id=$1)
 `, demoSeedEnvironmentID).Scan(&definitions, &triggers, &computers, &sessions, &turns, &runnable, &events); err != nil {
		t.Fatal(err)
	}
	if definitions != 1 || triggers != 1 || computers != 1 || sessions != 1 || turns != 1 || events != 3 || runnable != 0 {
		t.Fatalf("demo definitions/triggers/computers/sessions/turns/events/runnable=%d/%d/%d/%d/%d/%d/%d", definitions, triggers, computers, sessions, turns, events, runnable)
	}
	var chronological bool
	if err := pool.QueryRow(ctx, `SELECT s.created_at <= min(e.created_at)
 AND t.processing_closed_at=t.terminal_at
 AND max(e.created_at)=t.terminal_at
 AND d.created_at<=trigger.active_from AND d.created_at<=s.created_at
 AND spec.created_at<=s.created_at
 FROM sessions s JOIN session_events e ON (e.environment_id,e.session_id)=(s.environment_id,s.id)
 JOIN turns t ON (t.environment_id,t.session_id)=(s.environment_id,s.id)
 JOIN deployments d ON (d.environment_id,d.id)=(s.environment_id,s.deployment_id)
 JOIN trigger_activations trigger ON (trigger.environment_id,trigger.deployment_id)=(d.environment_id,d.id)
 JOIN image_specs spec ON spec.environment_id=s.environment_id
 WHERE s.environment_id=$1 AND s.id=$2
 GROUP BY s.created_at,t.processing_closed_at,t.terminal_at,d.created_at,trigger.active_from,spec.created_at`, demoSeedEnvironmentID, demoSeedSessionID).Scan(&chronological); err != nil || !chronological {
		t.Fatalf("demo history chronological=%v: %v", chronological, err)
	}
	caller := session.Caller{Kind: "user", ID: uuid.MustParse("00000000-0000-7000-8000-000000000101")}
	env := uuid.MustParse(demoSeedEnvironmentID)
	if _, err := session.Start(ctx, pool, nil, caller, session.StartRequest{EnvironmentID: env, Agent: "demo-agent", IdempotencyKey: "should-not-start", Input: []byte(`[{"type":"text","text":"work"}]`)}); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("unpromoted synthetic Deployment start=%v, want not ready", err)
	}
	if _, err := session.Enqueue(ctx, pool, nil, caller, session.EnqueueRequest{EnvironmentID: env, SessionID: uuid.MustParse(demoSeedSessionID), IdempotencyKey: "should-not-enqueue", Input: []byte(`[{"type":"text","text":"work"}]`)}); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("terminal demo enqueue=%v, want not ready", err)
	}
}

func TestDevSeedRestartPreservesEditsWithoutReseeding(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := dbtest.Open(t).Pool
	var serverVersion int
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		t.Fatal(err)
	}
	if serverVersion < 180000 {
		t.Skipf("Postgres %d is older than the Helmr PostgreSQL 18 schema baseline; skipping restart preservation test", serverVersion)
	}
	freshInit, err := migrate(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if !freshInit {
		t.Fatal("expected fresh database initialization")
	}
	q := db.New(pool)
	if _, err := q.CreateRegion(ctx, db.CreateRegionParams{ID: "dev-local", DisplayName: "Local"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateWorkerGroup(ctx, db.CreateWorkerGroupParams{
		ID: pgvalue.NewUUIDv7(), TokenID: pgvalue.NewUUIDv7(), TokenHash: make([]byte, 32),
		RegionID: "dev-local", Name: "default",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := devConfig{environmentExecutionLimits: devTestExecutionLimits(), bootstrap: config.Bootstrap{RegionID: "dev-local"}}
	if err := seedDevData(ctx, pool, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE environments
		   SET name = 'Renamed Production', color_hex = '#22C55E'
		 WHERE id = '00000000-0000-7000-8000-000000000401'
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agents SET key='renamed-demo' WHERE environment_id=$1 AND id=$2`, demoSeedEnvironmentID, demoSeedAgentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM trigger_activations WHERE id = $1`, demoSeedTriggerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE environments SET current_deployment_id = NULL WHERE id = $1
	`, demoSeedEnvironmentID); err != nil {
		t.Fatal(err)
	}

	freshInit, err = migrate(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if freshInit {
		t.Fatal("expected existing database on restart migrate")
	}

	var name, color, agentName string
	var triggerCount int
	var currentDeploymentID *string
	if err := pool.QueryRow(ctx, `
		SELECT name, color_hex FROM environments WHERE id = '00000000-0000-7000-8000-000000000401'
	`).Scan(&name, &color); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT key FROM agents WHERE environment_id=$1 AND id=$2`, demoSeedEnvironmentID, demoSeedAgentID).Scan(&agentName); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM trigger_activations WHERE id = $1`, demoSeedTriggerID).Scan(&triggerCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT current_deployment_id::text FROM environments WHERE id = $1
	`, demoSeedEnvironmentID).Scan(&currentDeploymentID); err != nil {
		t.Fatal(err)
	}
	if name != "Renamed Production" || color != "#22C55E" {
		t.Fatalf("environment after restart = %q/%q, want Renamed Production/#22C55E", name, color)
	}
	if agentName != "renamed-demo" {
		t.Fatalf("agent key after restart=%q, want renamed-demo", agentName)
	}
	if triggerCount != 0 {
		t.Fatalf("deleted trigger count after restart = %d, want 0", triggerCount)
	}
	if currentDeploymentID != nil {
		t.Fatalf("current_deployment_id after restart = %v, want NULL", currentDeploymentID)
	}
}
