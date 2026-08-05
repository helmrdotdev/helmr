package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestActorOutputReadUsesStableValidationCodes(t *testing.T) {
	for _, test := range []struct {
		name string
		url  string
		id   string
		code string
	}{
		{name: "reference", url: "/", id: "bad", code: "invalid_session_id"},
		{name: "read options", url: "/?limit=0", id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33", code: "invalid_session_output_read"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", test.url, nil)
			route := chi.NewRouteContext()
			route.URLParams.Add("sessionID", test.id)
			request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
			recorder := httptest.NewRecorder()
			(&Server{}).readSessionOutputHTTP(recorder, request)
			if recorder.Code != 400 ||
				!strings.Contains(recorder.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestParseSessionOutputPageOptionsRejectsClosedQueryViolations(t *testing.T) {
	tests := []struct {
		name     string
		rawQuery string
	}{
		{name: "unknown", rawQuery: "cursor=1"},
		{name: "signed after", rawQuery: "after=%2B1"},
		{name: "negative after", rawQuery: "after=-1"},
		{name: "unsafe after", rawQuery: "after=9007199254740992"},
		{name: "zero limit", rawQuery: "limit=0"},
		{name: "large limit", rawQuery: "limit=101"},
		{name: "repeated limit", rawQuery: "limit=1&limit=2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseSessionOutputPageOptions(
				httptest.NewRequest("GET", "/?"+test.rawQuery, nil),
			)
			if err == nil {
				t.Fatal("parseActorOutputReadRequest() succeeded")
			}
		})
	}
}

func TestProjectSessionOutput(t *testing.T) {
	recordUUID := uuid.Must(uuid.NewV7())
	runID := uuid.Must(uuid.NewV7())
	deploymentID := uuid.Must(uuid.NewV7())
	createdAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.FixedZone("offset", 9*60*60))
	record, err := projectSessionOutput(db.ReadPublicActorOutputPageRow{
		RecordID:              pgvalue.UUID(recordUUID),
		EffectiveAfter:        1,
		NextOutputSequence:    3,
		Sequence:              2,
		Data:                  []byte(`null`),
		ContentType:           "application/json",
		CreatedAt:             pgtype.Timestamptz{Time: createdAt, Valid: true},
		RunID:                 pgvalue.UUID(runID),
		ProducerAttemptNumber: 2,
		DeploymentID:          pgvalue.UUID(deploymentID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != recordUUID.String() ||
		record.Sequence != 2 ||
		string(record.Data) != "null" ||
		record.CreatedAt.Location() != time.UTC ||
		record.Provenance.RunID != runID.String() ||
		record.Provenance.AttemptNumber != 2 ||
		record.Provenance.DeploymentID != deploymentID.String() {
		t.Fatalf("record = %+v", record)
	}
}

func TestAuthorizeActorOutputReadBeforeLookup(t *testing.T) {
	scope := auth.Scope{
		ProjectID:     uuid.Must(uuid.NewV7()).String(),
		EnvironmentID: uuid.Must(uuid.NewV7()).String(),
	}
	for _, role := range []auth.Role{
		auth.RoleOwner,
		auth.RoleAdmin,
		auth.RoleDeveloper,
		auth.RoleViewer,
	} {
		t.Run(string(role), func(t *testing.T) {
			if err := authorizeSessionOutputReadBeforeLookup(auth.Actor{
				Kind: auth.ActorKindSession,
				Role: role,
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := authorizeSessionOutputReadBeforeLookup(auth.Actor{
		Kind:          auth.ActorKindAPIKey,
		Role:          auth.RoleDeveloper,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		Permissions:   []auth.Permission{auth.PermissionSessionsRead},
	}); err != nil {
		t.Fatal(err)
	}
	err := authorizeSessionOutputReadBeforeLookup(auth.Actor{
		Kind:          auth.ActorKindAPIKey,
		Role:          auth.RoleDeveloper,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
	})
	var coder errorCoder
	if !errors.As(err, &coder) || coder.ErrorCode() != "permission_required" {
		t.Fatalf("error = %v", err)
	}
}

func TestSessionOutputPageEncodesJSONNull(t *testing.T) {
	page := api.SessionOutputPage{
		Records: []api.SessionOutput{{Data: json.RawMessage(`null`)}},
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"data":null`) {
		t.Fatalf("page JSON = %s", raw)
	}
}
