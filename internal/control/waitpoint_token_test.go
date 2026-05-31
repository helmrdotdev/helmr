package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCreateWaitpointTokenRejectsDelayWaitpoint(t *testing.T) {
	runID := ids.New()
	waitpointID := ids.New()
	store := newWaitpointTokenCreationStore(runID, waitpointID, db.WaitpointKindDelay)
	handler := newWaitpointTokenCreationHandler(store)

	rec := postCreateWaitpointToken(t, handler, api.CreateWaitpointTokenRequest{
		WaitpointID: waitpointID.String(),
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.createdTokens) != 0 {
		t.Fatalf("created tokens = %+v", store.createdTokens)
	}
}

func TestCreateWaitpointTokenCreatesManualWaitpointToken(t *testing.T) {
	runID := ids.New()
	waitpointID := ids.New()
	store := newWaitpointTokenCreationStore(runID, waitpointID, db.WaitpointKindManual)
	handler := newWaitpointTokenCreationHandler(store)

	rec := postCreateWaitpointToken(t, handler, api.CreateWaitpointTokenRequest{
		WaitpointID: waitpointID.String(),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.createdTokens) != 1 {
		t.Fatalf("created tokens = %+v", store.createdTokens)
	}
}

func TestDecodeRespondWaitpointFormTreatsPlainTextAsJSONString(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/respond", strings.NewReader("token=tok&value=looks+good"))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")

	decoded, err := decodeRespondWaitpointRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Token != "tok" || string(decoded.Value) != `"looks good"` {
		t.Fatalf("decoded = %+v value=%s", decoded, decoded.Value)
	}
}

func TestWaitpointResponseHashIgnoresExternalSubject(t *testing.T) {
	first := waitpointResponseRequestHash(json.RawMessage(`{"ok":true}`), "old", json.RawMessage(`{"source":"email"}`))
	second := waitpointResponseRequestHash(json.RawMessage(`{"ok":true}`), "new", json.RawMessage(`{"source":"email"}`))
	if first != second {
		t.Fatalf("hash changed with external subject: %s != %s", first, second)
	}
}

func newWaitpointTokenCreationHandler(store *waitpointTokenCreationStore) http.Handler {
	return New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithUserAuth("01234567890123456789012345678901", "https://helmr.example.test"),
	)
}

func postCreateWaitpointToken(t *testing.T, handler http.Handler, request api.CreateWaitpointTokenRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/tokens", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+auth.APIKeyPrefix+"test-key")
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

type waitpointTokenCreationStore struct {
	db.Querier
	run           db.GetRunSummaryRow
	waitpoint     db.GetWaitpointForResponseTokenCreationRow
	createdTokens []db.CreateWaitpointResponseTokenParams
}

func newWaitpointTokenCreationStore(runID uuid.UUID, waitpointID uuid.UUID, kind db.WaitpointKind) *waitpointTokenCreationStore {
	return &waitpointTokenCreationStore{
		run: db.GetRunSummaryRow{
			ID:               ids.ToPG(runID),
			OrgID:            ids.ToPG(ids.DefaultOrgID),
			ProjectID:        testProjectID(),
			EnvironmentID:    testEnvironmentID(),
			DeploymentID:     testDeploymentID(),
			DeploymentTaskID: testDeploymentTaskID(),
			TaskID:           "deploy-prod",
			Status:           db.RunStatusWaiting,
			CreatedAt:        testTime(),
			UpdatedAt:        testTime(),
		},
		waitpoint: db.GetWaitpointForResponseTokenCreationRow{
			ID:            ids.ToPG(waitpointID),
			OrgID:         ids.ToPG(ids.DefaultOrgID),
			ProjectID:     testProjectID(),
			EnvironmentID: testEnvironmentID(),
			Kind:          kind,
		},
	}
}

func (s *waitpointTokenCreationStore) GetDefaultProjectEnvironment(context.Context, pgtype.UUID) (db.GetDefaultProjectEnvironmentRow, error) {
	return db.GetDefaultProjectEnvironmentRow{
		ProjectID:     testProjectID(),
		EnvironmentID: testEnvironmentID(),
	}, nil
}

func (s *waitpointTokenCreationStore) GetRunSummary(_ context.Context, arg db.GetRunSummaryParams) (db.GetRunSummaryRow, error) {
	if s.run.OrgID != arg.OrgID || s.run.ID != arg.ID {
		return db.GetRunSummaryRow{}, pgx.ErrNoRows
	}
	return s.run, nil
}

func (s *waitpointTokenCreationStore) GetWaitpointForResponseTokenCreation(_ context.Context, arg db.GetWaitpointForResponseTokenCreationParams) (db.GetWaitpointForResponseTokenCreationRow, error) {
	if s.run.OrgID != arg.OrgID || s.waitpoint.ID != arg.WaitpointID {
		return db.GetWaitpointForResponseTokenCreationRow{}, pgx.ErrNoRows
	}
	return s.waitpoint, nil
}

func (s *waitpointTokenCreationStore) CreateWaitpointResponseToken(_ context.Context, arg db.CreateWaitpointResponseTokenParams) (db.WaitpointResponseToken, error) {
	s.createdTokens = append(s.createdTokens, arg)
	return db.WaitpointResponseToken{
		ID:            arg.ID,
		OrgID:         arg.OrgID,
		ProjectID:     s.waitpoint.ProjectID,
		EnvironmentID: s.waitpoint.EnvironmentID,
		WaitpointID:   arg.WaitpointID,
		TokenHash:     arg.TokenHash,
		Status:        db.WaitpointResponseTokenStatusPending,
		ExpiresAt:     arg.ExpiresAt,
		Metadata:      arg.Metadata,
		CreatedAt:     testTime(),
	}, nil
}
