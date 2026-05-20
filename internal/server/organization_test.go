package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const organizationTestAuthSecret = "abcdefghijabcdefghijabcdefghij12"

func TestCreateOrganizationRequiresSetupTokenAndSingleton(t *testing.T) {
	ctx := context.Background()
	queries, pool := newServerPostgresTestDB(t, ctx)
	userID := ids.New()
	rawSession := createOrganizationTestSession(t, ctx, queries, pool, userID, "owner@example.test")
	handler := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDBTX(pool),
		WithUserAuth(organizationTestAuthSecret, "https://helmr.example.test"),
		WithInitialSetupToken("setup-secret"),
	)

	for _, tt := range []struct {
		name       string
		body       string
		wantStatus int
	}{
		{
			name:       "missing token",
			body:       `{"name":"Acme","slug":"acme"}`,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "wrong token",
			body:       `{"name":"Acme","slug":"acme","setup_token":"wrong"}`,
			wantStatus: http.StatusForbidden,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, createOrganizationTestRequest(tt.body, rawSession))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			orgIDs, err := queries.ListOrganizationIDs(ctx, 2)
			if err != nil {
				t.Fatal(err)
			}
			if len(orgIDs) != 0 {
				t.Fatalf("organizations = %v", orgIDs)
			}
		})
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, createOrganizationTestRequest(`{"name":"Acme","slug":"acme","setup_token":"setup-secret"}`, rawSession))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var created api.OrganizationSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	orgID, err := ids.Parse(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	member, err := queries.GetOrgMember(ctx, db.GetOrgMemberParams{
		OrgID:  ids.ToPG(orgID),
		UserID: ids.ToPG(userID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if member.Role != db.OrgMemberRoleOwner {
		t.Fatalf("member role = %q", member.Role)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, createOrganizationTestRequest(`{"name":"Other","slug":"other","setup_token":"setup-secret"}`, rawSession))
	if second.Code != http.StatusConflict {
		t.Fatalf("second status = %d body=%s", second.Code, second.Body.String())
	}
}

func TestMeReturnsAccessRequiredAfterSingletonOrganizationExists(t *testing.T) {
	ctx := context.Background()
	queries, pool := newServerPostgresTestDB(t, ctx)
	ownerID := ids.New()
	otherID := ids.New()
	ownerSession := createOrganizationTestSession(t, ctx, queries, pool, ownerID, "owner@example.test")
	otherSession := createOrganizationTestSession(t, ctx, queries, pool, otherID, "other@example.test")
	handler := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDBTX(pool),
		WithUserAuth(organizationTestAuthSecret, "https://helmr.example.test"),
		WithInitialSetupToken("setup-secret"),
	)

	before := getMeForSession(t, handler, otherSession)
	if !before.OrganizationRequired || !before.SetupTokenRequired || before.AccessRequired {
		t.Fatalf("before setup me = %+v", before)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, createOrganizationTestRequest(`{"name":"Acme","slug":"acme","setup_token":"setup-secret"}`, ownerSession))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	after := getMeForSession(t, handler, otherSession)
	if after.OrganizationRequired || after.SetupTokenRequired || !after.AccessRequired || after.ProjectRequired {
		t.Fatalf("after setup me = %+v", after)
	}
}

func TestManagedCloudAllowsMultipleOrganizationsWithoutSetupToken(t *testing.T) {
	ctx := context.Background()
	queries, pool := newServerPostgresTestDB(t, ctx)
	firstUserID := ids.New()
	secondUserID := ids.New()
	firstSession := createOrganizationTestSession(t, ctx, queries, pool, firstUserID, "first@example.test")
	secondSession := createOrganizationTestSession(t, ctx, queries, pool, secondUserID, "second@example.test")
	handler := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDeploymentMode(deploymentModeManagedCloud),
		WithDBTX(pool),
		WithUserAuth(organizationTestAuthSecret, "https://helmr.example.test"),
	)

	firstMe := getMeForSession(t, handler, firstSession)
	if !firstMe.OrganizationRequired || firstMe.SetupTokenRequired || firstMe.AccessRequired {
		t.Fatalf("first me = %+v", firstMe)
	}
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, createOrganizationTestRequest(`{"name":"First","slug":"first"}`, firstSession))
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d body=%s", first.Code, first.Body.String())
	}

	secondMe := getMeForSession(t, handler, secondSession)
	if !secondMe.OrganizationRequired || secondMe.SetupTokenRequired || secondMe.AccessRequired {
		t.Fatalf("second me = %+v", secondMe)
	}
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, createOrganizationTestRequest(`{"name":"Second","slug":"second"}`, secondSession))
	if second.Code != http.StatusCreated {
		t.Fatalf("second status = %d body=%s", second.Code, second.Body.String())
	}
	orgIDs, err := queries.ListOrganizationIDs(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(orgIDs) != 2 {
		t.Fatalf("organizations = %v", orgIDs)
	}
}

func createOrganizationTestSession(t *testing.T, ctx context.Context, queries *db.Queries, pool *pgxpool.Pool, userID uuid.UUID, email string) string {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, display_name, primary_email) VALUES ($1, $2, $3)`, ids.ToPG(userID), email, email); err != nil {
		t.Fatal(err)
	}
	rawSession := "session-" + userID.String()
	tokenHash, err := auth.HashToken([]byte(organizationTestAuthSecret), rawSession)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateSession(ctx, db.CreateSessionParams{
		ID:        ids.ToPG(ids.New()),
		OrgID:     pgtype.UUID{},
		UserID:    ids.ToPG(userID),
		TokenHash: tokenHash,
		ExpiresAt: pgTimeToPG(time.Now().Add(time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}
	return rawSession
}

func createOrganizationTestRequest(body string, rawSession string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "https://helmr.example.test/api/organizations", bytes.NewBufferString(body))
	req.Header.Set("content-type", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookieName(req), Value: rawSession})
	return req
}

func getMeForSession(t *testing.T, handler http.Handler, rawSession string) api.MeResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "https://helmr.example.test/api/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(req), Value: rawSession})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("me status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.MeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}
