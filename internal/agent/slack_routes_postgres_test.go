package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

type startPreflightFunc func(context.Context, SlackStartContext) (json.RawMessage, error)

func (f startPreflightFunc) PrepareStart(ctx context.Context, value SlackStartContext) (json.RawMessage, error) {
	return f(ctx, value)
}
func acceptedStartPreflight(context.Context, SlackStartContext) (json.RawMessage, error) {
	return json.RawMessage(`{"text":"Started agent.","mrkdwn":false}`), nil
}

func slackRouteFixture(t *testing.T, f fixture) (uuid.UUID, uuid.UUID) {
	t.Helper()
	installation, channel, registration, publication := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO slack_app_registrations(id,organization_id,app_id,client_id,credential_revision,credential_ciphertext,credential_nonce,created_by_user_id)
 SELECT $4,org_id,'app','client',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),$6 FROM environments WHERE id=$1;
 INSERT INTO slack_installations(id,app_registration_id,organization_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id)
 SELECT $2,$4,org_id,'app','team','bot',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),ARRAY['app_mentions:read','channels:read','channels:history','groups:read','groups:history','chat:write','assistant:write'],$6 FROM environments WHERE id=$1;
 INSERT INTO agent_publications(id,environment_id,agent_id,slack_app_registration_id,slack_installation_id,created_by_user_id) VALUES($5,$1,$7,$4,$2,$6);
 INSERT INTO slack_channels(id,environment_id,publication_id,installation_id,organization_id,team_id,slack_channel_id) SELECT $3,$1,$5,$2,org_id,'team','C1' FROM environments WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.env, installation, channel, registration, publication, f.user, f.agent)
	return installation, channel
}

func bindFixtureSlackRoot(t *testing.T, f fixture, channel uuid.UUID) {
	t.Helper()
	err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `UPDATE sessions SET slack_channel_id=$3 WHERE environment_id=$1 AND id=$2`, f.env, f.session, channel); err != nil {
			return err
		}
		connection, err := ReadSlackConnection(t.Context(), tx, f.env, f.agent, "C1")
		if err != nil {
			return err
		}
		return createSlackRoot(t.Context(), tx, f.env, f.session, uuid.Nil(), channel, SlackStartRoute{Target: connection, HumanThreadTS: "123.456", HumanActor: "human"})
	})
	if err != nil {
		t.Fatal(err)
	}
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
	req.SlackPreflight = startPreflightFunc(func(ctx context.Context, value SlackStartContext) (json.RawMessage, error) {
		calls++
		return acceptedStartPreflight(ctx, value)
	})
	first, err := Start(t.Context(), f.pool, nil, f.caller(), req)
	if err != nil {
		t.Fatal(err)
	}
	var exact bool
	if err = f.pool.QueryRow(t.Context(), `SELECT t.thread_ts IS NULL AND p.role='opening' AND p.turn_id=$2 AND p.session_id=t.front_session_id AND p.source_thread_id IS NULL AND p.publication_key=t.opening_publication_key FROM slack_threads t JOIN slack_posts p ON p.thread_id=t.id WHERE t.front_session_id=$1`, first.SessionID, first.TurnID).Scan(&exact); err != nil || !exact {
		t.Fatal(exact, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE agent_publications SET revoked_at=clock_timestamp() WHERE environment_id=$1`, f.env)
	retry, err := Start(t.Context(), f.pool, nil, f.caller(), req)
	if err != nil || retry.TurnID != first.TurnID || calls != 1 {
		t.Fatal(retry, calls, err)
	}
	req.RetryKey = "continue"
	req.SlackChannelID = nil
	next, err := Start(t.Context(), f.pool, nil, f.caller(), req)
	if err != nil || next.SessionID != first.SessionID || next.Sequence != 2 || calls != 1 {
		t.Fatal(next, calls, err)
	}
	var count int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_posts WHERE session_id=$1 AND role='opening'`, first.SessionID).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	other := "COTHER"
	req.RetryKey = "different-channel"
	req.SlackChannelID = &other
	if _, err = Start(t.Context(), f.pool, nil, f.caller(), req); !errors.Is(err, ErrSlackDestinationConflict) {
		t.Fatal(err)
	}
	req.RetryKey = "fresh"
	req.SessionKey = nil
	req.SlackChannelID = &channel
	if _, err = Start(t.Context(), f.pool, nil, f.caller(), req); !errors.Is(err, ErrTargetNotPublished) {
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
			req.SlackPreflight = startPreflightFunc(func(ctx context.Context, value SlackStartContext) (json.RawMessage, error) {
				if kind == "access" {
					return nil, ErrSlackChannelUnavailable
				}
				// This independent write would block if preflight retained the SQL gates.
				if _, err := f.pool.Exec(ctx, `UPDATE agent_publications SET revoked_at=clock_timestamp() WHERE id=$1`, value.Route.Target.PublicationID); err != nil {
					return nil, err
				}
				return acceptedStartPreflight(ctx, value)
			})
			_, err := Start(t.Context(), f.pool, nil, f.caller(), req)
			want := ErrSlackChannelUnavailable
			if kind == "changed" {
				want = ErrConversationChanged
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
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	caller := Caller{Kind: "session", ID: f.session, TurnID: parent.TurnID, Execution: f.execution(), Host: f.host()}
	req := f.startRequest("child")
	req.ComputerID = f.computer
	req.SlackPreflight = startPreflightFunc(func(context.Context, SlackStartContext) (json.RawMessage, error) {
		t.Fatal("spawn performed Slack preflight")
		return nil, nil
	})
	child, err := Spawn(t.Context(), f.pool, nil, caller, req)
	if err != nil {
		t.Fatal(err)
	}
	var exact bool
	if err = f.pool.QueryRow(t.Context(), `SELECT s.slack_channel_id IS NULL AND s.parent_session_id=$2 AND src.thread_id=root.id FROM sessions s JOIN slack_thread_sources src ON src.session_id=s.id JOIN slack_threads root ON root.front_session_id=$2 WHERE s.id=$1`, child.SessionID, f.session).Scan(&exact); err != nil || !exact {
		t.Fatal(exact, err)
	}
	req.RetryKey = "independent"
	req.SlackPreflight = startPreflightFunc(func(ctx context.Context, value SlackStartContext) (json.RawMessage, error) {
		if value.Route.Source == nil || value.Route.Source.SessionID != f.session {
			t.Fatal("missing source provenance")
		}
		return acceptedStartPreflight(ctx, value)
	})
	independent, err := Start(t.Context(), f.pool, nil, caller, req)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT s.parent_session_id IS NULL AND s.root_session_id=s.id AND s.requester_session_id=$2 AND t.front_session_id=s.id AND p.source_thread_id=source.id FROM sessions s JOIN slack_threads t ON t.front_session_id=s.id JOIN slack_posts p ON p.thread_id=t.id AND p.role='opening' JOIN slack_threads source ON source.front_session_id=$2 WHERE s.id=$1`, independent.SessionID, f.session).Scan(&exact); err != nil || !exact {
		t.Fatal(exact, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE agent_publications SET revoked_at=clock_timestamp() WHERE environment_id=$1`, f.env)
	retry, err := Start(t.Context(), f.pool, nil, caller, req)
	if err != nil || retry.TurnID != independent.TurnID {
		t.Fatal(retry, err)
	}
	req.RetryKey = "unavailable"
	if _, err = Start(t.Context(), f.pool, nil, caller, req); !errors.Is(err, ErrSourceConversationUnavailable) {
		t.Fatal(err)
	}
}

func TestSlackStartSourceRetainsPositivelyBoundSuppressedOpening(t *testing.T) {
	f := newAdmissionFixture(t)
	slackRouteFixture(t, f)
	channel := "C1"
	req := f.startRequest("root")
	req.SlackChannelID = &channel
	req.SlackPreflight = startPreflightFunc(acceptedStartPreflight)
	root, err := Start(t.Context(), f.pool, nil, f.caller(), req)
	if err != nil {
		t.Fatal(err)
	}
	read := func() (*SlackStartSource, error) {
		var source *SlackStartSource
		err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
			var err error
			source, err = readSlackStartSource(t.Context(), tx, f.env, root.SessionID)
			return err
		})
		return source, err
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_posts SET status='suppressed',suppressed_revision=desired_revision,error='publication_authority_unavailable' WHERE session_id=$1`, root.SessionID)
	if _, err = read(); !errors.Is(err, ErrSourceConversationUnavailable) {
		t.Fatal("unconfirmed unavailable root accepted", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_threads SET thread_ts='123.789' WHERE front_session_id=$1; UPDATE slack_posts SET message_ts='123.789',confirmed_revision=desired_revision,posted_at=clock_timestamp() WHERE session_id=$1`, pgx.QueryExecModeSimpleProtocol, root.SessionID)
	if source, err := read(); err != nil || source == nil || source.ThreadTS != "123.789" {
		t.Fatal("positive root discarded", source, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_posts SET delivery_disposed_at=clock_timestamp(),delivery_disposed_by=$2,closed_at=clock_timestamp() WHERE session_id=$1`, root.SessionID, f.user)
	if _, err = read(); !errors.Is(err, ErrSourceConversationUnavailable) {
		t.Fatal("explicit abandonment revived", err)
	}
}

func TestSlackPublicationAdmissionRequiresEveryBotScope(t *testing.T) {
	f := newAdmissionFixture(t)
	installation, _ := slackRouteFixture(t, f)
	var publication uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM agent_publications WHERE environment_id=$1 AND agent_id=$2`, f.env, f.agent).Scan(&publication); err != nil {
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
			allowed, err := LockSlackPublications(t.Context(), tx, f.env, []uuid.UUID{publication})
			if err != nil || allowed != (missing == "") {
				t.Fatalf("publication gate=%v err=%v", allowed, err)
			}
			_, err = ReadSlackConnection(t.Context(), tx, f.env, f.agent, "C1")
			if missing == "" && err != nil || missing != "" && !errors.Is(err, ErrTargetNotPublished) {
				t.Fatalf("connection gate: %v", err)
			}
		})
	}
}
