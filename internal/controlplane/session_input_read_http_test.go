package controlplane

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestSessionInputReadUsesStableValidationCodes(t *testing.T) {
	for _, test := range []struct {
		name string
		url  string
		id   string
		code string
	}{
		{name: "reference", url: "/", id: "bad", code: "invalid_session_id"},
		{name: "read options", url: "/?limit=0", id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33", code: "invalid_session_input_read"},
		{name: "unknown option", url: "/?cursor=1", id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33", code: "invalid_session_input_read"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", test.url, nil)
			route := chi.NewRouteContext()
			route.URLParams.Add("sessionID", test.id)
			request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
			recorder := httptest.NewRecorder()
			(&Server{}).readSessionInputHTTP(recorder, request)
			if recorder.Code != 400 ||
				!strings.Contains(recorder.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

type sessionInputPageStore struct {
	db.Querier
	rows []db.ReadPublicActorInputPageRow
}

func (store sessionInputPageStore) ReadPublicActorInputPage(
	context.Context, db.ReadPublicActorInputPageParams,
) ([]db.ReadPublicActorInputPageRow, error) {
	return store.rows, nil
}

func TestReadSessionInputPageProjectsSourcesAndBounds(t *testing.T) {
	sessionID := pgvalue.UUID(uuid.NewV7())
	runID := uuid.NewV7()
	recordIDs := []uuid.UUID{uuid.NewV7(), uuid.NewV7(), uuid.NewV7()}
	createdAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.FixedZone("offset", 9*60*60))
	row := func(index int, sequence int64, sourceKind string, sourceRunID pgtype.UUID) db.ReadPublicActorInputPageRow {
		return db.ReadPublicActorInputPageRow{
			SessionID: sessionID, NextInputSequence: 4, EffectiveAfter: 0,
			RecordID: pgvalue.UUID(recordIDs[index]), Sequence: sequence,
			Data:        []byte(`{"turn":` + string(rune('0'+sequence)) + `}`),
			SourceRunID: sourceRunID, CreatedAt: pgtype.Timestamptz{Time: createdAt, Valid: true},
		}
	}
	page, err := readSessionInputPage(t.Context(), sessionInputPageStore{rows: []db.ReadPublicActorInputPageRow{
		row(0, 1, "external", pgtype.UUID{}),
		row(1, 2, "run", pgvalue.UUID(runID)),
		row(2, 3, "external", pgtype.UUID{}),
	}}, pgtype.UUID{}, sessionID, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 2 || !page.HasMore || page.NextAfter != 2 {
		t.Fatalf("page = %+v", page)
	}
	if page.Records[0].ID != recordIDs[0].String() ||
		page.Records[0].Source != (api.SessionInputSource{Type: "external"}) ||
		string(page.Records[0].Data) != `{"turn":1}` ||
		page.Records[0].CreatedAt.Location() != time.UTC {
		t.Fatalf("external record = %+v", page.Records[0])
	}
	if page.Records[1].Source != (api.SessionInputSource{Type: "run", RunID: runID.String()}) {
		t.Fatalf("run record = %+v", page.Records[1])
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"source":{"type":"run","run_id":"`+runID.String()+`"}`) ||
		!strings.Contains(string(raw), `"next_after":2,"has_more":true`) {
		t.Fatalf("page JSON = %s", raw)
	}

	empty, err := readSessionInputPage(t.Context(), sessionInputPageStore{rows: []db.ReadPublicActorInputPageRow{
		{SessionID: sessionID, NextInputSequence: 1, EffectiveAfter: 0, Data: []byte("null"), CreatedAt: pgtype.Timestamptz{Valid: true}},
	}}, pgtype.UUID{}, sessionID, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Records) != 0 || empty.HasMore || empty.NextAfter != 0 {
		t.Fatalf("empty page = %+v", empty)
	}
	if _, err := readSessionInputPage(t.Context(), sessionInputPageStore{}, pgtype.UUID{}, sessionID, nil, 50); err == nil {
		t.Fatal("missing Session was projected")
	}
	beyondFrontier := row(0, 4, "external", pgtype.UUID{})
	if _, err := readSessionInputPage(t.Context(), sessionInputPageStore{rows: []db.ReadPublicActorInputPageRow{
		beyondFrontier,
	}}, pgtype.UUID{}, sessionID, nil, 50); err == nil {
		t.Fatal("record at the input frontier was projected")
	}
}
