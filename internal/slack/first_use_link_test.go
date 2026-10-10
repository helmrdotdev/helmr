package slack

import (
	"bytes"
	"errors"
	"net/url"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestFirstUseLinkSignatureAndLoginContinuationBounds(t *testing.T) {
	config := testProjectionConfig()
	id := uuid.NewV7()
	link, err := firstUseURL(config, id, strings.Repeat("C", 100))
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.RequestURI()) > 256 {
		t.Fatalf("link cannot survive login continuation: %d", len(u.RequestURI()))
	}
	token := u.Query().Get("link")
	got, channel, err := decodeFirstUseLink(config.ControlKey, token)
	if err != nil || got != id || channel != strings.Repeat("C", 100) {
		t.Fatal(got, channel, err)
	}
	for _, value := range []string{"", token + "x", "x" + token, strings.Repeat("x", 241)} {
		if _, _, err := decodeFirstUseLink(config.ControlKey, value); !errors.Is(err, ErrFirstUseLink) {
			t.Fatal("accepted modified link", err)
		}
	}
	if _, _, err := decodeFirstUseLink(bytes.Repeat([]byte{9}, 32), token); !errors.Is(err, ErrFirstUseLink) {
		t.Fatal("accepted foreign issuer", err)
	}
}

func TestFirstUseLinkRetainsContextButNeverAdmitsRejectedWork(t *testing.T) {
	f := newStatusFixture(t)
	request := f.reply(t, "first-use", false)
	config := testProjectionConfig()
	if err := ReconcileRequest(t.Context(), f.Pool, nil, config, nil, request); err != nil {
		t.Fatal(err)
	}
	// Feedback expires the message body, but its receipt remains the identity owner.
	if _, found, err := takeRejectedFeedback(t.Context(), f.Pool, config); err != nil || !found {
		t.Fatal(found, err)
	}
	u, err := firstUseURL(config, request, "C1")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(u)
	token := parsed.Query().Get("link")
	origin, err := ResolveFirstUseLink(t.Context(), f.Pool, config.ControlKey, token, f.User)
	if err != nil || origin.InstallationID != f.installation || origin.SlackUserID != "human" || origin.Linked || origin.ReturnURL != "https://slack.com/app_redirect?channel=C1&team=team" {
		t.Fatalf("context: %+v %v", origin, err)
	}
	if _, err := ResolveFirstUseLink(t.Context(), f.Pool, config.ControlKey, token, uuid.NewV7()); !errors.Is(err, ErrUserLinkDenied) {
		t.Fatal("nonmember accepted", err)
	}
	if err := LinkUser(t.Context(), f.Pool, origin.OrganizationID, f.User, f.installation, UserIdentity{TeamID: "team", SlackUserID: "human"}); err != nil {
		t.Fatal(err)
	}
	origin, err = ResolveFirstUseLink(t.Context(), f.Pool, config.ControlKey, token, f.User)
	if err != nil || !origin.Linked {
		t.Fatal("existing link missing", err)
	}
	var unchanged bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='rejected' AND user_id IS NULL AND turn_id IS NULL AND payload IS NULL AND (SELECT count(*)=0 FROM turns) FROM slack_requests WHERE id=$1`, request).Scan(&unchanged); err != nil || !unchanged {
		t.Fatal("linking changed rejected work", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET finished_at=clock_timestamp()-interval '11 minutes' WHERE id=$1`, request)
	if _, err := ResolveFirstUseLink(t.Context(), f.Pool, config.ControlKey, token, f.User); !errors.Is(err, ErrFirstUseLink) {
		t.Fatal("expired request accepted", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET finished_at=clock_timestamp() WHERE id=$1`, request)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET authorization_lost_at=clock_timestamp() WHERE id=$1`, f.installation)
	if _, err := ResolveFirstUseLink(t.Context(), f.Pool, config.ControlKey, token, f.User); !errors.Is(err, ErrFirstUseLink) {
		t.Fatal("revoked connection accepted", err)
	}
}
