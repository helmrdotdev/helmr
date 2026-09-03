package db_test

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestAuthIdentityRepeatLoginAndMagicLinkShareUser(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	queries := db.New(database.Pool)

	userID := uuid.NewV7()
	first, err := queries.UpsertAuthIdentity(t.Context(), db.UpsertAuthIdentityParams{
		UserID:           pgvalue.UUID(userID),
		DisplayName:      "first name",
		ProfileImageURL:  pgtype.Text{String: "https://example.test/first.png", Valid: true},
		EmailVerified:    true,
		Email:            pgtype.Text{String: "owner@example.test", Valid: true},
		IdentityProvider: "github",
		IdentitySubject:  "123",
		IdentityID:       pgvalue.UUID(uuid.NewV7()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != pgvalue.UUID(userID) {
		t.Fatalf("first user = %s, want %s", first.ID, userID)
	}

	repeated, err := queries.UpsertAuthIdentity(t.Context(), db.UpsertAuthIdentityParams{
		UserID:           pgvalue.UUID(uuid.NewV7()),
		DisplayName:      "updated name",
		ProfileImageURL:  pgtype.Text{String: "https://example.test/updated.png", Valid: true},
		EmailVerified:    true,
		Email:            pgtype.Text{String: "owner@example.test", Valid: true},
		IdentityProvider: "github",
		IdentitySubject:  "123",
		IdentityID:       pgvalue.UUID(uuid.NewV7()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if repeated.ID != first.ID || repeated.DisplayName != "updated name" ||
		repeated.ProfileImageURL.String != "https://example.test/updated.png" {
		t.Fatalf("repeated login = %+v, want updated original user", repeated)
	}

	linked, err := queries.UpsertMagicLinkAuthIdentity(t.Context(), db.UpsertMagicLinkAuthIdentityParams{
		UserID:           pgvalue.UUID(uuid.NewV7()),
		DisplayName:      "owner@example.test",
		Email:            pgtype.Text{String: "owner@example.test", Valid: true},
		IdentityProvider: "magic-link",
		IdentitySubject:  "owner@example.test",
		IdentityID:       pgvalue.UUID(uuid.NewV7()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if linked.ID != first.ID {
		t.Fatalf("Magic Link user = %s, want linked user %s", linked.ID, first.ID)
	}

	var identityCount, userCount int
	if err := database.Pool.QueryRow(t.Context(), `
		SELECT count(*), count(DISTINCT user_id)
		  FROM auth_identities
		 WHERE (provider, subject) IN (
		     ('github', '123'),
		     ('magic-link', 'owner@example.test')
		 )
	`).Scan(&identityCount, &userCount); err != nil {
		t.Fatal(err)
	}
	if identityCount != 2 || userCount != 1 {
		t.Fatalf("identities/users = %d/%d, want 2/1", identityCount, userCount)
	}
}
