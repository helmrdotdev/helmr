package controlplane

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const adminTestSession = "admin-session-token-with-more-than-forty-characters"

type adminHTTPQuerier struct {
	db.Querier
	admin       bool
	listCalls   int
	rotateCalls int
	conflict    bool
}

func (q *adminHTTPQuerier) GetAuthSessionByTokenHash(context.Context, []byte) (db.GetAuthSessionByTokenHashRow, error) {
	return db.GetAuthSessionByTokenHashRow{
		ID:          pgtype.UUID{Bytes: uuid.Must(uuid.NewV7()), Valid: true},
		UserID:      pgtype.UUID{Bytes: uuid.Must(uuid.NewV7()), Valid: true},
		DisplayName: "Administrator",
		Admin:       q.admin,
	}, nil
}

func (q *adminHTTPQuerier) RefreshAuthSession(context.Context, db.RefreshAuthSessionParams) error {
	return nil
}

func (q *adminHTTPQuerier) ListRegions(context.Context) ([]db.Region, error) {
	q.listCalls++
	return []db.Region{}, nil
}

func (q *adminHTTPQuerier) RotateWorkerGroupToken(context.Context, db.RotateWorkerGroupTokenParams) (db.WorkerGroupToken, error) {
	q.rotateCalls++
	return db.WorkerGroupToken{}, nil
}

func (q *adminHTTPQuerier) BeginQuerier(context.Context) (db.Querier, transaction, error) {
	return q, &adminHTTPTransaction{}, nil
}

func (q *adminHTTPQuerier) LockWorkerGroupMutation(context.Context, int64) error {
	return nil
}

func (q *adminHTTPQuerier) TransitionWorkerGroupState(context.Context, db.TransitionWorkerGroupStateParams) (db.TransitionWorkerGroupStateRow, error) {
	if q.conflict {
		return db.TransitionWorkerGroupStateRow{}, pgx.ErrNoRows
	}
	return db.TransitionWorkerGroupStateRow{}, nil
}

type adminHTTPTransaction struct{}

func (*adminHTTPTransaction) Commit(context.Context) error   { return nil }
func (*adminHTTPTransaction) Rollback(context.Context) error { return nil }

func adminHTTPRouter(t *testing.T, queries db.Querier) http.Handler {
	t.Helper()
	keys, err := auth.NewKeys(bytes.Repeat([]byte{1}, auth.RootKeySize))
	if err != nil {
		t.Fatal(err)
	}
	publicURL, err := url.Parse("https://helmr.example.test")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		db: queries, authKeys: keys, publicURL: publicURL,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	router := chi.NewRouter()
	server.mountRoutes(router)
	return router
}

func adminHTTPRequest(method, target, body string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+adminTestSession)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

func TestAdminHTTPRequiresPlatformAdminSession(t *testing.T) {
	for _, test := range []struct {
		name          string
		authenticated bool
		admin         bool
		wantStatus    int
		wantCalls     int
	}{
		{name: "unauthenticated", wantStatus: http.StatusUnauthorized},
		{name: "ordinary session", authenticated: true, wantStatus: http.StatusForbidden},
		{name: "administrator", authenticated: true, admin: true, wantStatus: http.StatusOK, wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			queries := &adminHTTPQuerier{admin: test.admin}
			request := httptest.NewRequest(http.MethodGet, "/admin/api/v1/regions", nil)
			if test.authenticated {
				request.Header.Set("Authorization", "Bearer "+adminTestSession)
			}
			response := httptest.NewRecorder()
			adminHTTPRouter(t, queries).ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
			if queries.listCalls != test.wantCalls {
				t.Fatalf("ListRegions calls = %d, want %d", queries.listCalls, test.wantCalls)
			}
		})
	}
}

func TestAdminHTTPWorkerGroupTokenRotationIsNeverCached(t *testing.T) {
	queries := &adminHTTPQuerier{admin: true}
	groupID := uuid.Must(uuid.NewV7()).String()
	response := httptest.NewRecorder()
	adminHTTPRouter(t, queries).ServeHTTP(response, adminHTTPRequest(
		http.MethodPost,
		"/admin/api/v1/worker-groups/"+groupID+"/token/rotate",
		"",
	))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", response.Header().Get("Cache-Control"))
	}
	if queries.rotateCalls != 1 || !strings.Contains(response.Body.String(), `"enrollment_token":"hlmr_wgt_`) {
		t.Fatalf("rotation response = %s, calls = %d", response.Body.String(), queries.rotateCalls)
	}
}

func TestAdminHTTPLifecycleConflictUsesConflictResponse(t *testing.T) {
	queries := &adminHTTPQuerier{admin: true, conflict: true}
	groupID := uuid.Must(uuid.NewV7()).String()
	response := httptest.NewRecorder()
	adminHTTPRouter(t, queries).ServeHTTP(response, adminHTTPRequest(
		http.MethodPost,
		"/admin/api/v1/worker-groups/"+groupID+"/pause",
		`{"expected_claim_version":1}`,
	))
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if got := decodeHTTPError(t, response.Body.Bytes()).Code; got != "conflict" {
		t.Fatalf("error code = %q, want conflict", got)
	}
}
