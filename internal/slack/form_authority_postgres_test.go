package slack

import (
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func TestSlackFormReadRequiresCurrentExactLinkedAuthority(t *testing.T) {
	for _, scenario := range []string{"viewer", "unlinked", "disabled_member", "disabled_user", "revoked_publication", "retired_app", "disconnected", "reauthorized", "reenabled", "wrong_app", "wrong_team", "bot", "wrong_session", "wrong_turn", "wrong_ask", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			f := newStatusFixture(t)
			f.link(t)
			c := f.control("open_answer")
			c.Target.Turn, c.Target.Ask = f.pendingQuestion(t)
			source := time.Now().UTC()
			app, team, actor := "app", "team", "human"
			switch scenario {
			case "viewer":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET role='viewer' WHERE user_id=$1`, f.User)
			case "unlinked":
				dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM slack_user_links`)
			case "disabled_member":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET disabled_at=clock_timestamp() WHERE user_id=$1`, f.User)
			case "disabled_user":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE users SET disabled_at=clock_timestamp() WHERE id=$1`, f.User)
			case "revoked_publication":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE agent_publications SET revoked_at=clock_timestamp() WHERE id=$1`, f.publication)
			case "retired_app":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_app_registrations SET retired_at=clock_timestamp(),credential_ciphertext=NULL,credential_nonce=NULL WHERE id=$1`, f.registration)
			case "disconnected":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET disconnected_at=clock_timestamp(),credential_ciphertext=NULL,credential_nonce=NULL WHERE id=$1`, f.installation)
			case "reauthorized":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET authorized_at=$2 WHERE id=$1`, f.installation, source.Add(time.Second))
			case "reenabled":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE agent_publications SET created_at=$2 WHERE id=$1`, f.publication, source.Add(time.Second))
			case "wrong_app":
				app = "other"
			case "wrong_team":
				team = "other"
			case "bot":
				actor = "bot"
			case "wrong_session":
				c.Target.Session = uuid.NewV7()
			case "wrong_turn":
				c.Target.Turn = uuid.NewV7()
			case "wrong_ask":
				c.Target.Ask = uuid.NewV7()
			case "expired":
				c.ExpiresAt = time.Now().Add(-time.Second).Unix()
			}
			err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
				view, credential, err := readFormQuestion(t.Context(), tx, app, team, actor, c, source)
				if scenario == "viewer" && err == nil && (view.ID != c.Target.Ask || view.Status != "pending" || len(view.Question) == 0 || credential != 1) {
					t.Fatalf("incomplete exact ask: %+v %d", view, credential)
				}
				if scenario != "viewer" && (len(view.Question) != 0 || credential != 0) {
					t.Fatal("unauthorized read disclosed question or credential reference")
				}
				return err
			})
			if (err == nil) != (scenario == "viewer") {
				t.Fatalf("authorization mismatch: %v", err)
			}
		})
	}
}
