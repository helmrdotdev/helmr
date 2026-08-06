package controlplane

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestDeploymentListCursorRoundTripAndScope(t *testing.T) {
	id := uuid.Must(uuid.NewV7())
	raw, err := encodeDeploymentListCursor(deploymentListCursor{
		ProjectID: "project-1", EnvironmentID: "environment-1",
		CreatedAt: time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC), ID: id.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/deployments?limit=25&cursor="+raw, nil)
	limit, cursor, err := parseDeploymentListQuery(request, "project-1", "environment-1")
	if err != nil {
		t.Fatal(err)
	}
	if limit != 25 || cursor == nil || cursor.ID != id.String() {
		t.Fatalf("limit=%d cursor=%+v", limit, cursor)
	}
	if _, _, err := parseDeploymentListQuery(request, "project-1", "environment-2"); err == nil {
		t.Fatal("cross-environment Deployment cursor succeeded")
	}
}

func TestDeploymentListQueryRejectsUnknownParameters(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/v1/deployments?status=deployed", nil)
	if _, _, err := parseDeploymentListQuery(request, "project-1", "environment-1"); err == nil {
		t.Fatal("unsupported Deployment filter succeeded")
	}
}

func TestTokenListCursorBindsStatus(t *testing.T) {
	id := uuid.Must(uuid.NewV7())
	raw, err := encodeTokenListCursor(tokenListCursor{
		ProjectID: "project-1", EnvironmentID: "environment-1", Status: "completed",
		CreatedAt: time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC), ID: id.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := decodeTokenListCursor(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.Status != "completed" || cursor.ID != id.String() {
		t.Fatalf("cursor=%+v", cursor)
	}
	if err := validateTokenListQuery(url.Values{"project_id": {"project-1"}}); err == nil {
		t.Fatal("Developer API Token list accepted an explicit project scope")
	}
}

func TestWorkspaceListItemExcludesSecretPlacements(t *testing.T) {
	now := pgvalue.Timestamptz(time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC))
	item, err := workspaceListItem(
		pgvalue.UUID(uuid.Must(uuid.NewV7())), pgvalue.Text("repository"), "repository-agent",
		pgvalue.UUID(uuid.Must(uuid.NewV7())), db.WorkspaceStateActive, now, now, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if item.Key == nil || *item.Key != "repository" || item.Status != api.WorkspaceStatusAvailable {
		t.Fatalf("item=%+v", item)
	}
}

func TestRunListItemProjectsOnlyCollectionFields(t *testing.T) {
	now := pgvalue.Timestamptz(time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC))
	item, err := projectRunListItem(db.ListRunListItemsRow{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), Status: db.RunStatusRunning,
		EntrypointKind: "task", EntrypointDeclaredID: "resize-image",
		WorkspaceID: pgvalue.UUID(uuid.Must(uuid.NewV7())), CurrentAttemptNumber: 1,
		CreatedAt: now, StartedAt: now, TerminalAt: pgtype.Timestamptz{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != api.RunStatusRunning || item.Entrypoint.ID != "resize-image" || item.TerminalAt != nil {
		t.Fatalf("item=%+v", item)
	}
}
