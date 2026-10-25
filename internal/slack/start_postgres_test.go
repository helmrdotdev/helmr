package slack

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/helmrdotdev/helmr/internal/slackroute"
	"github.com/jackc/pgx/v5"
)

type startPreflightFunc func(context.Context, slackroute.StartContext) (json.RawMessage, error)

func (f startPreflightFunc) PrepareStart(ctx context.Context, value slackroute.StartContext) (json.RawMessage, error) {
	return f(ctx, value)
}

func TestSlackStartSourceRetainsPositivelyBoundSuppressedOpening(t *testing.T) {
	f := newStatusFixture(t)
	sessiontest.PrepareAdmission(t, f.Fixture)
	channel := "C1"
	req := session.StartRequest{EnvironmentID: f.Environment, Agent: "agent", IdempotencyKey: "root", Input: json.RawMessage(`[{"type":"text","text":"work 1"}]`)}
	req.SlackChannelID = &channel
	preflight := startPreflightFunc(func(context.Context, slackroute.StartContext) (json.RawMessage, error) {
		return json.RawMessage(`{"text":"Started agent.","mrkdwn":false}`), nil
	})
	root, err := Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, preflight, session.Caller{Kind: "user", ID: f.User}, req)
	if err != nil {
		t.Fatal(err)
	}
	read := func() (*slackroute.StartSource, error) {
		var source *slackroute.StartSource
		err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
			var err error
			source, err = readSlackStartSource(t.Context(), tx, f.Environment, root.SessionID)
			return err
		})
		return source, err
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET status='suppressed',suppressed_revision=desired_revision,error='publication_authority_unavailable' WHERE session_id=$1`, root.SessionID)
	if _, err = read(); !errors.Is(err, ErrSourceConversationUnavailable) {
		t.Fatal("unconfirmed unavailable root accepted", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET thread_ts='123.789' WHERE front_session_id=$1; UPDATE slack_posts SET message_ts='123.789',confirmed_revision=desired_revision,posted_at=clock_timestamp() WHERE session_id=$1`, pgx.QueryExecModeSimpleProtocol, root.SessionID)
	if source, err := read(); err != nil || source == nil || source.ThreadTS != "123.789" {
		t.Fatal("positive root discarded", source, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET delivery_disposed_at=clock_timestamp(),delivery_disposed_by=$2,closed_at=clock_timestamp() WHERE session_id=$1`, root.SessionID, f.User)
	if _, err = read(); !errors.Is(err, ErrSourceConversationUnavailable) {
		t.Fatal("explicit abandonment revived", err)
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
	wg.Add(1)
	go func() {
		defer wg.Done()
		channel := "C2"
		_, err := Start(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, admission, nil, startPreflightFunc(func(context.Context, slackroute.StartContext) (json.RawMessage, error) {
			return json.RawMessage(`{"text":"Started agent.","mrkdwn":false}`), nil
		}), session.Caller{Kind: "user", ID: f.User}, session.StartRequest{
			EnvironmentID: f.Environment, Agent: "agent", IdempotencyKey: "first-channel", SlackChannelID: &channel,
			Input: json.RawMessage(`[{"type":"text","text":"work"}]`),
		})
		done <- err
	}()
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
