package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
)

type browserAuthQuerier struct {
	db.Querier
	sessionHash    []byte
	invitationHash []byte
}

func (q *browserAuthQuerier) CreateAuthSession(_ context.Context, arg db.CreateAuthSessionParams) (db.AuthSession, error) {
	q.sessionHash = append([]byte(nil), arg.TokenHash...)
	return db.AuthSession{}, nil
}

func (q *browserAuthQuerier) GetActiveInvitation(_ context.Context, tokenHash []byte) (db.GetActiveInvitationRow, error) {
	q.invitationHash = append([]byte(nil), tokenHash...)
	return db.GetActiveInvitationRow{}, nil
}

func TestBrowserAuthUsesSessionDomainForIssuedSession(t *testing.T) {
	keys, err := auth.NewKeys(bytes.Repeat([]byte{1}, auth.RootKeySize))
	if err != nil {
		t.Fatal(err)
	}
	queries := &browserAuthQuerier{}
	server := &Server{authKeys: keys}

	raw, err := server.issueSessionForOrg(
		httptest.NewRequest("POST", "/", nil),
		queries,
		pgtype.UUID{Bytes: uuid.NewV7(), Valid: true},
		pgtype.UUID{},
	)
	if err != nil {
		t.Fatal(err)
	}
	want, err := auth.HashToken(keys.Session, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(queries.sessionHash, want) {
		t.Fatal("issued session was not hashed with the session key")
	}
}

func TestBrowserAuthUsesInvitationDomainForInvitationValidation(t *testing.T) {
	keys, err := auth.NewKeys(bytes.Repeat([]byte{1}, auth.RootKeySize))
	if err != nil {
		t.Fatal(err)
	}
	queries := &browserAuthQuerier{}
	publicURL, err := url.Parse("https://helmr.example.test")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		db:        queries,
		authKeys:  keys,
		publicURL: publicURL,
	}

	raw := "invite-token"
	got, err := server.validateInvitationToken(
		httptest.NewRequest("POST", "/", nil),
		raw,
	)
	if err != nil {
		t.Fatal(err)
	}
	want, err := auth.HashToken(keys.Invitation, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) || !bytes.Equal(queries.invitationHash, want) {
		t.Fatal("invitation was not validated with the invitation key")
	}
}

type continuationAuthProvider struct{}

func (continuationAuthProvider) RedirectURL(state, verifier string) string {
	return "https://github.example.test/authorize?state=" + url.QueryEscape(state)
}
func (continuationAuthProvider) Resolve(context.Context, string, string) (authIdentity, error) {
	return authIdentity{Provider: "github", Subject: "continuation", DisplayName: "Fixture user", Email: "fixture@example.test", EmailVerified: true}, nil
}

func TestBrowserAuthSupersededCallbackKeepsNewerFlow(t *testing.T) {
	keys, err := auth.NewKeys(bytes.Repeat([]byte{1}, auth.RootKeySize))
	if err != nil {
		t.Fatal(err)
	}
	publicURL, _ := url.Parse("https://helmr.example.test")
	s := &Server{db: &browserAuthQuerier{}, authKeys: keys, publicURL: publicURL, authProvider: continuationAuthProvider{}}
	flow := browserAuthFlow{Kind: browserAuthGitHubLogin, State: "newer-state", Verifier: "verifier", RedirectAfter: "/auth/device?code=NEW"}
	encoded, err := s.encodeAuthFlow(flow)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"older-state", "", "newer-state"} {
		for _, denied := range []bool{false, true} {
			body := map[string]string{"state": state}
			if denied {
				body["error"] = "access_denied"
			}
			raw, _ := json.Marshal(body)
			r := httptest.NewRequest("POST", "https://helmr.example.test/api/auth/github/finish", bytes.NewReader(raw))
			r.AddCookie(authFlowCookie(r, encoded, 600))
			w := httptest.NewRecorder()
			s.githubFinish(w, r)
			if w.Code != 400 {
				t.Fatalf("state=%q denied=%v status=%d", state, denied, w.Code)
			}
			cleared := false
			for _, cookie := range w.Result().Cookies() {
				if cookie.Name == authFlowCookieName(r) && cookie.MaxAge < 0 {
					cleared = true
				}
			}
			if cleared != (state == flow.State) {
				t.Fatalf("state=%q denied=%v cleared=%v", state, denied, cleared)
			}
		}
	}
}

func TestBrowserAuthReturnDestinationValidation(t *testing.T) {
	for _, value := range []string{"https://evil.test", "//evil.test", "/\\evil.test", "/bad\npath", "/bad\x00path"} {
		if got := validateRedirectAfter(value); got != "/" {
			t.Fatalf("accepted %q: %q", value, got)
		}
	}
	const destination = "/auth/device?code=ABCD-EFGH#confirm"
	if got := validateRedirectAfter(destination); got != destination {
		t.Fatalf("lost destination %q", got)
	}
}
