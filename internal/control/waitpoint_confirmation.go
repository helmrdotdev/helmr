package control

import (
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/waitpoint"
)

type waitpointConfirmationView struct {
	TokenID     string
	Token       string
	RunID       string
	WaitpointID string
	TaskID      string
	Kind        db.WaitpointKind
	DisplayText string
	ExpiresAt   time.Time
}

func (s *Server) waitpointConfirmationPage(w http.ResponseWriter, r *http.Request) {
	view, err := s.loadWaitpointConfirmationView(r)
	if err != nil {
		status := http.StatusBadRequest
		if !s.waitpointResponseTokensConfigured() {
			status = http.StatusServiceUnavailable
		}
		if isNoRows(err) {
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
	tokenHash, err := waitpoint.HashResponseToken(s.authSecret, rawToken)
	if err != nil {
		return waitpointConfirmationView{}, err
	}
	token, err := s.db.GetWaitpointResponseTokenForRespond(r.Context(), db.GetWaitpointResponseTokenForRespondParams{
		ID:        ids.ToPG(tokenID),
		TokenHash: tokenHash,
	})
	if err != nil {
		return waitpointConfirmationView{}, err
	}
	return waitpointConfirmationView{
		TokenID:     tokenID.String(),
		Token:       rawToken,
		WaitpointID: ids.MustFromPG(token.WaitpointID).String(),
		Kind:        token.WaitpointKind,
		DisplayText: token.WaitpointDisplayText,
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
	action := "/api/waitpoints/tokens/" + url.PathEscape(view.TokenID) + "/respond"
	tokenInput := `<input type="hidden" name="token" value="` + html.EscapeString(view.Token) + `">`
	if !waitpoint.KindExternallyCompletable(view.Kind) {
		return summary + `<p>This waitpoint type is not supported.</p>`
	}
	return summary + `<form method="post" action="` + action + `">` + tokenInput + `<label>Value <textarea name="value"></textarea></label><button type="submit">Respond</button></form>`
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
