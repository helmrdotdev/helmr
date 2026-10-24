package controlplane

import (
	"errors"
	"net/http"
	"sort"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/slack"
)

// listAttentionHTTP returns what in the Environment is waiting on a person:
// pending Questions, holds on open Sessions and failed Turns (read with the
// authority to read Sessions), and Slack connections that need authorizing
// again (only for callers who manage those Agents).
func (s *Server) listAttentionHTTP(w http.ResponseWriter, r *http.Request) {
	caller, env, err := s.agentRequestAuthority(r, auth.PermissionSessionsRead)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	waiting, err := session.ListAttention(r.Context(), s.tx, caller, env)
	if err != nil && !errors.Is(err, session.ErrDenied) {
		s.writeAgentHTTPError(w, err)
		return
	}
	runsAllowed := err == nil
	response := api.Attention{Items: []api.AttentionItem{}, Counts: api.AttentionCounts{}}
	if runsAllowed {
		response.Counts.Question = &waiting.QuestionCount
		response.Counts.Hold = &waiting.HoldCount
		response.Counts.FailedTurn = &waiting.FailedTurnCount
	}
	for _, item := range waiting.Questions {
		value := attentionItem(api.AttentionQuestion, item)
		value.Prompt = &item.Summary
		response.Items = append(response.Items, value)
	}
	for _, item := range waiting.Holds {
		value := attentionItem(api.AttentionHold, item)
		value.Reason = &item.Summary
		response.Items = append(response.Items, value)
	}
	for _, item := range waiting.FailedTurns {
		value := attentionItem(api.AttentionFailedTurn, item)
		value.TurnSequence = item.TurnSequence
		if item.Summary != "" {
			value.ErrorCode = &item.Summary
		}
		response.Items = append(response.Items, value)
	}
	principal := principalFromContext(r.Context())
	if s.slackConfig != nil && principal.Kind == auth.PrincipalKindSession {
		connections, count, err := slack.ReauthorizationRequired(r.Context(), s.tx, principal.OrgID, principal.UserID, env, session.AttentionLimit)
		switch {
		case slack.IsInstallationDenied(err):
			// Management authority changed since sign-in; the kind stays hidden.
		case err != nil:
			s.writeSlackError(w, err)
			return
		default:
			response.Counts.SlackReauthorization = &count
			for _, connection := range connections {
				response.Items = append(response.Items, api.AttentionItem{Kind: api.AttentionSlackReauthorization, ID: connection.ConnectionID.String(), At: connection.LostAt.UTC(), Agent: connection.Agent, WorkspaceName: connection.WorkspaceName})
			}
		}
	}
	if !runsAllowed && response.Counts.SlackReauthorization == nil {
		s.writeAgentHTTPError(w, session.ErrDenied)
		return
	}
	sort.SliceStable(response.Items, func(i, j int) bool {
		a, b := response.Items[i], response.Items[j]
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.ID < b.ID
	})
	writeJSON(w, http.StatusOK, response)
}

func attentionItem(kind string, item session.AttentionItem) api.AttentionItem {
	sessionID := item.SessionID.String()
	return api.AttentionItem{Kind: kind, ID: item.ID.String(), At: item.At.UTC(), Agent: item.Agent, SessionID: &sessionID, TurnID: agentOptionalID(item.TurnID)}
}
