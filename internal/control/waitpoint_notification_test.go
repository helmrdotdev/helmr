package control

import (
	"context"
	"encoding/json"
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
	waitpoint := waitpointView{
		ID:             ids.ToPG(waitpointID),
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		RunID:          ids.ToPG(runID),
		Kind:           db.WaitpointKindApproval,
		DisplayText:    "Approve production deployment?",
		PolicyName:     pgtype.Text{String: "prod-deploy-approval", Valid: true},
		PolicySnapshot: []byte(`{"name":"prod-deploy-approval","label":"Production deploy approval","config":{"deliveries":[{"type":"email","to":["owner@example.test"]}]}}`),
		Status:         db.RunWaitStatusWaiting,
		RequestedAt:    testTime(),
	}
	store := &notificationStore{
		waitpoint: waitpoint,
		tokenID:   ids.ToPG(tokenID),
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
	server.notifyPendingWaitpoint(context.Background(), waitpoint)

	if len(sender.messages) != 0 {
		t.Fatalf("messages were sent synchronously = %+v", sender.messages)
	}
	if len(store.createdDeliveries) != 1 || store.createdDeliveries[0].Status != db.WaitpointDeliveryStatusQueued {
		t.Fatalf("queued deliveries = %+v", store.createdDeliveries)
	}
	if len(store.createdTokens) != 1 || store.sentDeliveries != 0 {
		t.Fatalf("tokens=%+v sent=%d", store.createdTokens, store.sentDeliveries)
	}

	deliveryID := ids.MustFromPG(store.createdDeliveries[0].ID)
	if err := server.SendQueuedWaitpointDelivery(context.Background(), deliveryID); err != nil {
		t.Fatalf("send queued delivery: %v", err)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("messages = %+v", sender.messages)
	}
	message := sender.messages[0]
	if message.To != "owner@example.test" {
		t.Fatalf("recipient = %q", message.To)
	}
	if !strings.HasPrefix(message.IdempotencyKey, "waitpoint-delivery/") {
		t.Fatalf("idempotency key = %q", message.IdempotencyKey)
	}
	if !strings.HasPrefix(message.MessageID, "<waitpoint-delivery-"+deliveryID.String()+"@helmr.example.test>") {
		t.Fatalf("message id = %q", message.MessageID)
	}
	for _, want := range []string{"Helmr waitpoint pending: deploy-prod", "Approve production deployment?", runID.String(), waitpointID.String(), "https://helmr.example.test/waitpoints/respond?", "id=" + deliveryID.String(), "token=hlmr_wpt_"} {
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

func TestSendQueuedWaitpointDeliveryMarksObsoleteDelivery(t *testing.T) {
	store := &notificationStore{}
	sender := &recordingEmailSender{}
	server := &Server{
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:         store,
		mailer:     sender,
		authSecret: []byte("01234567890123456789012345678901"),
		publicURL:  mustParseURL(t, "https://helmr.example.test"),
	}

	if err := server.SendQueuedWaitpointDelivery(context.Background(), ids.New()); err != nil {
		t.Fatalf("send queued delivery: %v", err)
	}
	if store.obsoleteDeliveries != 1 {
		t.Fatalf("obsolete deliveries = %d", store.obsoleteDeliveries)
	}
	if len(sender.messages) != 0 {
		t.Fatalf("messages = %+v", sender.messages)
	}
}

func TestSendQueuedWaitpointDeliveryDoesNotSwallowSupersededSentMark(t *testing.T) {
	runID := ids.New()
	waitpointID := ids.New()
	deliveryID := ids.New()
	tokenID := ids.New()
	store := &notificationStore{
		waitpoint: waitpointView{
			ID:          ids.ToPG(waitpointID),
			OrgID:       ids.ToPG(ids.DefaultOrgID),
			RunID:       ids.ToPG(runID),
			Kind:        db.WaitpointKindApproval,
			DisplayText: "Approve production deployment?",
			Status:      db.RunWaitStatusWaiting,
			RequestedAt: testTime(),
		},
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
		createdDeliveries: []db.WaitpointDelivery{{
			ID:              ids.ToPG(deliveryID),
			OrgID:           ids.ToPG(ids.DefaultOrgID),
			RunID:           ids.ToPG(runID),
			WaitpointID:     ids.ToPG(waitpointID),
			ResponseTokenID: ids.ToPG(tokenID),
			Channel:         "email",
			RecipientKind:   "email",
			Recipient:       "owner@example.test",
			Status:          db.WaitpointDeliveryStatusQueued,
			MessageID:       pgText("<waitpoint-delivery@example.test>"),
			CreatedAt:       testTime(),
			UpdatedAt:       testTime(),
		}},
		markSentErr: pgx.ErrNoRows,
	}
	sender := &recordingEmailSender{}
	server := &Server{
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:         store,
		mailer:     sender,
		authSecret: []byte("01234567890123456789012345678901"),
		publicURL:  mustParseURL(t, "https://helmr.example.test"),
	}

	err := server.SendQueuedWaitpointDelivery(context.Background(), deliveryID)
	if err == nil {
		t.Fatal("send queued delivery error = nil, want superseded claim error")
	}
	if len(sender.messages) != 1 {
		t.Fatalf("messages = %+v", sender.messages)
	}
	if store.sentDeliveries != 0 || store.retriedDeliveries != 1 {
		t.Fatalf("sent=%d retried=%d", store.sentDeliveries, store.retriedDeliveries)
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
		t.Fatalf("completed = %+v recorded = %+v", store.completedTokens, store.recordedResponses)
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
				t.Fatalf("completed = %+v recorded = %+v", store.completedTokens, store.recordedResponses)
			}
		})
	}
}

func TestWaitpointTokenCompleteCompletesTokenWaitpoint(t *testing.T) {
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
			AllowedActions:       []string{"complete"},
			Status:               db.WaitpointResponseTokenStatusPending,
			ExpiresAt:            pgTimeToPG(testTime().Time.Add(time.Hour)),
			Metadata:             []byte(`{"principal":"owner@example.test"}`),
			WaitpointKind:        db.WaitpointKindToken,
			WaitpointDisplayText: "provide payload",
		},
	}
	handler := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithUserAuth("01234567890123456789012345678901", "https://helmr.example.test"),
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/tokens/"+tokenID.String()+"/complete", strings.NewReader(`{"token":"hlmr_wpt_response-token","action":"complete","value":{"ok":true}}`))
	req.Header.Set("content-type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("complete status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.completedTokens) != 1 || store.completedTokens[0].Action != "complete" || store.completedTokens[0].Kind != db.WaitpointKindToken || store.completedTokens[0].ResolutionKind.String != "completed" {
		t.Fatalf("completed = %+v recorded = %+v", store.completedTokens, store.recordedResponses)
	}
	var resolution struct {
		Principal string `json:"principal"`
		Value     struct {
			OK bool `json:"ok"`
		} `json:"value"`
	}
	if err := json.Unmarshal(store.completedTokens[0].Resolution, &resolution); err != nil {
		t.Fatal(err)
	}
	if resolution.Principal != "owner@example.test" || !resolution.Value.OK {
		t.Fatalf("resolution = %s", store.completedTokens[0].Resolution)
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

func TestWaitpointTokenCompletionReturnsAcceptedWhenResolveDoesNotResume(t *testing.T) {
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
		resolveStatus: db.RunWaitStatusWaiting,
	}
	handler := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithUserAuth("01234567890123456789012345678901", "https://helmr.example.test"),
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/tokens/"+tokenID.String()+"/complete", strings.NewReader(`{"token":"hlmr_wpt_response-token","action":"approve"}`))
	req.Header.Set("content-type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("complete status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.completedTokens) != 1 || len(store.resolved) != 1 {
		t.Fatalf("completed = %+v resolved = %+v", store.completedTokens, store.resolved)
	}
}

type notificationStore struct {
	db.Querier
	run                db.GetRunSummaryRow
	waitpoint          waitpointView
	members            []db.ListOrgMembersRow
	tokenID            pgtype.UUID
	activeToken        db.GetActiveWaitpointResponseTokenRow
	createdTokens      []db.CreateWaitpointResponseTokenParams
	createdDeliveries  []db.WaitpointDelivery
	sentDeliveries     int
	retriedDeliveries  int
	obsoleteDeliveries int
	resolved           []db.ResolveWaitpointParams
	resolveStatus      db.RunWaitStatus
	markSentErr        error
	completedTokens    []db.CompleteWaitpointResponseTokenParams
	recordedResponses  []db.RecordWaitpointResponseParams
}

func notificationRunWaitID(waitpoint waitpointView) pgtype.UUID {
	if waitpoint.RunWaitID.Valid {
		return waitpoint.RunWaitID
	}
	return waitpoint.ID
}

func notificationWaitpointRow(waitpoint waitpointView) db.GetWaitpointForDeliveryRow {
	return db.GetWaitpointForDeliveryRow{
		ID:             waitpoint.ID,
		RunWaitID:      notificationRunWaitID(waitpoint),
		OrgID:          waitpoint.OrgID,
		RunID:          waitpoint.RunID,
		ExecutionID:    waitpoint.ExecutionID,
		CheckpointID:   waitpoint.CheckpointID,
		CorrelationID:  waitpoint.CorrelationID,
		Kind:           waitpoint.Kind,
		Request:        waitpoint.Request,
		DisplayText:    waitpoint.DisplayText,
		TimeoutSeconds: waitpoint.TimeoutSeconds,
		PolicyName:     waitpoint.PolicyName,
		PolicySnapshot: waitpoint.PolicySnapshot,
		Status:         waitpoint.Status,
		ResolutionKind: waitpoint.ResolutionKind,
		Resolution:     waitpoint.Resolution,
		CreatedAt:      waitpoint.CreatedAt,
		RequestedAt:    waitpoint.RequestedAt,
		ResolvedAt:     waitpoint.ResolvedAt,
	}
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
		RunWaitID:      notificationRunWaitID(s.waitpoint),
		WaitpointID:    arg.WaitpointID,
		AllowedActions: arg.AllowedActions,
		Status:         db.WaitpointResponseTokenStatusPending,
		ExpiresAt:      arg.ExpiresAt,
		Metadata:       arg.Metadata,
		CreatedAt:      testTime(),
	}, nil
}

func (s *notificationStore) ClaimWaitpointDeliveryForSend(_ context.Context, deliveryID pgtype.UUID) (db.WaitpointDelivery, error) {
	for _, delivery := range s.createdDeliveries {
		if delivery.ID != deliveryID {
			continue
		}
		delivery.Status = db.WaitpointDeliveryStatusSending
		delivery.AttemptCount = 1
		delivery.LastAttemptAt = testTime()
		delivery.SendingStartedAt = testTime()
		return delivery, nil
	}
	return db.WaitpointDelivery{}, pgx.ErrNoRows
}

func (s *notificationStore) GetWaitpointForDelivery(_ context.Context, arg db.GetWaitpointForDeliveryParams) (db.GetWaitpointForDeliveryRow, error) {
	if !s.waitpoint.ID.Valid || s.waitpoint.OrgID != arg.OrgID {
		return db.GetWaitpointForDeliveryRow{}, pgx.ErrNoRows
	}
	return notificationWaitpointRow(s.waitpoint), nil
}

func (s *notificationStore) CreateQueuedWaitpointEmailDelivery(_ context.Context, arg db.CreateQueuedWaitpointEmailDeliveryParams) (db.CreateQueuedWaitpointEmailDeliveryRow, error) {
	s.createdTokens = append(s.createdTokens, db.CreateWaitpointResponseTokenParams{
		ID:              arg.DeliveryID,
		OrgID:           arg.OrgID,
		RunID:           arg.RunID,
		WaitpointID:     arg.WaitpointID,
		TokenHash:       arg.TokenHash,
		AllowedActions:  arg.AllowedActions,
		ExpiresAt:       arg.ExpiresAt,
		ExternalSubject: pgText(arg.Recipient),
		Metadata:        arg.TokenMetadata,
	})
	delivery := db.WaitpointDelivery{
		ID:              arg.DeliveryID,
		OrgID:           arg.OrgID,
		RunID:           arg.RunID,
		RunWaitID:       notificationRunWaitID(s.waitpoint),
		WaitpointID:     arg.WaitpointID,
		ResponseTokenID: arg.DeliveryID,
		Channel:         "email",
		RecipientKind:   "email",
		Recipient:       arg.Recipient,
		Status:          db.WaitpointDeliveryStatusQueued,
		LastAttemptAt:   testTime(),
		MessageID:       arg.MessageID,
		Metadata:        arg.DeliveryMetadata,
		CreatedAt:       testTime(),
		UpdatedAt:       testTime(),
	}
	s.createdDeliveries = append(s.createdDeliveries, delivery)
	return db.CreateQueuedWaitpointEmailDeliveryRow{
		ID: delivery.ID, OrgID: delivery.OrgID, RunID: delivery.RunID, RunWaitID: delivery.RunWaitID, WaitpointID: delivery.WaitpointID,
		ResponseTokenID: delivery.ResponseTokenID, Channel: delivery.Channel, RecipientKind: delivery.RecipientKind,
		Recipient: delivery.Recipient, Status: delivery.Status, AttemptCount: delivery.AttemptCount,
		NextAttemptAt: delivery.NextAttemptAt, LastAttemptAt: delivery.LastAttemptAt,
		SendingStartedAt: delivery.SendingStartedAt, LastError: delivery.LastError, MessageID: arg.MessageID,
		Metadata: delivery.Metadata, SentAt: delivery.SentAt, CreatedAt: delivery.CreatedAt, UpdatedAt: delivery.UpdatedAt,
	}, nil
}

func (s *notificationStore) CreateWaitpointDelivery(_ context.Context, arg db.CreateWaitpointDeliveryParams) (db.WaitpointDelivery, error) {
	delivery := db.WaitpointDelivery{
		ID:              arg.DeliveryID,
		OrgID:           arg.OrgID,
		RunID:           arg.RunID,
		RunWaitID:       arg.RunWaitID,
		WaitpointID:     arg.WaitpointID,
		ResponseTokenID: arg.ResponseTokenID,
		Channel:         arg.Channel,
		RecipientKind:   arg.RecipientKind,
		Recipient:       arg.Recipient,
		Status:          arg.Status,
		Metadata:        arg.Metadata,
		CreatedAt:       testTime(),
		UpdatedAt:       testTime(),
	}
	s.createdDeliveries = append(s.createdDeliveries, delivery)
	return delivery, nil
}

func (s *notificationStore) MarkWaitpointDeliverySent(_ context.Context, arg db.MarkWaitpointDeliverySentParams) (db.WaitpointDelivery, error) {
	if s.markSentErr != nil {
		return db.WaitpointDelivery{}, s.markSentErr
	}
	s.sentDeliveries++
	return db.WaitpointDelivery{
		ID:        arg.DeliveryID,
		OrgID:     arg.OrgID,
		Status:    db.WaitpointDeliveryStatusSent,
		SentAt:    testTime(),
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}, nil
}

func (s *notificationStore) MarkWaitpointDeliveryRetrying(_ context.Context, arg db.MarkWaitpointDeliveryRetryingParams) (db.WaitpointDelivery, error) {
	s.retriedDeliveries++
	return db.WaitpointDelivery{
		ID:        arg.DeliveryID,
		OrgID:     arg.OrgID,
		Status:    db.WaitpointDeliveryStatusRetrying,
		LastError: arg.LastError,
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}, nil
}

func (s *notificationStore) MarkWaitpointDeliveryFailed(_ context.Context, arg db.MarkWaitpointDeliveryFailedParams) (db.WaitpointDelivery, error) {
	return db.WaitpointDelivery{
		ID:        arg.DeliveryID,
		OrgID:     arg.OrgID,
		Status:    db.WaitpointDeliveryStatusFailed,
		LastError: arg.LastError,
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}, nil
}

func (s *notificationStore) MarkObsoleteWaitpointDeliveryFailed(_ context.Context, deliveryID pgtype.UUID) (db.WaitpointDelivery, error) {
	s.obsoleteDeliveries++
	return db.WaitpointDelivery{
		ID:        deliveryID,
		Status:    db.WaitpointDeliveryStatusFailed,
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
	status := s.resolveStatus
	if status == "" {
		status = db.RunWaitStatusRestored
	}
	return db.ResolveWaitpointRow{
		ID:             arg.ID,
		RunWaitID:      notificationRunWaitID(s.waitpoint),
		OrgID:          arg.OrgID,
		RunID:          arg.RunID,
		Kind:           arg.Kind,
		Status:         status,
		ResolutionKind: arg.ResolutionKind,
		Resolution:     arg.Resolution,
		ResolvedAt:     testTime(),
	}, nil
}

func (s *notificationStore) RecordWaitpointResponse(_ context.Context, arg db.RecordWaitpointResponseParams) (db.WaitpointResponse, error) {
	s.recordedResponses = append(s.recordedResponses, arg)
	return db.WaitpointResponse{
		ID: arg.ID, OrgID: arg.OrgID, RunID: arg.RunID, RunWaitID: notificationRunWaitID(s.waitpoint), WaitpointID: arg.WaitpointID,
		ResponseKey: arg.ResponseKey, Action: arg.Action, ResolutionKind: arg.ResolutionKind,
		Resolution: arg.Resolution, EventPayload: arg.EventPayload, CompletedByPrincipal: arg.CompletedByPrincipal,
		CompletedVia: arg.CompletedVia, ExternalSubject: arg.ExternalSubject, Metadata: arg.Metadata,
		CreatedAt: testTime(), UpdatedAt: testTime(),
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

func (s *notificationStore) CompleteAndResolveWaitpointToken(ctx context.Context, complete db.CompleteWaitpointResponseTokenParams, resolve db.ResolveWaitpointParams) (db.ResolveWaitpointRow, error) {
	if _, err := s.CompleteWaitpointResponseToken(ctx, complete); err != nil {
		return db.ResolveWaitpointRow{}, err
	}
	return s.ResolveWaitpoint(ctx, resolve)
}

type recordingEmailSender struct {
	messages []emailMessage
}

func (s *recordingEmailSender) SendEmail(_ context.Context, message emailMessage) error {
	s.messages = append(s.messages, message)
	return nil
}
