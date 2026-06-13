package db_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/ids"
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
		EmailVerified:    true,
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
		EmailVerified:    true,
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

func TestUpsertAuthIdentityDoesNotLinkUnverifiedEmail(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	existingUserID := ids.ToPG(ids.New())
	if _, err := pool.Exec(ctx, "INSERT INTO users (id, display_name, primary_email) VALUES ($1, $2, $3)", existingUserID, "owner", "owner@example.com"); err != nil {
		t.Fatal(err)
	}

	row, err := queries.UpsertAuthIdentity(ctx, db.UpsertAuthIdentityParams{
		UserID:           ids.ToPG(ids.New()),
		IdentityID:       ids.ToPG(ids.New()),
		IdentityProvider: "github",
		IdentitySubject:  "attacker",
		DisplayName:      "attacker",
		Email:            pgtype.Text{String: "owner@example.com", Valid: true},
		EmailVerified:    false,
		Claims:           []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.ID == existingUserID {
		t.Fatal("unverified email linked to existing user")
	}
	if row.PrimaryEmail.Valid {
		t.Fatalf("unverified email was stored as primary email: %+v", row.PrimaryEmail)
	}
}

func TestUpsertAuthIdentityConcurrentEmailCreatesOneUser(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name              string
		email             string
		wantIdentityCount int
		upsert            func(context.Context, *db.Queries, int) (db.UpsertAuthIdentityRow, error)
	}{
		{
			name:              "oauth",
			email:             "octo-race@example.com",
			wantIdentityCount: 2,
			upsert: func(ctx context.Context, queries *db.Queries, index int) (db.UpsertAuthIdentityRow, error) {
				return queries.UpsertAuthIdentity(ctx, db.UpsertAuthIdentityParams{
					UserID:           ids.ToPG(ids.New()),
					IdentityID:       ids.ToPG(ids.New()),
					IdentityProvider: "github",
					IdentitySubject:  fmt.Sprintf("race-%d", index),
					DisplayName:      fmt.Sprintf("octo-%d", index),
					Email:            pgtype.Text{String: "octo-race@example.com", Valid: true},
					EmailVerified:    true,
					Claims:           []byte(`{}`),
				})
			},
		},
		{
			name:              "magic_link",
			email:             "magic-race@example.com",
			wantIdentityCount: 1,
			upsert: func(ctx context.Context, queries *db.Queries, index int) (db.UpsertAuthIdentityRow, error) {
				row, err := queries.UpsertMagicLinkAuthIdentity(ctx, db.UpsertMagicLinkAuthIdentityParams{
					UserID:           ids.ToPG(ids.New()),
					IdentityID:       ids.ToPG(ids.New()),
					IdentityProvider: "magic-link",
					IdentitySubject:  "magic-race@example.com",
					DisplayName:      fmt.Sprintf("magic-%d", index),
					Email:            pgtype.Text{String: "magic-race@example.com", Valid: true},
					Claims:           []byte(`{}`),
				})
				return db.UpsertAuthIdentityRow(row), err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queries, pool := newPostgresTestDB(t, ctx)
			start := make(chan struct{})
			results := make(chan struct {
				row db.UpsertAuthIdentityRow
				err error
			}, 2)

			for i := range 2 {
				go func(index int) {
					<-start
					row, err := tt.upsert(ctx, queries, index)
					results <- struct {
						row db.UpsertAuthIdentityRow
						err error
					}{row: row, err: err}
				}(i)
			}
			close(start)

			first := <-results
			second := <-results
			if first.err != nil {
				t.Fatal(first.err)
			}
			if second.err != nil {
				t.Fatal(second.err)
			}
			if first.row.ID != second.row.ID {
				t.Fatalf("concurrent upserts returned different users: %v and %v", first.row.ID, second.row.ID)
			}

			var userCount int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE lower(primary_email) = lower($1)`, tt.email).Scan(&userCount); err != nil {
				t.Fatal(err)
			}
			if userCount != 1 {
				t.Fatalf("user count = %d, want 1", userCount)
			}

			var identityCount int
			if err := pool.QueryRow(ctx, `
SELECT count(*)
  FROM auth_identities
 WHERE lower(email) = lower($1)
`, tt.email).Scan(&identityCount); err != nil {
				t.Fatal(err)
			}
			if identityCount != tt.wantIdentityCount {
				t.Fatalf("identity count = %d, want %d", identityCount, tt.wantIdentityCount)
			}
		})
	}
}

func TestOwnerExistsIgnoresDisabledUsers(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)
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

func TestTouchActiveAPIKeyUsesStoredKeyRole(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)

	seedPostgresTestOrganization(t, ctx, pool, orgID)
	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	bootstrapKey, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.IssueAPIKey(ctx, db.IssueAPIKeyParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		Role:          db.OrgMemberRoleOwner,
		Name:          "bootstrap",
		KeyPrefix:     bootstrapKey.KeyPrefix,
		TokenHash:     bootstrapKey.TokenHash,
	}); err != nil {
		t.Fatal(err)
	}
	bootstrapRow, err := queries.TouchActiveAPIKeyByTokenHash(ctx, bootstrapKey.TokenHash)
	if err != nil {
		t.Fatal(err)
	}
	if bootstrapRow.Role != string(db.OrgMemberRoleOwner) {
		t.Fatalf("bootstrap role = %q", bootstrapRow.Role)
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
		ProjectID:       scope.ProjectID,
		EnvironmentID:   scope.EnvironmentID,
		CreatedByUserID: userID,
		Role:            db.OrgMemberRoleViewer,
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
	if row.Role != string(db.OrgMemberRoleViewer) {
		t.Fatalf("role = %q", row.Role)
	}

	if _, err := pool.Exec(ctx, "UPDATE org_members SET disabled_at = now() WHERE org_id = $1 AND user_id = $2", orgID, userID); err != nil {
		t.Fatal(err)
	}
	row, err = queries.TouchActiveAPIKeyByTokenHash(ctx, apiKey.TokenHash)
	if err != nil {
		t.Fatal(err)
	}
	if row.Role != string(db.OrgMemberRoleViewer) {
		t.Fatalf("role after creator disabled = %q", row.Role)
	}
}

func TestIssueAPIKeyRevokesSameNameOnlyInScope(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)

	seedPostgresTestOrganization(t, ctx, pool, orgID)
	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	siblingEnvironment, err := queries.CreateEnvironment(ctx, db.CreateEnvironmentParams{
		ID:        ids.ToPG(ids.New()),
		OrgID:     orgID,
		ProjectID: scope.ProjectID,
		Slug:      "sibling",
		Name:      "Sibling",
		ColorHex:  "#4F46E5",
	})
	if err != nil {
		t.Fatal(err)
	}

	firstToken := []byte("same-name-token-1")
	if _, err := queries.IssueAPIKey(ctx, db.IssueAPIKeyParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		Role:          db.OrgMemberRoleDeveloper,
		Name:          "deployer",
		KeyPrefix:     "key_1",
		TokenHash:     firstToken,
	}); err != nil {
		t.Fatal(err)
	}
	siblingToken := []byte("same-name-token-2")
	if _, err := queries.IssueAPIKey(ctx, db.IssueAPIKeyParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: siblingEnvironment.ID,
		Role:          db.OrgMemberRoleDeveloper,
		Name:          "deployer",
		KeyPrefix:     "key_2",
		TokenHash:     siblingToken,
	}); err != nil {
		t.Fatal(err)
	}
	replacementToken := []byte("same-name-token-3")
	if _, err := queries.IssueAPIKey(ctx, db.IssueAPIKeyParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		Role:          db.OrgMemberRoleDeveloper,
		Name:          "deployer",
		KeyPrefix:     "key_3",
		TokenHash:     replacementToken,
	}); err != nil {
		t.Fatal(err)
	}

	var firstRevoked, siblingRevoked, replacementRevoked bool
	if err := pool.QueryRow(ctx, `
SELECT revoked_at IS NOT NULL
  FROM api_keys
 WHERE token_hash = $1
`, firstToken).Scan(&firstRevoked); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
SELECT revoked_at IS NOT NULL
  FROM api_keys
 WHERE token_hash = $1
`, siblingToken).Scan(&siblingRevoked); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
SELECT revoked_at IS NOT NULL
  FROM api_keys
 WHERE token_hash = $1
`, replacementToken).Scan(&replacementRevoked); err != nil {
		t.Fatal(err)
	}
	if !firstRevoked {
		t.Fatal("same-scope API key was not revoked")
	}
	if siblingRevoked {
		t.Fatal("sibling environment API key was revoked")
	}
	if replacementRevoked {
		t.Fatal("replacement API key was revoked")
	}
}

func TestWaitpointPolicyNameIsScopedToEnvironment(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)

	seedPostgresTestOrganization(t, ctx, pool, orgID)
	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	siblingEnvironment, err := queries.CreateEnvironment(ctx, db.CreateEnvironmentParams{
		ID:        ids.ToPG(ids.New()),
		OrgID:     orgID,
		ProjectID: scope.ProjectID,
		Slug:      "approval-sibling",
		Name:      "Approval sibling",
		ColorHex:  "#0891B2",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := queries.CreateWaitpointPolicy(ctx, db.CreateWaitpointPolicyParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		Name:          "deploy-approval",
		Label:         "Production approval",
		Config:        []byte(`{"deliveries":[{"type":"email","to":["prod@example.test"]}]}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateWaitpointPolicy(ctx, db.CreateWaitpointPolicyParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: siblingEnvironment.ID,
		Name:          "deploy-approval",
		Label:         "Staging approval",
		Config:        []byte(`{"deliveries":[{"type":"email","to":["staging@example.test"]}]}`),
	}); err != nil {
		t.Fatal(err)
	}

	productionPolicy, err := queries.GetWaitpointPolicyByName(ctx, db.GetWaitpointPolicyByNameParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		Name:          "deploy-approval",
	})
	if err != nil {
		t.Fatal(err)
	}
	if productionPolicy.Label != "Production approval" {
		t.Fatalf("production policy = %+v", productionPolicy)
	}
	siblingPolicy, err := queries.GetWaitpointPolicyByName(ctx, db.GetWaitpointPolicyByNameParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: siblingEnvironment.ID,
		Name:          "deploy-approval",
	})
	if err != nil {
		t.Fatal(err)
	}
	if siblingPolicy.Label != "Staging approval" {
		t.Fatalf("sibling policy = %+v", siblingPolicy)
	}
}

func TestDisableOrgMemberAndRevokeOrgSessionsRevokesGlobalSession(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)
	userID := ids.ToPG(ids.New())
	sessionID := ids.ToPG(ids.New())

	seedPostgresTestOrganization(t, ctx, pool, orgID)
	if _, err := pool.Exec(ctx, "INSERT INTO users (id, display_name, primary_email) VALUES ($1, $2, $3)", userID, "octocat", "octocat@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO org_members (org_id, user_id, role, display_name) VALUES ($1, $2, $3, $4)", orgID, userID, db.OrgMemberRoleDeveloper, "octocat"); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateSession(ctx, db.CreateSessionParams{
		ID:        sessionID,
		UserID:    userID,
		TokenHash: []byte("global-session-token"),
		ExpiresAt: pgTime(time.Now().Add(time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}

	removed, err := queries.DisableOrgMemberAndRevokeOrgSessions(ctx, db.DisableOrgMemberAndRevokeOrgSessionsParams{
		OrgID:        orgID,
		UserID:       userID,
		ExpectedRole: db.OrgMemberRoleDeveloper,
		ActorIsOwner: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if removed.RevokedSessionCount != 1 {
		t.Fatalf("revoked session count = %d, want 1", removed.RevokedSessionCount)
	}
	var revokedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, "SELECT revoked_at FROM sessions WHERE id = $1", sessionID).Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	if !revokedAt.Valid {
		t.Fatal("global session was not revoked")
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
		"runs_set_updated_at",
	}).Scan(&triggerCount); err != nil {
		t.Fatal(err)
	}
	if triggerCount != 5 {
		t.Fatalf("updated_at trigger count = %d, want 5", triggerCount)
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
