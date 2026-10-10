package slack

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestSlackUserLinksNeverMergeAndPreserveHistoricalAdmissions(t *testing.T) {
	f, _, org := credentialFixture(t)
	first := UserIdentity{TeamID: "team", SlackUserID: "human"}
	if err := LinkUser(t.Context(), f.Pool, org, f.User, f.installation, first); err != nil {
		t.Fatal(err)
	}
	accepted := f.reply(t, "already-admitted", false)
	if err := admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), accepted); err != nil {
		t.Fatal(err)
	}
	links, err := ListUserLinks(t.Context(), f.Pool, org, f.User, "", 50)
	if err != nil || len(links) != 1 {
		t.Fatal(links, err)
	}
	if err = LinkUser(t.Context(), f.Pool, org, f.User, f.installation, first); err != nil {
		t.Fatal(err)
	}
	retried, err := ListUserLinks(t.Context(), f.Pool, org, f.User, "", 50)
	if err != nil || !retried[0].LinkedAt.Equal(links[0].LinkedAt) {
		t.Fatal("retry renewed authority", err)
	}
	if err = LinkUser(t.Context(), f.Pool, org, f.User, f.installation, UserIdentity{TeamID: "team", SlackUserID: "other"}); !errors.Is(err, ErrUserLinkConflict) {
		t.Fatal("identity replaced without unlink", err)
	}
	delayed := f.reply(t, "delayed-before-unlink", false)
	if err = UnlinkUser(t.Context(), f.Pool, org, f.User, "team", "human"); err != nil {
		t.Fatal(err)
	}
	if err = LinkUser(t.Context(), f.Pool, org, f.User, f.installation, first); err != nil {
		t.Fatal(err)
	}
	if err = admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), delayed); err != nil {
		t.Fatal(err)
	}
	var exact bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT a.status='accepted' AND a.user_id=$1 AND b.status='rejected' AND b.error='identity_unlinked' FROM slack_requests a,slack_requests b WHERE a.id=$2 AND b.id=$3`, f.User, accepted, delayed).Scan(&exact); err != nil || !exact {
		t.Fatal("relink replayed gesture or erased accepted identity", err)
	}
	if err = UnlinkUser(t.Context(), f.Pool, org, f.User, "team", "other"); err != nil {
		t.Fatal(err)
	}
	links, err = ListUserLinks(t.Context(), f.Pool, org, f.User, "", 50)
	if err != nil || len(links) != 1 {
		t.Fatal("stale unlink removed replacement", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET disabled_at=clock_timestamp() WHERE org_id=$1 AND user_id=$2`, org, f.User)
	if err = LinkUser(t.Context(), f.Pool, org, f.User, f.installation, first); !errors.Is(err, ErrUserLinkDenied) {
		t.Fatal("disabled member linked", err)
	}
}
func TestSlackUserLinksCompetingUsersCannotStealIdentity(t *testing.T) {
	f, _, org := credentialFixture(t)
	other := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO users(id,display_name) VALUES($1,'Other')`, other)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO org_members(org_id,user_id,role) VALUES($1,$2,'developer')`, org, other)
	ready := make(chan struct{})
	results := make(chan error, 2)
	for _, user := range []uuid.UUID{f.User, other} {
		go func() {
			<-ready
			results <- LinkUser(t.Context(), f.Pool, org, user, f.installation, UserIdentity{TeamID: "team", SlackUserID: "human"})
		}()
	}
	close(ready)
	a, b := <-results, <-results
	if (a == nil) == (b == nil) {
		t.Fatal(a, b)
	}
	if a != nil && !errors.Is(a, ErrUserLinkConflict) || b != nil && !errors.Is(b, ErrUserLinkConflict) {
		t.Fatal(a, b)
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_user_links WHERE team_id='team' AND slack_user_id='human'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}
