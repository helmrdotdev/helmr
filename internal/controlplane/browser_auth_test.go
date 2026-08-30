package controlplane

import (
	"bytes"
	"context"
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
