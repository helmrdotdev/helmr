package slack

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

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
	root, err := Start(t.Context(), f.Pool, nil, preflight, session.Caller{Kind: "user", ID: f.User}, req)
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
