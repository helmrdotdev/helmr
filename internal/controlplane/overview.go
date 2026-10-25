package controlplane

import (
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/session"
)

// getOverviewHTTP summarizes the Environment's Sessions with the authority to
// read them.
func (s *Server) getOverviewHTTP(w http.ResponseWriter, r *http.Request) {
	caller, env, err := s.agentRequestAuthority(r, auth.PermissionSessionsRead)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	overview, err := session.ReadOverview(r.Context(), s.tx, caller, env)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, overviewResponse(overview))
}

func overviewResponse(o session.Overview) api.Overview {
	response := api.Overview{
		Turns:    api.OverviewTurns{Total: o.Turns, Failed: o.Failed},
		Previous: api.OverviewTurns{Total: o.PreviousTurns, Failed: o.PreviousFailed},
		Agents:   make([]api.OverviewAgent, 0, len(o.Agents)),
		Active:   api.OverviewActive{Count: o.ActiveCount, Running: o.Running, Queued: o.Queued, Held: o.Held, Sessions: make([]api.OverviewSession, 0, len(o.Active))},
		Failures: make([]api.OverviewFailure, 0, len(o.Failures)),
	}
	for _, a := range o.Agents {
		response.Agents = append(response.Agents, api.OverviewAgent{Agent: a.Agent, Active: a.Active, Turns: a.Turns, Failed: a.Failed, Hourly: a.Hourly[:], HourlyFailed: a.HourlyFailed[:]})
	}
	for _, s := range o.Active {
		response.Active.Sessions = append(response.Active.Sessions, api.OverviewSession{ID: s.ID.String(), Agent: s.Agent, State: s.State, Since: s.Since.UTC(), Origin: s.Origin, SlackChannelID: s.SlackChannelID})
	}
	for _, f := range o.Failures {
		response.Failures = append(response.Failures, api.OverviewFailure{Agent: f.Agent, ErrorCode: f.ErrorCode, Count: f.Count, LastHour: f.LastHour, FirstAt: f.FirstAt.UTC(), LastAt: f.LastAt.UTC(), LastSessionID: f.LastSessionID.String(), LastTurnID: f.LastTurnID.String(), LastTurnSequence: f.LastTurnSequence})
	}
	return response
}
