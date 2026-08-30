package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestCreateProjectSelectsFirstRegionWhenDefaultIsOmitted(t *testing.T) {
	ctx := context.Background()
	database := dbtest.Open(t)
	if err := schema.Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	queries := db.New(database.Pool)
	for _, region := range []db.CreateRegionParams{
		{ID: "region-z", DisplayName: "Zulu"},
		{ID: "region-a", DisplayName: "Alpha"},
	} {
		if _, err := queries.CreateRegion(ctx, region); err != nil {
			t.Fatal(err)
		}
	}
	orgID := uuid.NewV7()
	if _, err := queries.CreateOrganization(ctx, db.CreateOrganizationParams{
		ID: pgvalue.UUID(orgID), Name: "Test", Slug: "test",
	}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(`{
		"slug":"project", "name":"Project"
	}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(context.WithValue(request.Context(), actorContextKey{}, auth.Actor{OrgID: orgID}))
	response := httptest.NewRecorder()
	server := &Server{db: queries, tx: database.Pool}
	server.createProject(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}
	var regionID string
	if err := database.Pool.QueryRow(ctx, `SELECT default_region_id FROM projects WHERE slug = 'project'`).Scan(&regionID); err != nil {
		t.Fatal(err)
	}
	if regionID != "region-a" {
		t.Fatalf("default region = %q, want region-a", regionID)
	}
}

func TestCreateProjectRequiresARegionWhenDefaultIsOmitted(t *testing.T) {
	ctx := context.Background()
	database := dbtest.Open(t)
	if err := schema.Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	queries := db.New(database.Pool)
	orgID := uuid.NewV7()
	if _, err := queries.CreateOrganization(ctx, db.CreateOrganizationParams{
		ID: pgvalue.UUID(orgID), Name: "Test", Slug: "test",
	}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(`{
		"slug":"project", "name":"Project"
	}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(context.WithValue(request.Context(), actorContextKey{}, auth.Actor{OrgID: orgID}))
	response := httptest.NewRecorder()
	server := &Server{db: queries, tx: database.Pool}
	server.createProject(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "no region configured") {
		t.Fatalf("response = %d %s, want missing Region error", response.Code, response.Body.String())
	}
}
