package main

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"log/slog"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/slack"
	"github.com/jackc/pgx/v5/pgxpool"
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
	cfg := devConfig{auditPolicy: auditpolicy.Policy{RetentionSeconds: 3600}, environmentExecutionLimits: devTestExecutionLimits(), bootstrap: config.Bootstrap{RegionID: "dev-local"}}
	if err := seedDevData(ctx, pool, cfg); err != nil {
		t.Fatalf("seed dev data: %v", err)
	}

	var definitions, triggers, computers, openSessions, closedSessions, cancelledSessions, failedTurns, holds, runnable, events int
	var current bool
	if err := pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM agent_definitions d JOIN environments e ON e.id=d.environment_id AND e.current_deployment_id=d.deployment_id WHERE d.environment_id=$1),
 (SELECT count(*) FROM trigger_activations WHERE environment_id=$1 AND active_until IS NULL AND next_fire_at>now()),
 (SELECT count(*) FROM computers WHERE environment_id=$1 AND preparation_failed_at IS NOT NULL),
 (SELECT count(*) FROM sessions WHERE environment_id=$1 AND status='open'),
 (SELECT count(*) FROM sessions WHERE environment_id=$1 AND status='closed'),
 (SELECT count(*) FROM sessions WHERE environment_id=$1 AND close_how='cancel'),
 (SELECT count(*) FROM turns WHERE environment_id=$1 AND status='failed'),
 (SELECT count(*) FROM session_holds WHERE environment_id=$1 AND released_at IS NULL),
 (SELECT count(*) FROM turns WHERE environment_id=$1 AND status IN ('queued','running'))+
 (SELECT count(*) FROM turn_asks WHERE environment_id=$1 AND status='pending')+
 (SELECT count(*) FROM turn_messages WHERE environment_id=$1 AND status IN ('admitted','started'))+
 (SELECT count(*) FROM image_builds WHERE environment_id=$1)+
 (SELECT count(*) FROM computer_leases WHERE environment_id=$1 AND fenced_at IS NULL)+
 (SELECT count(*) FROM session_processes WHERE environment_id=$1 AND fenced_at IS NULL)+
 (SELECT count(*) FROM computer_saves WHERE environment_id=$1 AND status<>'published')+
 (SELECT count(*) FROM worker_hosts h JOIN worker_groups g ON g.id=h.worker_group_id WHERE g.name='console-demo-retired' AND (h.status<>'lost' OR g.status<>'disabled')),
 (SELECT count(*) FROM session_events WHERE environment_id=$1),
 (SELECT current_deployment_id IS NOT NULL FROM environments WHERE id=$1)
 `, demoSeedEnvironmentID).Scan(&definitions, &triggers, &computers, &openSessions, &closedSessions, &cancelledSessions, &failedTurns, &holds, &runnable, &events, &current); err != nil {
		t.Fatal(err)
	}
	if !current || definitions != 3 || triggers != 1 || computers != 1 || openSessions != 3 || closedSessions != 7 || cancelledSessions != 2 || failedTurns != 5 || holds != 1 || events < 200 || runnable != 0 {
		t.Fatalf("demo current/definitions/triggers/computers/open/closed/cancelled/failed/holds/events/runnable=%v/%d/%d/%d/%d/%d/%d/%d/%d/%d/%d", current, definitions, triggers, computers, openSessions, closedSessions, cancelledSessions, failedTurns, holds, events, runnable)
	}
	// Every Session's counters match its rows, each Turn has its own
	// turn.queued event, and events are chronological within the Session.
	var inconsistent int
	if err := pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM sessions s WHERE s.environment_id=$1 AND (
   s.next_event_seq-1 <> (SELECT count(*) FROM session_events e WHERE (e.environment_id,e.session_id)=(s.environment_id,s.id))
   OR s.next_event_seq-1 <> (SELECT COALESCE(max(seq),0) FROM session_events e WHERE (e.environment_id,e.session_id)=(s.environment_id,s.id))
   OR s.next_turn_seq-1 <> (SELECT count(*) FROM turns t WHERE (t.environment_id,t.session_id)=(s.environment_id,s.id))
   OR s.next_turn_seq-1 <> (SELECT COALESCE(max(seq),0) FROM turns t WHERE (t.environment_id,t.session_id)=(s.environment_id,s.id))
   OR s.created_at > (SELECT min(created_at) FROM session_events e WHERE (e.environment_id,e.session_id)=(s.environment_id,s.id))
   OR (s.status='closed') <> (s.history_eligible_at IS NOT NULL)))+
 (SELECT count(*) FROM turns t WHERE t.environment_id=$1 AND NOT EXISTS(SELECT 1 FROM session_events e
   WHERE (e.environment_id,e.session_id)=(t.environment_id,t.session_id) AND e.turn_id=t.id AND e.kind='turn.queued'))+
 (SELECT count(*) FROM session_events WHERE environment_id=$1 AND kind='turn.finalizing')+
 (SELECT count(*) FROM turns t JOIN computer_saves s ON (s.environment_id,s.turn_id)=(t.environment_id,t.id)
   WHERE t.environment_id=$1 AND (t.terminal_at>s.requested_at OR t.terminal_at>=s.captured_at))+
 (SELECT count(*) FROM session_events e JOIN session_events later ON (later.environment_id,later.session_id)=(e.environment_id,e.session_id) AND later.seq>e.seq AND later.created_at<e.created_at WHERE e.environment_id=$1)+
 (SELECT count(*) FROM session_events e JOIN sessions s ON (s.environment_id,s.id)=(e.environment_id,e.session_id) JOIN computers c ON (c.environment_id,c.id)=(s.environment_id,s.computer_id)
   WHERE e.environment_id=$1 AND c.deleted_at IS NOT NULL AND e.created_at>c.deleted_at)
 `, demoSeedEnvironmentID).Scan(&inconsistent); err != nil || inconsistent != 0 {
		t.Fatalf("demo history inconsistencies=%d: %v", inconsistent, err)
	}
	var longTurns, longCompleted, longFailed, longInterrupted, longEvents, childSessions, expiredEvents int
	var jsonInput string
	if err := pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM turns WHERE environment_id=$1 AND session_id=$2),
 (SELECT count(*) FROM turns WHERE environment_id=$1 AND session_id=$2 AND status='completed' AND result IS NOT NULL AND response IS NOT NULL),
 (SELECT count(*) FROM turns WHERE environment_id=$1 AND session_id=$2 AND status='failed' AND error_message IS NOT NULL),
 (SELECT count(*) FROM turns WHERE environment_id=$1 AND session_id=$2 AND status='interrupted'),
 (SELECT count(*) FROM session_events WHERE environment_id=$1 AND session_id=$2),
 (SELECT count(*) FROM sessions WHERE environment_id=$1 AND parent_session_id=$2 AND requester_session_id=$2 AND origin_turn_id IS NOT NULL),
 (SELECT count(*) FROM session_events e JOIN sessions s ON (s.environment_id,s.id)=(e.environment_id,e.session_id) WHERE e.environment_id=$1 AND e.session_id=$3 AND e.data IS NULL AND s.history_expired_at IS NOT NULL),
 (SELECT convert_from(input,'UTF8') FROM turns WHERE environment_id=$1 AND session_id=$4 AND seq=1)
 `, demoSeedEnvironmentID, demoSeedLongSessionID, demoSeedExpiredSessionID, demoSeedJSONSessionID).Scan(&longTurns, &longCompleted, &longFailed, &longInterrupted, &longEvents, &childSessions, &expiredEvents, &jsonInput); err != nil {
		t.Fatal(err)
	}
	if longTurns != 45 || longCompleted != 40 || longFailed != 2 || longInterrupted != 3 || longEvents < 200 || childSessions != 2 || expiredEvents == 0 || jsonInput[0] != '{' {
		t.Fatalf("long turns/completed/failed/interrupted/events/children/expired events/json input=%d/%d/%d/%d/%d/%d/%d/%q", longTurns, longCompleted, longFailed, longInterrupted, longEvents, childSessions, expiredEvents, jsonInput)
	}
	// The dev control plane's background owners leave the seeded history alone.
	before := demoFingerprint(ctx, t, pool)
	allocator, err := dispatch.NewAllocator(pool, auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	loopCtx, stopLoops := context.WithTimeout(ctx, 3*time.Second)
	var loops sync.WaitGroup
	for _, run := range []func(context.Context) error{
		func(ctx context.Context) error { return allocator.Run(ctx, log, nil) },
		func(ctx context.Context) error {
			return command.ReconcileCommands(ctx, auditpolicy.Policy{
				RetentionSeconds: 3600,
			}, pool, log)
		},
		func(ctx context.Context) error { return slack.RunHistoryRetention(ctx, pool, log) },
		func(ctx context.Context) error {
			return session.RunSessionLifecycle(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, pool, log)
		},
		func(ctx context.Context) error {
			return computer.RunRetention(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, pool, log)
		},
		func(ctx context.Context) error {
			return dispatch.RunLeaseLifecycle(ctx, auditpolicy.Policy{
				RetentionSeconds: 3600,
			}, pool, log)
		},
		func(ctx context.Context) error {
			return dispatch.RunImageBuildLifecycle(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, pool, log)
		},
	} {
		loops.Go(func() { _ = run(loopCtx) })
	}
	loops.Wait()
	stopLoops()
	if after := demoFingerprint(ctx, t, pool); after != before {
		t.Fatalf("background reconciliation changed the demo history:\n%s\n%s", before, after)
	}
	// The Overview reads the held Session and the recent failure as waiting work.
	var organization uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT org_id FROM environments WHERE id=$1`, uuid.MustParse(demoSeedEnvironmentID)).Scan(&organization); err != nil {
		t.Fatal(err)
	}
	human := dbtest.BrowserPrincipal(t, pool, organization, uuid.MustParse("00000000-0000-7000-8000-000000000101"))
	attention, err := session.ListAttention(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, pool, session.UserCaller(human), uuid.MustParse(demoSeedEnvironmentID))
	if err != nil || attention.HoldCount != 1 || attention.FailedTurnCount != 1 || attention.QuestionCount != 0 ||
		attention.Holds[0].SessionID.String() != demoSeedHeldSessionID || attention.FailedTurns[0].ID.String() != demoSeedFailedTurnID {
		t.Fatalf("demo attention: %+v %v", attention, err)
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
	caller := session.UserCaller(human)
	env := uuid.MustParse(demoSeedEnvironmentID)
	// The promoted Deployment is synthetic: its image spec has no Computer seed
	// profile, so work sent to it is refused before anything is admitted.
	if _, err := session.Start(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, pool, nil, caller, session.StartRequest{EnvironmentID: env, Agent: "demo-agent", IdempotencyKey: "should-not-start", Input: []byte(`[{"type":"text","text":"work"}]`)}); !errors.Is(err, computer.ErrInvalidInput) {
		t.Fatalf("synthetic Deployment start=%v, want refused", err)
	}
	var admitted int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE environment_id=$1`, demoSeedEnvironmentID).Scan(&admitted); err != nil || admitted != 10 {
		t.Fatalf("Sessions after refused start=%d %v, want the 10 seeded", admitted, err)
	}
	if _, err := session.Enqueue(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, pool, nil, caller, session.EnqueueRequest{EnvironmentID: env, SessionID: uuid.MustParse(demoSeedSessionID), IdempotencyKey: "should-not-enqueue", Input: []byte(`[{"type":"text","text":"work"}]`)}); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("closed synthetic history enqueue=%v, want not ready", err)
	}
}

func demoFingerprint(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var fingerprint string
	if err := pool.QueryRow(ctx, `SELECT concat_ws(' ',
 (SELECT md5(string_agg(x::text,'|' ORDER BY x::text)) FROM sessions x WHERE environment_id=$1),
 (SELECT md5(string_agg(x::text,'|' ORDER BY x::text)) FROM turns x WHERE environment_id=$1),
 (SELECT md5(string_agg(x::text,'|' ORDER BY x::text)) FROM session_events x WHERE environment_id=$1),
 (SELECT md5(string_agg(x::text,'|' ORDER BY x::text)) FROM session_processes x WHERE environment_id=$1),
 (SELECT md5(string_agg(x::text,'|' ORDER BY x::text)) FROM session_holds x WHERE environment_id=$1),
 (SELECT md5(string_agg(x::text,'|' ORDER BY x::text)) FROM turn_asks x WHERE environment_id=$1),
 (SELECT md5(string_agg(x::text,'|' ORDER BY x::text)) FROM turn_messages x WHERE environment_id=$1),
 (SELECT md5(string_agg(x::text,'|' ORDER BY x::text)) FROM computer_leases x WHERE environment_id=$1),
 (SELECT md5(string_agg(x::text,'|' ORDER BY x::text)) FROM computer_saves x WHERE environment_id=$1),
 (SELECT md5(string_agg(x::text,'|' ORDER BY x::text)) FROM computers x WHERE environment_id=$1),
 (SELECT md5(string_agg(x::text,'|' ORDER BY x::text)) FROM worker_hosts x WHERE id=$2))`, demoSeedEnvironmentID, demoSeedRetiredHostID).Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}
	return fingerprint
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
	cfg := devConfig{auditPolicy: auditpolicy.Policy{RetentionSeconds: 3600}, environmentExecutionLimits: devTestExecutionLimits(), bootstrap: config.Bootstrap{RegionID: "dev-local"}}
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
