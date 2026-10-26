package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/slack"

	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/slackroute"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type startPreflightFunc func(context.Context, slackroute.StartContext) (json.RawMessage, error)

func (f startPreflightFunc) PrepareStart(ctx context.Context, value slackroute.StartContext) (json.RawMessage, error) {
	return f(ctx, value)
}
func acceptedStartPreflight(context.Context, slackroute.StartContext) (json.RawMessage, error) {
	return json.RawMessage(`{"text":"Started agent.","mrkdwn":false}`), nil
}

func slackRouteFixture(t *testing.T, f fixture) (uuid.UUID, uuid.UUID) {
	t.Helper()
	installation, channel, registration, publication := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO slack_app_registrations(transport,id,org_id,app_id,client_id,credential_revision,credential_ciphertext,credential_nonce,created_by_user_id)
 SELECT 'http',$4,org_id,'app','client',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),$6 FROM environments WHERE id=$1;
 INSERT INTO slack_installations(id,app_registration_id,org_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id)
 SELECT $2,$4,org_id,'app','team','bot',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),ARRAY['app_mentions:read','channels:read','channels:history','groups:read','groups:history','chat:write','assistant:write'],$6 FROM environments WHERE id=$1;
 INSERT INTO slack_connections(id,environment_id,agent_id,slack_app_registration_id,slack_installation_id,created_by_user_id,org_id) VALUES($5,$1,$7,$4,$2,$6,(SELECT org_id FROM slack_app_registrations WHERE id=$4));
 INSERT INTO slack_channel_bindings(id,environment_id,connection_id,installation_id,org_id,team_id,slack_channel_id) SELECT $3,$1,$5,$2,org_id,'team','C1' FROM environments WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.env, installation, channel, registration, publication, f.user, f.agent)
	return installation, channel
}

func bindFixtureSlackRoot(t *testing.T, f fixture, channel uuid.UUID) {
	t.Helper()
	thread := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO slack_threads(id,environment_id,front_session_id,slack_channel_binding_id,org_id,team_id,slack_channel_id,thread_ts,recipient_team_id,recipient_user_id)
 SELECT $1,$2,$3,id,org_id,team_id,slack_channel_id,'123.456',team_id,'human' FROM slack_channel_bindings WHERE id=$4;
 INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id) VALUES($5,$1,$2,$3)`, pgx.QueryExecModeSimpleProtocol, thread, f.env, f.session, channel, uuid.NewV7())
}

func TestSlackStartCreatesOpeningAndPreservesReceiptAndKeyedRoute(t *testing.T) {
	f := newAdmissionFixture(t)
	slackRouteFixture(t, f)
	channel := "C1"
	key := "work"
	req := f.startRequest("start")
	req.SlackChannelID = &channel
	req.SessionKey = &key
	calls := 0
	preflight := startPreflightFunc(func(ctx context.Context, value slackroute.StartContext) (json.RawMessage, error) {
		calls++
		return acceptedStartPreflight(ctx, value)
	})
	first, err := slack.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, preflight, f.caller(), req)
	if err != nil {
		t.Fatal(err)
	}
	var exact bool
	if err = f.pool.QueryRow(t.Context(), `SELECT t.thread_ts IS NULL AND p.role='opening' AND p.turn_id=$2 AND p.session_id=t.front_session_id AND p.source_thread_id IS NULL AND p.post_key=t.opening_post_key FROM slack_threads t JOIN slack_posts p ON p.thread_id=t.id WHERE t.front_session_id=$1`, first.SessionID, first.TurnID).Scan(&exact); err != nil || !exact {
		t.Fatal(exact, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_connections SET revoked_at=clock_timestamp() WHERE environment_id=$1`, f.env)
	retry, err := slack.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, preflight, f.caller(), req)
	if err != nil || retry.TurnID != first.TurnID || calls != 1 {
		t.Fatal(retry, calls, err)
	}
	req.IdempotencyKey = "continue"
	req.SlackChannelID = nil
	if _, err := slack.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, preflight, f.caller(), req); !errors.Is(err, session.ErrConflict) {
		t.Fatalf("duplicate key: %v", err)
	}
	next, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), session.EnqueueRequest{EnvironmentID: f.env, SessionID: first.SessionID, IdempotencyKey: "send", Input: req.Input})
	if err != nil || next.Sequence != 2 || calls != 1 {
		t.Fatal(next, calls, err)
	}
	var count int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_posts WHERE session_id=$1 AND role='opening'`, first.SessionID).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	other := "COTHER"
	req.IdempotencyKey = "different-channel"
	req.SlackChannelID = &other
	if _, err = slack.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, preflight, f.caller(), req); !errors.Is(err, session.ErrConflict) {
		t.Fatal(err)
	}
	req.IdempotencyKey = "fresh"
	req.SessionKey = nil
	req.SlackChannelID = &channel
	if _, err = slack.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, preflight, f.caller(), req); !errors.Is(err, slackroute.ErrTargetNotPublished) {
		t.Fatal(err)
	}
}

func TestSlackStartPreflightRejectsWithoutAdmissionAndRechecksGeneration(t *testing.T) {
	for _, kind := range []string{"access", "changed"} {
		t.Run(kind, func(t *testing.T) {
			f := newAdmissionFixture(t)
			slackRouteFixture(t, f)
			var before int
			if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			channel := "C1"
			req := f.startRequest("new")
			req.SlackChannelID = &channel
			preflight := startPreflightFunc(func(ctx context.Context, value slackroute.StartContext) (json.RawMessage, error) {
				if kind == "access" {
					return nil, slackroute.ErrChannelUnavailable
				}
				// This independent write would block if preflight retained the SQL gates.
				if _, err := f.pool.Exec(ctx, `UPDATE slack_connections SET revoked_at=clock_timestamp() WHERE id=$1`, value.Route.Target.ConnectionID); err != nil {
					return nil, err
				}
				return acceptedStartPreflight(ctx, value)
			})
			_, err := slack.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, preflight, f.caller(), req)
			want := slackroute.ErrChannelUnavailable
			if kind == "changed" {
				want = slackroute.ErrConversationChanged
			}
			if !errors.Is(err, want) {
				t.Fatal(err)
			}
			var unchanged bool
			if err = f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM sessions)=$1 AND NOT EXISTS(SELECT 1 FROM slack_posts)`, before).Scan(&unchanged); err != nil || !unchanged {
				t.Fatal(unchanged, err)
			}
		})
	}
}

func TestSlackSpawnJoinsSourceButStartOwnsIndependentRoot(t *testing.T) {
	f := newAdmissionFixture(t)
	_, channel := slackRouteFixture(t, f)
	bindFixtureSlackRoot(t, f, channel)
	parent := f.enqueue(t, "parent")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	caller := session.Caller{Kind: "session", ID: f.session, TurnID: parent.TurnID, Execution: f.execution(), Host: f.host()}
	req := f.startRequest("child")
	req.ComputerID = f.computer

	child, err := session.Spawn(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, req)
	if err != nil {
		t.Fatal(err)
	}
	var exact bool
	if err = f.pool.QueryRow(t.Context(), `SELECT NOT EXISTS(SELECT 1 FROM slack_threads own WHERE own.front_session_id=s.id) AND s.parent_session_id=$2 AND src.thread_id=root.id FROM sessions s JOIN slack_thread_sources src ON src.session_id=s.id JOIN slack_threads root ON root.front_session_id=$2 WHERE s.id=$1`, child.SessionID, f.session).Scan(&exact); err != nil || !exact {
		t.Fatal(exact, err)
	}
	// The route authority is the thread row, and only a root Session may own
	// one: the schema rejects a thread fronted by the child.
	var violation *pgconn.PgError
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO slack_threads(id,environment_id,front_session_id,slack_channel_binding_id,org_id,team_id,slack_channel_id,thread_ts) SELECT $1,$2,$3,c.id,c.org_id,c.team_id,c.slack_channel_id,'999.000' FROM slack_channel_bindings c WHERE c.environment_id=$2`, uuid.NewV7(), f.env, child.SessionID); !errors.As(err, &violation) || violation.Code != "23503" {
		t.Fatalf("child owned a Slack route: %v", err)
	}
	req.IdempotencyKey = "independent"
	preflight := startPreflightFunc(func(ctx context.Context, value slackroute.StartContext) (json.RawMessage, error) {
		if value.Route.Source == nil || value.Route.Source.SessionID != f.session {
			t.Fatal("missing source provenance")
		}
		return acceptedStartPreflight(ctx, value)
	})
	independent, err := slack.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, preflight, caller, req)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT s.parent_session_id IS NULL AND s.root_session_id=s.id AND s.requester_session_id=$2 AND t.front_session_id=s.id AND p.source_thread_id=source.id FROM sessions s JOIN slack_threads t ON t.front_session_id=s.id JOIN slack_posts p ON p.thread_id=t.id AND p.role='opening' JOIN slack_threads source ON source.front_session_id=$2 WHERE s.id=$1`, independent.SessionID, f.session).Scan(&exact); err != nil || !exact {
		t.Fatal(exact, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_connections SET revoked_at=clock_timestamp() WHERE environment_id=$1`, f.env)
	retry, err := slack.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, preflight, caller, req)
	if err != nil || retry.TurnID != independent.TurnID {
		t.Fatal(retry, err)
	}
	req.IdempotencyKey = "unavailable"
	if _, err = slack.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, preflight, caller, req); !errors.Is(err, slack.ErrSourceConversationUnavailable) {
		t.Fatal(err)
	}
}

func TestSlackConnectionAdmissionRequiresEveryBotScope(t *testing.T) {
	f := newAdmissionFixture(t)
	installation, _ := slackRouteFixture(t, f)
	var publication uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM slack_connections WHERE environment_id=$1 AND agent_id=$2`, f.env, f.agent).Scan(&publication); err != nil {
		t.Fatal(err)
	}
	// Keep the expected permissions independent from the production definition.
	for _, missing := range []string{"", "app_mentions:read", "channels:read", "channels:history", "groups:read", "groups:history", "chat:write", "assistant:write"} {
		t.Run("missing/"+missing, func(t *testing.T) {
			tx, err := f.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if missing != "" {
				dbtest.MustExec(t, t.Context(), tx, `UPDATE slack_installations SET granted_scopes=array_remove(granted_scopes,$2) WHERE id=$1`, installation, missing)
			}
			allowed, err := slackroute.LockConnections(t.Context(), tx, f.env, []uuid.UUID{publication})
			if err != nil || allowed != (missing == "") {
				t.Fatalf("publication gate=%v err=%v", allowed, err)
			}
			_, err = slackroute.ReadConnection(t.Context(), tx, f.env, f.agent, "C1")
			if missing == "" && err != nil || missing != "" && !errors.Is(err, slackroute.ErrTargetNotPublished) {
				t.Fatalf("connection gate: %v", err)
			}
		})
	}
}
