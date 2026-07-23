package control

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
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestActorOutputReadUsesStableValidationCodes(t *testing.T) {
	for _, test := range []struct {
		name string
		url  string
		code string
	}{
		{name: "reference", url: "/?actor_id=bad", code: "invalid_actor_reference"},
		{name: "read options", url: "/?actor_key=thread&limit=0", code: "invalid_actor_output_read"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", test.url, nil)
			route := chi.NewRouteContext()
			route.URLParams.Add("actorDeclaredID", "operator.v1")
			request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
			recorder := httptest.NewRecorder()
			(&Server{}).readActorOutputHTTP(recorder, request)
			if recorder.Code != 400 ||
				!strings.Contains(recorder.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestParseActorOutputReadRequest(t *testing.T) {
	actorID, err := publicid.New(publicid.Actor)
	if err != nil {
		t.Fatal(err)
	}
	maxAfter := int64(1<<53 - 1)
	request := httptest.NewRequest(
		"GET",
		"/?actor_id="+actorID+"&after=9007199254740991&limit=100",
		nil,
	)
	parsed, err := parseActorOutputReadRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.address.publicID != actorID ||
		parsed.after == nil ||
		*parsed.after != maxAfter ||
		parsed.limit != actorOutputReadMaxLimit {
		t.Fatalf("parsed request = %+v", parsed)
	}

	withoutCursor, err := parseActorOutputReadRequest(
		httptest.NewRequest("GET", "/?actor_key=thread%3A1", nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if withoutCursor.address.key != "thread:1" ||
		withoutCursor.after != nil ||
		withoutCursor.limit != actorOutputReadDefaultLimit {
		t.Fatalf("parsed request = %+v", withoutCursor)
	}
}

func TestParseActorOutputReadRequestRejectsClosedQueryViolations(t *testing.T) {
	actorID, err := publicid.New(publicid.Actor)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		rawQuery  string
		reference bool
	}{
		{name: "neither address", rawQuery: "", reference: true},
		{name: "both addresses", rawQuery: "actor_id=" + actorID + "&actor_key=thread", reference: true},
		{name: "invalid actor id", rawQuery: "actor_id=act_bad", reference: true},
		{name: "repeated address", rawQuery: "actor_key=a&actor_key=b", reference: true},
		{name: "unknown", rawQuery: "actor_key=a&cursor=1"},
		{name: "signed after", rawQuery: "actor_key=a&after=%2B1"},
		{name: "negative after", rawQuery: "actor_key=a&after=-1"},
		{name: "unsafe after", rawQuery: "actor_key=a&after=9007199254740992"},
		{name: "zero limit", rawQuery: "actor_key=a&limit=0"},
		{name: "large limit", rawQuery: "actor_key=a&limit=101"},
		{name: "repeated limit", rawQuery: "actor_key=a&limit=1&limit=2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseActorOutputReadRequest(
				httptest.NewRequest("GET", "/?"+test.rawQuery, nil),
			)
			if err == nil {
				t.Fatal("parseActorOutputReadRequest() succeeded")
			}
			var referenceError actorOutputReferenceError
			if got := errors.As(err, &referenceError); got != test.reference {
				t.Fatalf("reference error = %v, want %v: %v", got, test.reference, err)
			}
		})
	}
}

func TestProjectActorOutputRecord(t *testing.T) {
	recordUUID := uuid.Must(uuid.NewV7())
	runID, err := publicid.New(publicid.Run)
	if err != nil {
		t.Fatal(err)
	}
	deploymentID, err := publicid.New(publicid.Deployment)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.FixedZone("offset", 9*60*60))
	record, err := projectActorOutputRecord(db.ReadPublicActorOutputPageRow{
		RecordID:              pgvalue.UUID(recordUUID),
		EffectiveAfter:        1,
		NextOutputSequence:    3,
		Sequence:              2,
		Data:                  []byte(`null`),
		ContentType:           "application/json",
		CreatedAt:             pgtype.Timestamptz{Time: createdAt, Valid: true},
		RunPublicID:           runID,
		ProducerAttemptNumber: 2,
		DeploymentPublicID:    deploymentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(record.ID, publicid.ActorRecord.String()) ||
		record.Sequence != 2 ||
		string(record.Data) != "null" ||
		record.CreatedAt.Location() != time.UTC ||
		record.Provenance.RunID != runID ||
		record.Provenance.AttemptNumber != 2 ||
		record.Provenance.DeploymentID != deploymentID {
		t.Fatalf("record = %+v", record)
	}
	if _, err := publicid.DecodeActorRecord(record.ID); err != nil {
		t.Fatalf("record ID = %q: %v", record.ID, err)
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
			if err := authorizeActorOutputReadBeforeLookup(auth.Actor{
				Kind: auth.ActorKindSession,
				Role: role,
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := authorizeActorOutputReadBeforeLookup(auth.Actor{
		Kind:          auth.ActorKindAPIKey,
		Role:          auth.RoleDeveloper,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		Permissions:   []auth.Permission{auth.PermissionActorsRead},
	}); err != nil {
		t.Fatal(err)
	}
	err := authorizeActorOutputReadBeforeLookup(auth.Actor{
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

func TestActorOutputPageEncodesJSONNull(t *testing.T) {
	page := api.ActorOutputPage{
		Records: []api.ActorOutputRecord{{Data: json.RawMessage(`null`)}},
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"data":null`) {
		t.Fatalf("page JSON = %s", raw)
	}
}
