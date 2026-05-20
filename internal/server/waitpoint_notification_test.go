package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestNotifyPendingWaitpointSendsConfirmationLink(t *testing.T) {
	runID := ids.New()
	waitpointID := ids.New()
	tokenID := ids.New()
	store := &notificationStore{
		tokenID: ids.ToPG(tokenID),
		run: db.GetRunSummaryRow{
			ID:            ids.ToPG(runID),
			OrgID:         ids.ToPG(ids.DefaultOrgID),
			ProjectID:     testProjectID(),
			EnvironmentID: testEnvironmentID(),
			TaskID:        "deploy-prod",
			Status:        db.RunStatusWaiting,
			CreatedAt:     testTime(),
			UpdatedAt:     testTime(),
		},
		members: []db.ListOrgMembersRow{
			{Role: db.OrgMemberRoleOwner, PrimaryEmail: pgtype.Text{String: "owner@example.test", Valid: true}},
			{Role: db.OrgMemberRoleViewer, PrimaryEmail: pgtype.Text{String: "viewer@example.test", Valid: true}},
		},
	}
	sender := &recordingEmailSender{}
	server := &Server{
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:         store,
		mailer:     sender,
		authSecret: []byte("01234567890123456789012345678901"),
		publicURL:  mustParseURL(t, "https://helmr.example.test"),
	}
	server.notifyPendingWaitpoint(context.Background(), db.Waitpoint{
		ID:             ids.ToPG(waitpointID),
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		RunID:          ids.ToPG(runID),
		Kind:           db.WaitpointKindApproval,
		DisplayText:    "Approve production deployment?",
		PolicyName:     pgtype.Text{String: "prod-deploy-approval", Valid: true},
		PolicySnapshot: []byte(`{"name":"prod-deploy-approval","label":"Production deploy approval","config":{"deliveries":[{"type":"email","to":["owner@example.test"]}],"resolution":{"type":"any","count":1}}}`),
		Status:         db.WaitpointStatusPending,
		RequestedAt:    testTime(),
	})

	if len(sender.messages) != 1 {
		t.Fatalf("messages = %+v", sender.messages)
	}
	message := sender.messages[0]
	if message.To != "owner@example.test" {
		t.Fatalf("recipient = %q", message.To)
	}
	for _, want := range []string{"Helmr waitpoint pending: deploy-prod", "Approve production deployment?", runID.String(), waitpointID.String(), "https://helmr.example.test/waitpoints/respond?", "id=" + tokenID.String(), "token=hlmr_wpt_"} {
		if !strings.Contains(message.Subject+"\n"+message.PlainText, want) {
			t.Fatalf("email missing %q:\nsubject=%s\n%s", want, message.Subject, message.PlainText)
		}
	}
	if len(store.createdTokens) != 1 || store.createdTokens[0].WaitpointID != ids.ToPG(waitpointID) {
		t.Fatalf("created tokens = %+v", store.createdTokens)
	}
	if got := strings.Join(store.createdTokens[0].AllowedActions, ","); got != "approve,deny" {
		t.Fatalf("allowed actions = %q", got)
	}
	if len(store.createdDeliveries) != 1 || store.sentDeliveries != 1 {
		t.Fatalf("deliveries = %+v sent=%d", store.createdDeliveries, store.sentDeliveries)
	}
}

func TestWaitpointConfirmationPageAndFormCompletion(t *testing.T) {
	runID := ids.New()
	waitpointID := ids.New()
	tokenID := ids.New()
	store := &notificationStore{
		tokenID: ids.ToPG(tokenID),
		run: db.GetRunSummaryRow{
			ID:            ids.ToPG(runID),
			OrgID:         ids.ToPG(ids.DefaultOrgID),
			ProjectID:     testProjectID(),
			EnvironmentID: testEnvironmentID(),
			TaskID:        "deploy-prod",
			Status:        db.RunStatusWaiting,
			CreatedAt:     testTime(),
			UpdatedAt:     testTime(),
		},
		activeToken: db.GetActiveWaitpointResponseTokenRow{
			ID:                   ids.ToPG(tokenID),
			OrgID:                ids.ToPG(ids.DefaultOrgID),
			RunID:                ids.ToPG(runID),
			WaitpointID:          ids.ToPG(waitpointID),
			AllowedActions:       []string{"approve", "deny"},
			Status:               db.WaitpointResponseTokenStatusPending,
			ExpiresAt:            pgTimeToPG(testTime().Time.Add(time.Hour)),
			Metadata:             []byte(`{"principal":"owner@example.test"}`),
			WaitpointKind:        db.WaitpointKindApproval,
			WaitpointDisplayText: "Approve production deployment?",
		},
	}
	handler := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithUserAuth("01234567890123456789012345678901", "https://helmr.example.test"),
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waitpoints/respond?id="+tokenID.String()+"&token=hlmr_wpt_response-token", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("page status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`action="/api/waitpoints/tokens/` + tokenID.String() + `/complete"`, `name="action" value="approve"`, `name="action" value="deny"`, "Approve production deployment?"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing %q:\n%s", want, body)
		}
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/waitpoints/tokens/"+tokenID.String()+"/complete", strings.NewReader("token=hlmr_wpt_response-token&action=approve&reason=looks+good"))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "text/html")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("complete status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.completedTokens) != 1 || store.completedTokens[0].ID != ids.ToPG(tokenID) || store.completedTokens[0].ResolutionKind.String != "approved" || store.completedTokens[0].Kind != db.WaitpointKindApproval {
		t.Fatalf("completed = %+v", store.completedTokens)
	}
}

func TestWaitpointConfirmationPageRespectsAllowedActions(t *testing.T) {
	runID := ids.New()
	waitpointID := ids.New()
	tokenID := ids.New()
	store := &notificationStore{
		tokenID: ids.ToPG(tokenID),
		run: db.GetRunSummaryRow{
			ID:            ids.ToPG(runID),
			OrgID:         ids.ToPG(ids.DefaultOrgID),
			ProjectID:     testProjectID(),
			EnvironmentID: testEnvironmentID(),
			TaskID:        "deploy-prod",
			Status:        db.RunStatusWaiting,
			CreatedAt:     testTime(),
			UpdatedAt:     testTime(),
		},
		activeToken: db.GetActiveWaitpointResponseTokenRow{
			ID:                   ids.ToPG(tokenID),
			OrgID:                ids.ToPG(ids.DefaultOrgID),
			RunID:                ids.ToPG(runID),
			WaitpointID:          ids.ToPG(waitpointID),
			AllowedActions:       []string{"approve"},
			Status:               db.WaitpointResponseTokenStatusPending,
			ExpiresAt:            pgTimeToPG(testTime().Time.Add(time.Hour)),
			Metadata:             []byte(`{"principal":"owner@example.test"}`),
			WaitpointKind:        db.WaitpointKindApproval,
			WaitpointDisplayText: "Approve production deployment?",
		},
	}
	handler := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithUserAuth("01234567890123456789012345678901", "https://helmr.example.test"),
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waitpoints/respond?id="+tokenID.String()+"&token=hlmr_wpt_response-token", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("page status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="action" value="approve"`) {
		t.Fatalf("page missing approve action:\n%s", body)
	}
	if strings.Contains(body, `name="action" value="deny"`) {
		t.Fatalf("page rendered disallowed deny action:\n%s", body)
	}
}

func TestWaitpointTokenReplyCompletesMessageToken(t *testing.T) {
	for _, tt := range []struct {
		name           string
		allowedActions []string
	}{
		{name: "message action", allowedActions: []string{"message"}},
		{name: "reply-only action", allowedActions: []string{"reply"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runID := ids.New()
			waitpointID := ids.New()
			tokenID := ids.New()
			store := &notificationStore{
				tokenID: ids.ToPG(tokenID),
				activeToken: db.GetActiveWaitpointResponseTokenRow{
					ID:                   ids.ToPG(tokenID),
					OrgID:                ids.ToPG(ids.DefaultOrgID),
					RunID:                ids.ToPG(runID),
					WaitpointID:          ids.ToPG(waitpointID),
					AllowedActions:       tt.allowedActions,
					Status:               db.WaitpointResponseTokenStatusPending,
					ExpiresAt:            pgTimeToPG(testTime().Time.Add(time.Hour)),
					ExternalSubject:      pgtype.Text{String: "owner@example.test", Valid: true},
					Metadata:             []byte(`{"principal":"owner@example.test"}`),
					WaitpointKind:        db.WaitpointKindMessage,
					WaitpointDisplayText: "Which database should we use?",
				},
			}
			handler := New(
				slog.New(slog.NewTextHandler(io.Discard, nil)),
				WithDB(store),
				WithUserAuth("01234567890123456789012345678901", "https://helmr.example.test"),
			)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/tokens/"+tokenID.String()+"/complete", strings.NewReader(`{"token":"hlmr_wpt_response-token","action":"reply","text":"staging","external_subject":"responder@example.test","metadata":{"source":"sdk"}}`))
			req.Header.Set("content-type", "application/json")
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("complete status = %d body=%s", rec.Code, rec.Body.String())
			}
			if len(store.completedTokens) != 1 || store.completedTokens[0].Action != "message" || store.completedTokens[0].Kind != db.WaitpointKindMessage || store.completedTokens[0].ResolutionKind.String != "replied" || store.completedTokens[0].CompletedByPrincipal.String != "owner@example.test" || store.completedTokens[0].ExternalSubject.String != "owner@example.test" || string(store.completedTokens[0].Metadata) != `{"source":"sdk"}` {
				t.Fatalf("completed = %+v", store.completedTokens)
			}
		})
	}
}

func TestWaitpointTokenCompletionRejectsInvalidMetadata(t *testing.T) {
	runID := ids.New()
	waitpointID := ids.New()
	tokenID := ids.New()
	store := &notificationStore{
		tokenID: ids.ToPG(tokenID),
		activeToken: db.GetActiveWaitpointResponseTokenRow{
			ID:                   ids.ToPG(tokenID),
			OrgID:                ids.ToPG(ids.DefaultOrgID),
			RunID:                ids.ToPG(runID),
			WaitpointID:          ids.ToPG(waitpointID),
			AllowedActions:       []string{"approve"},
			Status:               db.WaitpointResponseTokenStatusPending,
			ExpiresAt:            pgTimeToPG(testTime().Time.Add(time.Hour)),
			Metadata:             []byte(`{"principal":"owner@example.test"}`),
			WaitpointKind:        db.WaitpointKindApproval,
			WaitpointDisplayText: "Approve production deployment?",
		},
	}
	handler := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithUserAuth("01234567890123456789012345678901", "https://helmr.example.test"),
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/tokens/"+tokenID.String()+"/complete", strings.NewReader(`{"token":"hlmr_wpt_response-token","action":"approve","metadata":[]}`))
	req.Header.Set("content-type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("complete status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.completedTokens) != 0 {
		t.Fatalf("completed = %+v", store.completedTokens)
	}
}

func TestWaitpointTokenCompletionUsesRequestSubjectWhenTokenHasNone(t *testing.T) {
	runID := ids.New()
	waitpointID := ids.New()
	tokenID := ids.New()
	store := &notificationStore{
		tokenID: ids.ToPG(tokenID),
		activeToken: db.GetActiveWaitpointResponseTokenRow{
			ID:                   ids.ToPG(tokenID),
			OrgID:                ids.ToPG(ids.DefaultOrgID),
			RunID:                ids.ToPG(runID),
			WaitpointID:          ids.ToPG(waitpointID),
			AllowedActions:       []string{"approve"},
			Status:               db.WaitpointResponseTokenStatusPending,
			ExpiresAt:            pgTimeToPG(testTime().Time.Add(time.Hour)),
			Metadata:             []byte(`{}`),
			WaitpointKind:        db.WaitpointKindApproval,
			WaitpointDisplayText: "Approve production deployment?",
		},
	}
	handler := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithUserAuth("01234567890123456789012345678901", "https://helmr.example.test"),
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/tokens/"+tokenID.String()+"/complete", strings.NewReader(`{"token":"hlmr_wpt_response-token","action":"approve","external_subject":"responder@example.test"}`))
	req.Header.Set("content-type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("complete status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.completedTokens) != 1 || store.completedTokens[0].CompletedByPrincipal.String != "responder@example.test" || store.completedTokens[0].ExternalSubject.String != "responder@example.test" {
		t.Fatalf("completed = %+v", store.completedTokens)
	}
}

type notificationStore struct {
	db.Querier
	run               db.GetRunSummaryRow
	members           []db.ListOrgMembersRow
	tokenID           pgtype.UUID
	activeToken       db.GetActiveWaitpointResponseTokenRow
	createdTokens     []db.CreateWaitpointResponseTokenParams
	createdDeliveries []db.CreateWaitpointDeliveryParams
	sentDeliveries    int
	resolved          []db.ResolveWaitpointParams
	completedTokens   []db.CompleteWaitpointResponseTokenParams
}

func (s *notificationStore) GetRunSummary(context.Context, db.GetRunSummaryParams) (db.GetRunSummaryRow, error) {
	if !s.run.ID.Valid {
		return db.GetRunSummaryRow{}, pgx.ErrNoRows
	}
	return s.run, nil
}

func (s *notificationStore) ListOrgMembers(context.Context, pgtype.UUID) ([]db.ListOrgMembersRow, error) {
	return s.members, nil
}

func (s *notificationStore) CreateWaitpointResponseToken(_ context.Context, arg db.CreateWaitpointResponseTokenParams) (db.WaitpointResponseToken, error) {
	s.createdTokens = append(s.createdTokens, arg)
	id := arg.ID
	if s.tokenID.Valid {
		id = s.tokenID
	}
	return db.WaitpointResponseToken{
		ID:             id,
		OrgID:          arg.OrgID,
		RunID:          arg.RunID,
		WaitpointID:    arg.WaitpointID,
		AllowedActions: arg.AllowedActions,
		Status:         db.WaitpointResponseTokenStatusPending,
		ExpiresAt:      arg.ExpiresAt,
		Metadata:       arg.Metadata,
		CreatedAt:      testTime(),
	}, nil
}

func (s *notificationStore) CreateWaitpointDelivery(_ context.Context, arg db.CreateWaitpointDeliveryParams) (db.WaitpointDelivery, error) {
	s.createdDeliveries = append(s.createdDeliveries, arg)
	return db.WaitpointDelivery{
		ID:              arg.ID,
		OrgID:           arg.OrgID,
		RunID:           arg.RunID,
		WaitpointID:     arg.WaitpointID,
		ResponseTokenID: arg.ResponseTokenID,
		Channel:         arg.Channel,
		RecipientKind:   arg.RecipientKind,
		Recipient:       arg.Recipient,
		Status:          arg.Status,
		Metadata:        arg.Metadata,
		CreatedAt:       testTime(),
		UpdatedAt:       testTime(),
	}, nil
}

func (s *notificationStore) MarkWaitpointDeliverySent(_ context.Context, arg db.MarkWaitpointDeliverySentParams) (db.WaitpointDelivery, error) {
	s.sentDeliveries++
	return db.WaitpointDelivery{
		ID:        arg.ID,
		OrgID:     arg.OrgID,
		Status:    db.WaitpointDeliveryStatusSent,
		SentAt:    testTime(),
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}, nil
}

func (s *notificationStore) MarkWaitpointDeliveryFailed(_ context.Context, arg db.MarkWaitpointDeliveryFailedParams) (db.WaitpointDelivery, error) {
	return db.WaitpointDelivery{
		ID:        arg.ID,
		OrgID:     arg.OrgID,
		Status:    db.WaitpointDeliveryStatusFailed,
		LastError: arg.LastError,
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}, nil
}

func (s *notificationStore) GetActiveWaitpointResponseToken(_ context.Context, arg db.GetActiveWaitpointResponseTokenParams) (db.GetActiveWaitpointResponseTokenRow, error) {
	if s.tokenID.Valid && arg.ID != s.tokenID {
		return db.GetActiveWaitpointResponseTokenRow{}, pgx.ErrNoRows
	}
	return s.activeToken, nil
}

func (s *notificationStore) ResolveWaitpoint(_ context.Context, arg db.ResolveWaitpointParams) (db.ResolveWaitpointRow, error) {
	s.resolved = append(s.resolved, arg)
	return db.ResolveWaitpointRow{
		ID:             arg.ID,
		OrgID:          arg.OrgID,
		RunID:          arg.RunID,
		Kind:           arg.Kind,
		Status:         db.WaitpointStatusResolved,
		ResolutionKind: arg.ResolutionKind,
		Resolution:     arg.Resolution,
		ResolvedAt:     testTime(),
	}, nil
}

func (s *notificationStore) CompleteWaitpointResponseToken(_ context.Context, arg db.CompleteWaitpointResponseTokenParams) (db.CompleteWaitpointResponseTokenRow, error) {
	if s.tokenID.Valid && arg.ID != s.tokenID {
		return db.CompleteWaitpointResponseTokenRow{}, pgx.ErrNoRows
	}
	if s.activeToken.WaitpointKind != "" && arg.Kind != s.activeToken.WaitpointKind {
		return db.CompleteWaitpointResponseTokenRow{}, pgx.ErrNoRows
	}
	if len(s.activeToken.AllowedActions) > 0 && !waitpointTokenAllows(s.activeToken.AllowedActions, api.WaitpointTokenAction(arg.Action)) {
		return db.CompleteWaitpointResponseTokenRow{}, pgx.ErrNoRows
	}
	s.completedTokens = append(s.completedTokens, arg)
	return db.CompleteWaitpointResponseTokenRow{ID: arg.ID, Status: db.WaitpointResponseTokenStatusCompleted}, nil
}

type recordingEmailSender struct {
	messages []emailMessage
}

func (s *recordingEmailSender) SendEmail(_ context.Context, message emailMessage) error {
	s.messages = append(s.messages, message)
	return nil
}
