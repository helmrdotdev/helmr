package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/email"
	"github.com/helmrdotdev/helmr/internal/org"
)

// batchInvitationSender records each batch of emails, or fails every batch
// when err is set; onSend runs before it returns. It sends no single emails.
type batchInvitationSender struct {
	email.Unconfigured
	mu      sync.Mutex
	batches [][]email.Message
	err     error
	onSend  func(context.Context)
}

func (s *batchInvitationSender) SendEmails(ctx context.Context, messages []email.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, messages)
	if s.onSend != nil {
		s.onSend(ctx)
	}
	return s.err
}

func newInvitationHTTPFixture(t *testing.T, sender email.Sender) httpPostgresFixture {
	t.Helper()
	return newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
		if sender != nil {
			cfg.Mailer = sender
			cfg.EmailCodeDelivery = NewEmailCodeDelivery(discardTestLogger(), time.Second)
		}
	})
}

func createInvitationsRequest(t *testing.T, fixture httpPostgresFixture, token string, request api.CreateInvitationsRequest) (int, api.CreateInvitationsResponse, string) {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	recorder := fixture.request(t, http.MethodPost, "/api/invitations", token, string(body))
	var response api.CreateInvitationsResponse
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
	}
	return recorder.Code, response, recorder.Body.String()
}

// memberWithEmail adds an active member with a primary email to the
// organization.
func (f httpPostgresFixture) memberWithEmail(t *testing.T, orgID uuid.UUID, address string, role string) {
	t.Helper()
	userID := uuid.NewV7()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO users (id, display_name, primary_email) VALUES ($1, $2, $3)`, userID, "Member", address); err != nil {
		t.Fatal(err)
	}
	f.member(t, orgID, userID, role)
}

func emailDelivery(t *testing.T, fixture httpPostgresFixture, token string) bool {
	t.Helper()
	recorder := fixture.request(t, http.MethodGet, "/api/auth/config", token, "")
	var me api.AuthConfigResponse
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &me) != nil {
		t.Fatalf("me = %d %s", recorder.Code, recorder.Body.String())
	}
	return me.EmailDelivery
}

func countInvitations(t *testing.T, fixture httpPostgresFixture, addresses ...string) int {
	t.Helper()
	var count int
	if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM invitations WHERE invitee_email = ANY($1)`, addresses).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestCreateInvitationsHTTPPostgresRequiresEmailDelivery(t *testing.T) {
	fixture := newInvitationHTTPFixture(t, nil)
	orgID, token := fixture.organizationOwner(t, "acme")
	developer := fixture.user(t, "Developer")
	fixture.member(t, orgID, developer, "member")
	if code, _, body := createInvitationsRequest(t, fixture, fixture.session(t, developer, orgID), api.CreateInvitationsRequest{Emails: []string{"dev@example.test"}}); code != http.StatusForbidden {
		t.Fatalf("developer without email delivery = %d %s, want authorization first", code, body)
	}
	if emailDelivery(t, fixture, token) {
		t.Fatal("me reports email delivery without a mailer")
	}
	code, _, body := createInvitationsRequest(t, fixture, token, api.CreateInvitationsRequest{Emails: []string{"new@example.test"}})
	if code != http.StatusServiceUnavailable || !strings.Contains(body, `"service_unavailable"`) || !strings.Contains(body, "email delivery is not configured") {
		t.Fatalf("create invitations without email = %d %s", code, body)
	}
	if count := countInvitations(t, fixture, "new@example.test"); count != 0 {
		t.Fatalf("invitations = %d", count)
	}
}

func TestCreateInvitationsHTTPPostgresEmailsOneBatch(t *testing.T) {
	sender := &batchInvitationSender{}
	fixture := newInvitationHTTPFixture(t, sender)
	orgID, token := fixture.organizationOwner(t, "acme")
	if !emailDelivery(t, fixture, token) {
		t.Fatal("me does not report the configured mailer")
	}
	fixture.memberWithEmail(t, orgID, "member@example.test", "member")
	if code, _, body := createInvitationsRequest(t, fixture, token, api.CreateInvitationsRequest{Emails: []string{"pending@example.test"}}); code != http.StatusOK {
		t.Fatalf("seed invitation = %d %s", code, body)
	}
	sender.batches = nil

	code, response, body := createInvitationsRequest(t, fixture, token, api.CreateInvitationsRequest{
		Emails: []string{"New@Example.test", "not-an-address", "member@example.test", "new@example.test", "pending@example.test", "second@example.test"},
	})
	if code != http.StatusOK {
		t.Fatalf("create invitations = %d %s", code, body)
	}
	if strings.Contains(body, "invite_url") || strings.Contains(body, "token") {
		t.Fatalf("response exposes an invitation link: %s", body)
	}
	want := []struct{ email, code string }{
		{"new@example.test", ""},
		{"not-an-address", "invalid_email"},
		{"member@example.test", "already_member"},
		{"pending@example.test", "invitation_pending"},
		{"second@example.test", ""},
	}
	if len(response.Results) != len(want) {
		t.Fatalf("results = %+v", response.Results)
	}
	for i, expected := range want {
		result := response.Results[i]
		if result.Email != expected.email {
			t.Fatalf("result %d email = %q, want %q", i, result.Email, expected.email)
		}
		if expected.code == "" {
			if result.Status != api.InvitationResultInvited || result.Error != nil {
				t.Fatalf("%s result = %+v", expected.email, result)
			}
			continue
		}
		if result.Status != api.InvitationResultFailed || result.Error == nil || result.Error.Code != expected.code || result.Error.Message == "" {
			t.Fatalf("%s result = %+v", expected.email, result)
		}
	}

	if len(sender.batches) != 1 || len(sender.batches[0]) != 2 {
		t.Fatalf("batches = %+v, want one batch of two emails", sender.batches)
	}
	var facts int
	if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='invitation.created' AND org_id=$1`, orgID).Scan(&facts); err != nil || facts != 3 {
		t.Fatalf("audit facts = %d, err = %v", facts, err)
	}
	for i, address := range []string{"new@example.test", "second@example.test"} {
		message := sender.batches[0][i]
		if message.To != address || message.Subject != "Owner invited you to acme on Helmr" || message.Link == nil || message.Link.Purpose != "invitation" || message.Link.Email != address {
			t.Fatalf("message %d = %+v", i, message)
		}
		parsed, err := url.Parse(message.Link.URL)
		if err != nil || parsed.Host != "console.example.test" || parsed.Path != "/invite" {
			t.Fatalf("invite URL = %q, err = %v", message.Link.URL, err)
		}
		tokenHash, err := auth.HashToken(fixture.keys.Invitation, parsed.Query().Get("token"))
		if err != nil {
			t.Fatal(err)
		}
		invitation, err := org.PendingInvitationByTokenHash(t.Context(), fixture.queries, tokenHash)
		if err != nil || invitation.InviteeEmail != address {
			t.Fatalf("emailed link resolves to %+v, err = %v", invitation, err)
		}
		expires := message.Link.ExpiresAt.Format("January 2, 2006 15:04 MST")
		wantBody := "Owner invited you to join the acme organization on Helmr.\n\n" +
			"Open this link to accept the invitation:\n\n" + message.Link.URL + "\n\n" +
			"This invitation expires at " + expires + ". If you did not expect it, you can ignore this email.\n"
		if message.PlainText != wantBody {
			t.Fatalf("body = %q, want %q", message.PlainText, wantBody)
		}
		if days := time.Until(message.Link.ExpiresAt).Hours() / 24; days < 6.9 || days > 7 {
			t.Fatalf("expiry in %.2f days, want the 7-day default", days)
		}
	}

	t.Run("a full request is one batch", func(t *testing.T) {
		sender.batches = nil
		addresses := make([]string, org.MaxInvitationsPerRequest)
		for i := range addresses {
			addresses[i] = fmt.Sprintf("bulk-%d@example.test", i)
		}
		code, response, body := createInvitationsRequest(t, fixture, token, api.CreateInvitationsRequest{Emails: addresses})
		if code != http.StatusOK || len(response.Results) != len(addresses) {
			t.Fatalf("create invitations = %d %s", code, body)
		}
		if len(sender.batches) != 1 || len(sender.batches[0]) != len(addresses) {
			t.Fatalf("batches = %d, want one batch of %d", len(sender.batches), len(addresses))
		}
	})
}

func TestCreateInvitationsHTTPPostgresRollsBackAFailedBatch(t *testing.T) {
	sender := &batchInvitationSender{err: errors.New("provider unavailable")}
	fixture := newInvitationHTTPFixture(t, sender)
	orgID, token := fixture.organizationOwner(t, "acme")
	fixture.memberWithEmail(t, orgID, "member@example.test", "member")

	code, response, body := createInvitationsRequest(t, fixture, token, api.CreateInvitationsRequest{
		Emails: []string{"first@example.test", "member@example.test", "second@example.test"},
	})
	if code != http.StatusOK || len(response.Results) != 3 {
		t.Fatalf("create invitations = %d %s", code, body)
	}
	for _, i := range []int{0, 2} {
		result := response.Results[i]
		if result.Status != api.InvitationResultFailed || result.Error == nil || result.Error.Code != "email_failed" || result.Error.Message != "Invitation emails could not be sent" {
			t.Fatalf("%s result = %+v", result.Email, result)
		}
	}
	if result := response.Results[1]; result.Error == nil || result.Error.Code != "already_member" {
		t.Fatalf("refused result = %+v", result)
	}
	if len(sender.batches) != 1 || len(sender.batches[0]) != 2 {
		t.Fatalf("batches = %+v", sender.batches)
	}
	if count := countInvitations(t, fixture, "first@example.test", "second@example.test"); count != 0 {
		t.Fatalf("invitations after failed batch = %d", count)
	}
	var facts int
	if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='invitation.created' AND org_id=$1`, orgID).Scan(&facts); err != nil || facts != 0 {
		t.Fatalf("audit facts after failed batch = %d, err = %v", facts, err)
	}
}

func TestCreateInvitationsHTTPPostgresRejectsTheRequestAsAWhole(t *testing.T) {
	sender := &batchInvitationSender{}
	fixture := newInvitationHTTPFixture(t, sender)
	orgID, token := fixture.organizationOwner(t, "acme")
	tooMany := make([]string, org.MaxInvitationsPerRequest+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("bulk-%d@example.test", i)
	}
	for name, request := range map[string]api.CreateInvitationsRequest{
		"over limit":   {Emails: tooMany},
		"no addresses": {Emails: []string{}},
	} {
		if code, _, body := createInvitationsRequest(t, fixture, token, request); code != http.StatusBadRequest || !strings.Contains(body, `"bad_request"`) {
			t.Fatalf("%s = %d %s", name, code, body)
		}
	}

	developer := fixture.user(t, "Developer")
	fixture.member(t, orgID, developer, "member")
	if code, _, body := createInvitationsRequest(t, fixture, fixture.session(t, developer, orgID), api.CreateInvitationsRequest{Emails: []string{"dev@example.test"}}); code != http.StatusForbidden {
		t.Fatalf("developer = %d %s", code, body)
	}
	if count := countInvitations(t, fixture, append(tooMany, "role@example.test", "dev@example.test", "owner@example.test", "old@example.test")...); count != 0 || len(sender.batches) != 0 {
		t.Fatalf("rejected requests created %d invitations and sent %d batches", count, len(sender.batches))
	}
}

func TestCreateInvitationsHTTPPostgresNamesTheRefusedRecipient(t *testing.T) {
	sender := &batchInvitationSender{err: email.RecipientError{Address: "grace@example.test", Err: errors.New("550 mailbox unavailable")}}
	fixture := newInvitationHTTPFixture(t, sender)
	_, token := fixture.organizationOwner(t, "acme")
	code, response, body := createInvitationsRequest(t, fixture, token, api.CreateInvitationsRequest{Emails: []string{"ada@example.test", "grace@example.test", "lin@example.test"}})
	if code != http.StatusOK || len(response.Results) != 3 {
		t.Fatalf("create invitations = %d %s", code, body)
	}
	for i, want := range []string{"email_failed", "email_rejected", "email_failed"} {
		if result := response.Results[i]; result.Status != api.InvitationResultFailed || result.Error == nil || result.Error.Code != want {
			t.Fatalf("%s result = %+v, want %s", result.Email, result, want)
		}
	}
	if count := countInvitations(t, fixture, "ada@example.test", "grace@example.test", "lin@example.test"); count != 0 {
		t.Fatalf("invitations = %d", count)
	}
}

func TestCreateInvitationsHTTPPostgresCommitsWhenTheCallerLeavesAfterTheSend(t *testing.T) {
	requestCtx, leave := context.WithCancel(t.Context())
	defer leave()
	var sendCtxErr error
	var deadline time.Time
	sender := &batchInvitationSender{onSend: func(ctx context.Context) {
		leave()
		sendCtxErr = ctx.Err()
		deadline, _ = ctx.Deadline()
	}}
	fixture := newInvitationHTTPFixture(t, sender)
	_, token := fixture.organizationOwner(t, "acme")
	request := httptest.NewRequestWithContext(requestCtx, http.MethodPost, "/api/invitations", strings.NewReader(`{"emails":["ada@example.test"]}`))
	fixture.authenticateRequest(request, token)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"invited"`) {
		t.Fatalf("create invitations = %d %s", recorder.Code, recorder.Body.String())
	}
	if sendCtxErr != nil || time.Until(deadline) > invitationRequestTimeout || time.Until(deadline) < invitationRequestTimeout-10*time.Second {
		t.Fatalf("send context err = %v, deadline in %s", sendCtxErr, time.Until(deadline))
	}
	if count := countInvitations(t, fixture, "ada@example.test"); count != 1 {
		t.Fatalf("invitations = %d, want the sent invitation committed", count)
	}
}
