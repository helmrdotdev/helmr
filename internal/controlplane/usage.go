package controlplane

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/session"
)

// getUsageHTTP summarizes agent use over ?range=7d, 30d (the default) or 90d
// with the authority to read the Environment's Sessions.
func (s *Server) getUsageHTTP(w http.ResponseWriter, r *http.Request) {
	days := 30
	if value := r.URL.Query().Get("range"); value != "" {
		parsed, err := strconv.Atoi(strings.TrimSuffix(value, "d"))
		if err != nil || !strings.HasSuffix(value, "d") || !session.UsageRanges[parsed] {
			s.writeAgentHTTPError(w, r, session.ErrInvalidInput)
			return
		}
		days = parsed
	}
	caller, env, err := s.agentRequestAuthority(r, auth.PermissionSessionsRead)
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	usage, err := session.ReadUsage(r.Context(), s.tx, caller, env, days)
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, usageResponse(usage))
}

func usageResponse(u session.Usage) api.Usage {
	response := api.Usage{
		Days:   u.Days,
		Work:   api.UsageWork{Seconds: u.WorkSeconds, PreviousSeconds: u.PreviousWorkSeconds, Turns: u.Turns, PreviousTurns: u.PreviousTurns, Daily: u.DailyWorkSeconds},
		People: api.UsagePeople{Weekly: u.WeeklyPeople, Members: u.Members, Slack: u.SlackPeople, App: u.AppPeople, Daily: u.DailyWeeklyPeople, Channels: make([]api.UsageChannel, 0, len(u.Channels))},
		Runs:   api.UsageRuns{Bounds: session.UsageRunBounds[:], Buckets: u.RunBuckets[:], MedianSeconds: u.MedianRunSeconds, Completed: u.Completed, Unassisted: u.Unassisted},
		Agents: make([]api.UsageAgent, 0, len(u.Agents)),
	}
	for _, c := range u.Channels {
		response.People.Channels = append(response.People.Channels, api.UsageChannel{ID: c.ChannelID, Name: c.Name, People: c.People})
	}
	if l := u.Longest; l != nil {
		response.Runs.Longest = &api.UsageRun{Agent: l.Agent, Seconds: l.Seconds, SessionID: l.SessionID.String(), TurnID: l.TurnID.String()}
	}
	for _, a := range u.Agents {
		response.Agents = append(response.Agents, api.UsageAgent{Agent: a.Agent, Seconds: a.WorkSeconds, People: a.People})
	}
	return response
}
