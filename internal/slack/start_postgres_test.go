package slack

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/jackc/pgx/v5"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/helmrdotdev/helmr/internal/slackroute"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

type startPreflightFunc func(context.Context, slackroute.StartContext) (json.RawMessage, error)

func (f startPreflightFunc) PrepareStart(ctx context.Context, value slackroute.StartContext) (json.RawMessage, error) {
	return f(ctx, value)
}

func TestRuntimeStartDoesNotDependOnIngressDelivery(t *testing.T) {
	f := newStatusFixture(t)
	sessiontest.PrepareAdmission(t, f.Fixture)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET authorization_lost_at=clock_timestamp(); UPDATE sessions SET origin_slack_connection_id=$2 WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.Session, f.publication)
	e := session.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1, AuthenticatedHostVersion: 1}
	host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	caller := session.Caller{Kind: "session", ID: f.Session, Execution: e, Host: &host, MCPScopeID: f.MCPScope}
	result, err := Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, nil, caller, session.StartRequest{EnvironmentID: f.Environment, Agent: "agent", IdempotencyKey: "independent", Input: json.RawMessage(`[{"type":"text","text":"independent work"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	var correct bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT deployment_id=$2 AND requester_session_id=$3 AND parent_session_id IS NULL AND NOT EXISTS(SELECT 1 FROM slack_threads WHERE front_session_id=$1) FROM sessions WHERE id=$1`, result.SessionID, f.Deployment, f.Session).Scan(&correct); err != nil || !correct {
		t.Fatal(correct, err)
	}
	channel := "C1"
	_, err = Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, nil, caller, session.StartRequest{EnvironmentID: f.Environment, Agent: "agent", IdempotencyKey: "unsupported-delivery", SlackChannelID: &channel, Input: json.RawMessage(`[{"type":"text","text":"work"}]`)})
	if !errors.Is(err, session.ErrInvalidInput) {
		t.Fatalf("runtime channel creation: %v", err)
	}

}

func TestSlackStartWaitsForFirstChannelBindingBeforeEnvironment(t *testing.T) {
	f := newStatusFixture(t)
	sessiontest.PrepareAdmission(t, f.Fixture)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	promotion, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer promotion.Rollback(context.Background())
	// Promotion pins its channel binding before taking the Environment lock.
	route, err := slackroute.ReadConnection(ctx, promotion, f.Environment, f.Agent, "C2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = slackroute.ResolveChannelBinding(ctx, promotion, route); err != nil {
		t.Fatal(err)
	}
	admission, err := f.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		channel := "C2"
		_, err := Start(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, admission, nil, startPreflightFunc(func(context.Context, slackroute.StartContext) (json.RawMessage, error) {
			return json.RawMessage(`{"text":"Started agent.","mrkdwn":false}`), nil
		}), session.UserCaller(f.Principal(t)), session.StartRequest{
			EnvironmentID: f.Environment, Agent: "agent", IdempotencyKey: "first-channel", SlackChannelID: &channel,
			Input: json.RawMessage(`[{"type":"text","text":"work"}]`),
		})
		done <- err
	})
	defer func() { cancel(); promotion.Rollback(context.Background()); wg.Wait(); admission.Release() }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err = f.Pool.QueryRow(ctx, `SELECT COALESCE(wait_event_type='Lock' AND query LIKE '%INSERT INTO slack_channel_bindings%',false) FROM pg_stat_activity WHERE pid=$1`, admission.Conn().PgConn().PID()).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("admission did not wait for binding: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	// This would fail immediately if admission took Environment before waiting
	// for the binding, proving the inverse edge without relying on deadlock timing.
	var locked uuid.UUID
	if err = promotion.QueryRow(ctx, `SELECT id FROM environments WHERE id=$1 FOR NO KEY UPDATE NOWAIT`, f.Environment).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	if err = promotion.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
