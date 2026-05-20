package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestUpsertAuthIdentityCreatesNewUserAndUpdatesExisting(t *testing.T) {
	ctx := context.Background()
	queries, _ := newPostgresTestDB(t, ctx)

	first, err := queries.UpsertAuthIdentity(ctx, db.UpsertAuthIdentityParams{
		UserID:           ids.ToPG(ids.New()),
		IdentityID:       ids.ToPG(ids.New()),
		IdentityProvider: "github",
		IdentitySubject:  "123",
		DisplayName:      "octocat",
		ProfileImageUrl:  pgtype.Text{String: "https://avatars.example.test/octocat.png", Valid: true},
		Email:            pgtype.Text{String: "octocat@example.com", Valid: true},
		Claims:           []byte(`{"login":"octocat"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.DisplayName != "octocat" ||
		first.ProfileImageUrl.String != "https://avatars.example.test/octocat.png" ||
		first.PrimaryEmail.String != "octocat@example.com" {
		t.Fatalf("created user = %+v", first)
	}

	second, err := queries.UpsertAuthIdentity(ctx, db.UpsertAuthIdentityParams{
		UserID:           ids.ToPG(ids.New()),
		IdentityID:       ids.ToPG(ids.New()),
		IdentityProvider: "github",
		IdentitySubject:  "123",
		DisplayName:      "octo",
		ProfileImageUrl:  pgtype.Text{String: "https://avatars.example.test/octo.png", Valid: true},
		Email:            pgtype.Text{String: "octo@example.com", Valid: true},
		Claims:           []byte(`{"login":"octo"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID ||
		second.DisplayName != "octo" ||
		second.ProfileImageUrl.String != "https://avatars.example.test/octo.png" ||
		second.PrimaryEmail.String != "octo@example.com" {
		t.Fatalf("updated user = %+v, first = %+v", second, first)
	}
}

func TestOwnerExistsIgnoresDisabledUsers(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	userID := ids.ToPG(ids.New())

	seedPostgresTestOrganization(t, ctx, pool, orgID)
	exists, err := queries.OwnerExists(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("owner exists before seeding owner")
	}

	if _, err := pool.Exec(ctx, "INSERT INTO users (id, display_name, primary_email) VALUES ($1, $2, $3)", userID, "octocat", "octocat@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO org_members (org_id, user_id, role, display_name) VALUES ($1, $2, $3, $4)", orgID, userID, db.OrgMemberRoleOwner, "octocat"); err != nil {
		t.Fatal(err)
	}

	exists, err = queries.OwnerExists(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("owner does not exist after seeding owner")
	}

	if _, err := pool.Exec(ctx, "UPDATE users SET disabled_at = now() WHERE id = $1", userID); err != nil {
		t.Fatal(err)
	}
	exists, err = queries.OwnerExists(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("disabled user should not count as owner")
	}
}

func TestTouchActiveAPIKeyRequiresActiveCreatorMembership(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	seedPostgresTestOrganization(t, ctx, pool, orgID)
	bootstrapKey, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.IssueAPIKey(ctx, db.IssueAPIKeyParams{
		ID:        ids.ToPG(ids.New()),
		OrgID:     orgID,
		Name:      "bootstrap",
		KeyPrefix: bootstrapKey.KeyPrefix,
		TokenHash: bootstrapKey.TokenHash,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.TouchActiveAPIKeyByTokenHash(ctx, bootstrapKey.TokenHash); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("creatorless key auth error = %v, want pgx.ErrNoRows", err)
	}

	userID := ids.ToPG(ids.New())
	if _, err := pool.Exec(ctx, "INSERT INTO users (id, display_name, primary_email) VALUES ($1, $2, $3)", userID, "octocat", "octocat@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO org_members (org_id, user_id, role, display_name) VALUES ($1, $2, $3, $4)", orgID, userID, db.OrgMemberRoleOwner, "octocat"); err != nil {
		t.Fatal(err)
	}
	apiKey, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.IssueAPIKey(ctx, db.IssueAPIKeyParams{
		ID:              ids.ToPG(ids.New()),
		OrgID:           orgID,
		CreatedByUserID: userID,
		Name:            "cli",
		KeyPrefix:       apiKey.KeyPrefix,
		TokenHash:       apiKey.TokenHash,
	}); err != nil {
		t.Fatal(err)
	}
	row, err := queries.TouchActiveAPIKeyByTokenHash(ctx, apiKey.TokenHash)
	if err != nil {
		t.Fatal(err)
	}
	if row.Role != string(db.OrgMemberRoleOwner) {
		t.Fatalf("role = %q", row.Role)
	}

	if _, err := pool.Exec(ctx, "UPDATE org_members SET disabled_at = now() WHERE org_id = $1 AND user_id = $2", orgID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.TouchActiveAPIKeyByTokenHash(ctx, apiKey.TokenHash); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("disabled creator key auth error = %v, want pgx.ErrNoRows", err)
	}
}

func TestUpdatedAtTriggerIsInstalled(t *testing.T) {
	ctx := context.Background()
	_, pool := newPostgresTestDB(t, ctx)

	var triggerCount int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
  FROM pg_trigger
 WHERE tgname = ANY($1)
   AND NOT tgisinternal
`, []string{
		"users_set_updated_at",
		"auth_identities_set_updated_at",
		"org_members_set_updated_at",
		"secrets_set_updated_at",
		"github_app_installations_set_updated_at",
		"runs_set_updated_at",
	}).Scan(&triggerCount); err != nil {
		t.Fatal(err)
	}
	if triggerCount != 6 {
		t.Fatalf("updated_at trigger count = %d, want 6", triggerCount)
	}

	userID := ids.ToPG(ids.New())
	oldUpdatedAt := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, display_name, primary_email, updated_at)
VALUES ($1, 'before', 'before@example.com', $2)
`, userID, oldUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET display_name = 'after' WHERE id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	var updatedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM users WHERE id = $1`, userID).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	if !updatedAt.After(oldUpdatedAt) {
		t.Fatalf("updated_at = %s, want after %s", updatedAt, oldUpdatedAt)
	}
}
