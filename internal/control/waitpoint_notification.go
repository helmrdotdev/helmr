package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type waitpointConfirmationView struct {
	TokenID     string
	Token       string
	RunID       string
	WaitpointID string
	TaskID      string
	Kind        db.WaitpointKind
	DisplayText string
	Actions     []string
	ExpiresAt   time.Time
}

func (s *Server) emailDeliveryConfigured() bool {
	switch s.mailer.(type) {
	case nil, unconfiguredEmailSender, legacyMagicLinkEmailSender:
		return false
	default:
		return true
	}
}

func (s *Server) waitpointResponseTokensConfigured() bool {
	return s.db != nil && auth.ValidateTokenSecret(s.authSecret) == nil
}

func (s *Server) notifyPendingWaitpoint(ctx context.Context, waitpoint db.Waitpoint) {
	_, config, ok, err := waitpointPolicyFromSnapshot(waitpoint)
	if err != nil {
		s.log.Warn("parse waitpoint policy failed", "run_id", ids.MustFromPG(waitpoint.RunID).String(), "waitpoint_id", ids.MustFromPG(waitpoint.ID).String(), "error", err)
		return
	}
	if !ok {
		return
	}
	recipients := waitpointPolicyEmailRecipients(config)
	if len(recipients) == 0 {
		return
	}
	if !s.emailDeliveryConfigured() {
		s.createFailedWaitpointEmailDeliveries(ctx, waitpoint, recipients, "email delivery is not configured")
		return
	}
	if !s.waitpointResponseTokensConfigured() {
		s.log.Warn("skip waitpoint email notification: response token API is not configured", "run_id", ids.MustFromPG(waitpoint.RunID).String(), "waitpoint_id", ids.MustFromPG(waitpoint.ID).String())
		s.createFailedWaitpointEmailDeliveries(ctx, waitpoint, recipients, "response token API is not configured")
		return
	}
	run, err := s.db.GetRunSummary(ctx, db.GetRunSummaryParams{OrgID: waitpoint.OrgID, ID: waitpoint.RunID})
	if err != nil {
		s.log.Warn("load run for waitpoint notification failed", "run_id", ids.MustFromPG(waitpoint.RunID).String(), "error", err)
		return
	}
	for _, recipient := range recipients {
		token, err := s.createWaitpointEmailResponseToken(ctx, waitpoint, recipient)
		if err != nil {
			s.log.Warn("create waitpoint response token failed", "run_id", ids.MustFromPG(waitpoint.RunID).String(), "waitpoint_id", ids.MustFromPG(waitpoint.ID).String(), "recipient", recipient, "error", err)
			s.createFailedWaitpointEmailDelivery(ctx, waitpoint, pgtype.UUID{}, recipient, err.Error())
			continue
		}
		delivery, err := s.createWaitpointEmailDelivery(ctx, waitpoint, token.ID, recipient, db.WaitpointDeliveryStatusQueued, "")
		if err != nil {
			s.log.Warn("create waitpoint delivery failed", "run_id", ids.MustFromPG(waitpoint.RunID).String(), "waitpoint_id", ids.MustFromPG(waitpoint.ID).String(), "recipient", recipient, "error", err)
			continue
		}
		link, err := s.waitpointConfirmationURL(ids.MustFromPG(token.ID).String(), token.Raw)
		if err != nil {
			s.log.Warn("build waitpoint confirmation URL failed", "error", err)
			s.markWaitpointDeliveryFailed(ctx, delivery.ID, waitpoint.OrgID, err.Error())
			continue
		}
		message := waitpointNotificationEmail(recipient, getRunSummary(run), waitpoint, link)
		message.IdempotencyKey = "waitpoint-delivery/" + ids.MustFromPG(delivery.ID).String()
		if err := s.mailer.SendEmail(ctx, message); err != nil {
			s.log.Warn("send waitpoint notification failed", "run_id", ids.MustFromPG(waitpoint.RunID).String(), "waitpoint_id", ids.MustFromPG(waitpoint.ID).String(), "recipient", recipient, "error", err)
			s.markWaitpointDeliveryFailed(ctx, delivery.ID, waitpoint.OrgID, err.Error())
			continue
		}
		if _, err := s.db.MarkWaitpointDeliverySent(ctx, db.MarkWaitpointDeliverySentParams{OrgID: waitpoint.OrgID, ID: delivery.ID}); err != nil {
			s.log.Warn("mark waitpoint delivery sent failed", "delivery_id", ids.MustFromPG(delivery.ID).String(), "error", err)
		}
	}
}

type waitpointEmailResponseToken struct {
	ID  pgtype.UUID
	Raw string
}

func (s *Server) createWaitpointEmailResponseToken(ctx context.Context, waitpoint db.Waitpoint, recipient string) (waitpointEmailResponseToken, error) {
	rawToken, tokenHash, err := s.generateWaitpointResponseToken()
	if err != nil {
		return waitpointEmailResponseToken{}, err
	}
	actions, err := waitpointTokenActionsForKind(waitpoint.Kind)
	if err != nil {
		return waitpointEmailResponseToken{}, err
	}
	metadata, err := json.Marshal(map[string]any{
		"source":    "email",
		"recipient": recipient,
		"principal": recipient,
	})
	if err != nil {
		return waitpointEmailResponseToken{}, err
	}
	row, err := s.db.CreateWaitpointResponseToken(ctx, db.CreateWaitpointResponseTokenParams{
		ID:              ids.ToPG(ids.New()),
		OrgID:           waitpoint.OrgID,
		RunID:           waitpoint.RunID,
		WaitpointID:     waitpoint.ID,
		TokenHash:       tokenHash,
		AllowedActions:  actions,
		ExpiresAt:       pgTimeToPG(time.Now().UTC().Add(defaultWaitpointResponseTokenTTL)),
		ExternalSubject: pgText(recipient),
		Metadata:        metadata,
	})
	if err != nil {
		return waitpointEmailResponseToken{}, err
	}
	return waitpointEmailResponseToken{ID: row.ID, Raw: rawToken}, nil
}

func (s *Server) createFailedWaitpointEmailDeliveries(ctx context.Context, waitpoint db.Waitpoint, recipients []string, reason string) {
	for _, recipient := range recipients {
		s.createFailedWaitpointEmailDelivery(ctx, waitpoint, pgtype.UUID{}, recipient, reason)
	}
}

func (s *Server) createFailedWaitpointEmailDelivery(ctx context.Context, waitpoint db.Waitpoint, tokenID pgtype.UUID, recipient string, reason string) {
	if _, err := s.createWaitpointEmailDelivery(ctx, waitpoint, tokenID, recipient, db.WaitpointDeliveryStatusFailed, reason); err != nil {
		s.log.Warn("create failed waitpoint delivery failed", "run_id", ids.MustFromPG(waitpoint.RunID).String(), "waitpoint_id", ids.MustFromPG(waitpoint.ID).String(), "recipient", recipient, "error", err)
	}
}

func (s *Server) createWaitpointEmailDelivery(ctx context.Context, waitpoint db.Waitpoint, tokenID pgtype.UUID, recipient string, status db.WaitpointDeliveryStatus, lastError string) (db.WaitpointDelivery, error) {
	metadata, err := json.Marshal(map[string]any{
		"source": "policy",
	})
	if err != nil {
		return db.WaitpointDelivery{}, err
	}
	delivery, err := s.db.CreateWaitpointDelivery(ctx, db.CreateWaitpointDeliveryParams{
		ID:              ids.ToPG(ids.New()),
		OrgID:           waitpoint.OrgID,
		RunID:           waitpoint.RunID,
		WaitpointID:     waitpoint.ID,
		ResponseTokenID: tokenID,
		Channel:         "email",
		RecipientKind:   "email",
		Recipient:       recipient,
		Status:          status,
		Metadata:        metadata,
	})
	if err != nil {
		return db.WaitpointDelivery{}, err
	}
	if lastError != "" {
		return s.db.MarkWaitpointDeliveryFailed(ctx, db.MarkWaitpointDeliveryFailedParams{
			OrgID:     waitpoint.OrgID,
			ID:        delivery.ID,
			LastError: pgText(lastError),
		})
	}
	return delivery, nil
}

func (s *Server) markWaitpointDeliveryFailed(ctx context.Context, deliveryID pgtype.UUID, orgID pgtype.UUID, reason string) {
	if _, err := s.db.MarkWaitpointDeliveryFailed(ctx, db.MarkWaitpointDeliveryFailedParams{OrgID: orgID, ID: deliveryID, LastError: pgText(reason)}); err != nil {
		s.log.Warn("mark waitpoint delivery failed failed", "delivery_id", ids.MustFromPG(deliveryID).String(), "error", err)
	}
}

func waitpointPolicyFromSnapshot(waitpoint db.Waitpoint) (resolvedWaitpointPolicy, api.WaitpointPolicyConfig, bool, error) {
	if len(waitpoint.PolicySnapshot) == 0 {
		return resolvedWaitpointPolicy{}, api.WaitpointPolicyConfig{}, false, nil
	}
	var policy resolvedWaitpointPolicy
	if err := json.Unmarshal(waitpoint.PolicySnapshot, &policy); err != nil {
		return resolvedWaitpointPolicy{}, api.WaitpointPolicyConfig{}, false, err
	}
	var config api.WaitpointPolicyConfig
	if len(policy.Config) > 0 {
		if err := json.Unmarshal(policy.Config, &config); err != nil {
			return resolvedWaitpointPolicy{}, api.WaitpointPolicyConfig{}, false, err
		}
	}
	return policy, config, true, nil
}

func waitpointTokenActionsForKind(kind db.WaitpointKind) ([]string, error) {
	switch kind {
	case db.WaitpointKindApproval:
		return []string{string(api.WaitpointTokenActionApprove), string(api.WaitpointTokenActionDeny)}, nil
	case db.WaitpointKindMessage:
		return []string{string(api.WaitpointTokenActionMessage)}, nil
	default:
		return nil, fmt.Errorf("unsupported waitpoint kind %q", kind)
	}
}

func waitpointNotificationEmail(to string, run runSummary, waitpoint db.Waitpoint, link string) emailMessage {
	runID := ids.MustFromPG(run.ID).String()
	waitpointID := ids.MustFromPG(waitpoint.ID).String()
	body := fmt.Sprintf(
		"A Helmr run is waiting for input.\n\nTask: %s\nRun: %s\nWaitpoint: %s\nType: %s\nRequested: %s\n\n%s\n\nReview and respond here:\n%s\n\nThis link opens a confirmation page before submitting a response.\n",
		run.TaskID,
		runID,
		waitpointID,
		waitpoint.Kind,
		pgTime(waitpoint.RequestedAt).Format(time.RFC3339),
		waitpoint.DisplayText,
		link,
	)
	return emailMessage{
		To:        to,
		Subject:   "Helmr waitpoint pending: " + run.TaskID,
		PlainText: body,
	}
}

func (s *Server) waitpointConfirmationURL(tokenID string, token string) (string, error) {
	if s.publicURL == nil {
		return "", errors.New("public URL is not configured")
	}
	confirmation := s.publicURL.ResolveReference(&url.URL{Path: "/waitpoints/respond"})
	query := confirmation.Query()
	query.Set("id", tokenID)
	query.Set("token", token)
	confirmation.RawQuery = query.Encode()
	return confirmation.String(), nil
}

func waitpointConfirmationPath(tokenID string, token string) string {
	confirmation := url.URL{Path: "/waitpoints/respond"}
	query := confirmation.Query()
	query.Set("id", tokenID)
	query.Set("token", token)
	confirmation.RawQuery = query.Encode()
	return confirmation.String()
}

func (s *Server) waitpointConfirmationPage(w http.ResponseWriter, r *http.Request) {
	view, err := s.loadWaitpointConfirmationView(r)
	if err != nil {
		status := http.StatusBadRequest
		if !s.waitpointResponseTokensConfigured() {
			status = http.StatusServiceUnavailable
		}
		if errors.Is(err, pgx.ErrNoRows) {
			status = http.StatusBadRequest
		}
		writeWaitpointHTML(w, status, "Invalid link", "<p>This waitpoint link is no longer valid.</p>")
		return
	}
	writeWaitpointHTML(w, http.StatusOK, "Respond to waitpoint", waitpointConfirmationBody(view))
}

func (s *Server) loadWaitpointConfirmationView(r *http.Request) (waitpointConfirmationView, error) {
	if !s.waitpointResponseTokensConfigured() {
		return waitpointConfirmationView{}, errors.New("waitpoint response tokens are not configured")
	}
	tokenID, err := ids.Parse(strings.TrimSpace(r.URL.Query().Get("id")))
	if err != nil {
		return waitpointConfirmationView{}, errors.New("id must be a UUID")
	}
	rawToken := strings.TrimSpace(r.URL.Query().Get("token"))
	tokenHash, err := s.hashWaitpointResponseToken(rawToken)
	if err != nil {
		return waitpointConfirmationView{}, err
	}
	token, err := s.db.GetActiveWaitpointResponseToken(r.Context(), db.GetActiveWaitpointResponseTokenParams{
		ID:        ids.ToPG(tokenID),
		TokenHash: tokenHash,
	})
	if err != nil {
		return waitpointConfirmationView{}, err
	}
	taskID := ""
	run, err := s.db.GetRunSummary(r.Context(), db.GetRunSummaryParams{OrgID: token.OrgID, ID: token.RunID})
	if err == nil {
		taskID = run.TaskID
	}
	return waitpointConfirmationView{
		TokenID:     tokenID.String(),
		Token:       rawToken,
		RunID:       ids.MustFromPG(token.RunID).String(),
		WaitpointID: ids.MustFromPG(token.WaitpointID).String(),
		TaskID:      taskID,
		Kind:        token.WaitpointKind,
		DisplayText: token.WaitpointDisplayText,
		Actions:     token.AllowedActions,
		ExpiresAt:   pgTime(token.ExpiresAt),
	}, nil
}

func waitpointConfirmationBody(view waitpointConfirmationView) string {
	summary := fmt.Sprintf(
		"<dl><dt>Task</dt><dd>%s</dd><dt>Run</dt><dd>%s</dd><dt>Waitpoint</dt><dd>%s</dd><dt>Request</dt><dd>%s</dd></dl>",
		html.EscapeString(view.TaskID),
		html.EscapeString(view.RunID),
		html.EscapeString(view.WaitpointID),
		html.EscapeString(view.DisplayText),
	)
	action := "/api/waitpoints/tokens/" + url.PathEscape(view.TokenID) + "/complete"
	tokenInput := `<input type="hidden" name="token" value="` + html.EscapeString(view.Token) + `">`
	switch view.Kind {
	case db.WaitpointKindApproval:
		body := summary
		if waitpointConfirmationAllows(view.Actions, api.WaitpointTokenActionApprove) {
			body += `<form method="post" action="` + action + `">` + tokenInput + `<input type="hidden" name="action" value="approve"><label>Reason <input name="reason"></label><button type="submit">Approve</button></form>`
		}
		if waitpointConfirmationAllows(view.Actions, api.WaitpointTokenActionDeny) {
			body += `<form method="post" action="` + action + `">` + tokenInput + `<input type="hidden" name="action" value="deny"><label>Reason <input name="reason"></label><button type="submit">Deny</button></form>`
		}
		if body == summary {
			body += `<p>This waitpoint link does not allow any approval actions.</p>`
		}
		return body
	case db.WaitpointKindMessage:
		if !waitpointConfirmationAllows(view.Actions, api.WaitpointTokenActionMessage) {
			return summary + `<p>This waitpoint link does not allow message replies.</p>`
		}
		return summary + `<form method="post" action="` + action + `">` + tokenInput + `<input type="hidden" name="action" value="message"><label>Message <textarea name="text" required></textarea></label><button type="submit">Send</button></form>`
	default:
		return summary + `<p>This waitpoint type is not supported.</p>`
	}
}

func waitpointConfirmationAllows(actions []string, action api.WaitpointTokenAction) bool {
	return waitpointTokenAllows(actions, action)
}

func acceptsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("accept"), "text/html") || strings.Contains(r.Header.Get("content-type"), "application/x-www-form-urlencoded")
}

func writeWaitpointHTML(w http.ResponseWriter, status int, title string, body string) {
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>%s</title><style>body{font-family:system-ui,sans-serif;margin:2rem;max-width:42rem;color:#111827}dt{font-size:.75rem;text-transform:uppercase;color:#6b7280;margin-top:1rem}dd{margin:.25rem 0 0}form{margin-top:1rem;padding-top:1rem;border-top:1px solid #e5e7eb}label{display:block;margin-bottom:.75rem}input,textarea{display:block;width:100%%;box-sizing:border-box;margin-top:.25rem;padding:.5rem;border:1px solid #d1d5db}button{padding:.55rem .85rem;border:1px solid #111827;background:#111827;color:white}</style></head><body><h1>%s</h1>%s</body></html>`, html.EscapeString(title), html.EscapeString(title), body)
}
