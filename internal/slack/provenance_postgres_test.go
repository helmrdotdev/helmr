package slack

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5/pgconn"
	"testing"
	"uuid"
)

func TestSlackProjectionUsesOriginalTransportSource(t *testing.T) {
	for _, change := range []string{"original", "console", "payload-expired", "removed-member", "deleted-link", "relinked"} {
		t.Run(change, func(t *testing.T) {
			f := newStatusFixture(t)
			link := f.linkActor(t, f.User, "human")
			f.outputWriter(t)
			var turn uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM turns WHERE session_id=$1`, f.Session).Scan(&turn); err != nil {
				t.Fatal(err)
			}
			if change != "console" {
				f.bindLinkedSource(t)
			}
			switch change {
			case "payload-expired":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET payload=NULL,payload_expired_at=clock_timestamp() WHERE turn_id=$1`, turn)
			case "removed-member":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET is_owner=false,removed_at=clock_timestamp() WHERE id=(SELECT member_id FROM slack_user_links WHERE id=$1)`, link)
			case "deleted-link":
				dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM slack_user_links WHERE id=$1`, link)
			case "relinked":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_user_links SET revoked_at=clock_timestamp() WHERE id=$1`, link)
				f.linkActor(t, f.User, "replacement-human")
			}
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			team, user, err := projectionRecipient(t.Context(), tx, projectionOwner{environment: f.Environment, session: f.Session, installation: f.installation}, &turn)
			if err != nil {
				t.Fatal(err)
			}
			want := change == "original" || change == "payload-expired" || change == "relinked"
			if want {
				if team == nil || *team != "team" || user == nil || *user != "human" {
					t.Fatal("lost original recipient", team, user)
				}
			} else if team != nil || user != nil {
				t.Fatal("expected recipient-less fallback", team, user)
			}
		})
	}
}

func TestSlackTurnAdmissionSourceConstraints(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	f.outputWriter(t)
	f.bindLinkedSource(t)
	_, err := f.Pool.Exec(t.Context(), `UPDATE slack_requests SET source_link_id=NULL WHERE session_id=$1`, f.Session)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23514" {
		t.Fatal("accepted input without source", err)
	}
	_, err = f.Pool.Exec(t.Context(), `INSERT INTO slack_requests(id,installation_id,request_key,request_digest,source_occurred_at,slack_user_id,user_id,thread_id,environment_id,session_id,thread_source_id,operation,payload_expired_at,status,expires_at,turn_id,finished_at,source_link_id)
 SELECT gen_random_uuid(),installation_id,'duplicate-admission',request_digest,source_occurred_at,slack_user_id,user_id,thread_id,environment_id,session_id,thread_source_id,operation,payload_expired_at,status,expires_at,turn_id,finished_at,source_link_id FROM slack_requests WHERE session_id=$1`, f.Session)
	if !errors.As(err, &pe) || pe.Code != "23505" {
		t.Fatal("multiple admitting sources", err)
	}
}
