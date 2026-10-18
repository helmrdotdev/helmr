package main

import (
	"context"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
)

func collectDiagnosticAgents(ctx context.Context, cp *client.Client, scope client.EnvironmentScopeOptions, report *diagnosticReport) {
	agents := []string{}
	cursor := ""
	for {
		page, err := cp.ListAgentDefinitions(ctx, client.DefinitionListOptions{EnvironmentScopeOptions: scope, DeploymentID: report.Live.Deployment.ID, Cursor: cursor, Limit: 100})
		if err != nil {
			report.readError("agents.registered", err)
			return
		}
		for _, agent := range page.AgentDefinitions {
			agents = append(agents, agent.ID)
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			report.check("agents.registered", "error", "Server repeated an Agent page cursor.", nil)
			return
		}
		cursor = page.NextCursor
	}
	report.Live.Agents = agents
	report.check("agents.registered", "pass", fmt.Sprintf("%d Agents in the selected deployment.", len(report.Live.Agents)), nil)
}

func collectDiagnosticRecentTurns(ctx context.Context, cp *client.Client, scope client.EnvironmentScopeOptions, sessions []api.AgentSession, report *diagnosticReport) {
	var failed, holds int64
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, s := range sessions {
		if s.Status != "open" {
			continue
		}
		var latest *api.AgentTurn
		holds += int64(len(s.Holds))
		cursor := ""
		for {
			page, err := cp.ListSessionTurns(ctx, s.ID, client.SessionTurnListOptions{EnvironmentScopeOptions: scope, Cursor: cursor, Limit: 100})
			if err != nil {
				report.readError("turns.recent", err)
				return
			}
			for _, turn := range page.Turns {
				if latest == nil || turn.Sequence > latest.Sequence {
					latest = &turn
				}
			}
			if page.NextCursor == "" {
				break
			}
			if page.NextCursor == cursor {
				report.check("turns.recent", "error", "Server repeated a Turn page cursor.", nil)
				return
			}
			cursor = page.NextCursor
		}
		if latest != nil && latest.Status == "failed" && latest.TerminalAt != nil && latest.TerminalAt.After(cutoff) {
			failed++
		}
	}
	diagnosticCount(report, "sessions.holds", holds, "holds in the latest 10 Sessions", "Inspect held Sessions and resolve their hold reasons.", true)
	diagnosticCount(report, "turns.failed", failed, "latest failed Turns within 24 hours in open Sessions among the latest 10 Sessions", "Read the affected Session events before retrying.", false)
}

func collectDiagnosticSlack(ctx context.Context, cp *client.Client, scope client.EnvironmentScopeOptions, doctor bool, report *diagnosticReport) {
	teams := map[string]bool{}
	agents := []string{}
	cursor := ""
	for {
		page, err := cp.ListRetainedAgents(ctx, scope, cursor)
		if err != nil {
			report.readError("slack.connections", err)
			return
		}
		for _, agent := range page.Agents {
			agents = append(agents, agent.Agent)
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			report.check("slack.connections", "error", "Server repeated a retained Agent page cursor.", nil)
			return
		}
		cursor = page.NextCursor
	}
	connections := []diagnosticSlack{}
	complete := true
	for _, agent := range agents {
		connection, err := cp.GetAgentSlackConnection(ctx, agent, scope)
		if err != nil {
			report.readError("slack.connection."+agent, err)
			complete = false
			continue
		}
		connections = append(connections, diagnosticSlack{agent, connection})
		if connection == nil {
			continue
		}
		if connection.TeamID != nil {
			teams[*connection.TeamID] = true
		}
		if doctor {
			status := "pass"
			var remedy *diagnosticRemedy
			switch connection.Status {
			case "connected":
			case "setup_incomplete", "disconnected", "reauthorization_required":
				status = "fail"
				remedy = &diagnosticRemedy{"Complete or repair this existing Agent Slack connection in the Console.", true}
			default:
				status = "error"
				remedy = &diagnosticRemedy{"Inspect this Agent's Slack connection in the Console; its status is not recognized by this CLI.", true}
			}
			report.check("slack.connection."+agent, status, "Existing connection status: "+connection.Status, remedy)
		}
	}
	if complete {
		report.Live.Slack = connections
	}
	if !doctor {
		report.check("slack.identity", "skip", "Caller identity links are checked by doctor.", nil)
		return
	}
	if len(teams) == 0 {
		report.check("slack.identity", "skip", "No connected workspace requiring a caller identity check.", nil)
		return
	}
	cursor = ""
	for {
		page, err := cp.GetSlackUserLinks(ctx, cursor)
		if err != nil {
			report.readError("slack.identity", err)
			return
		}
		for _, link := range page.Links {
			delete(teams, link.TeamID)
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			report.check("slack.identity", "error", "Server repeated a Slack identity page cursor.", nil)
			return
		}
		cursor = page.NextCursor
	}
	if len(teams) > 0 {
		report.check("slack.identity", "skip", fmt.Sprintf("Caller is not linked to %d connected Slack workspaces; a personal link is optional until the caller uses an Agent there.", len(teams)), &diagnosticRemedy{"When you want to use an Agent in Slack, mention it and follow its account-link prompt, then mention it again.", true})
	} else {
		report.check("slack.identity", "pass", "Caller has an identity link for each inspected connected workspace.", nil)
	}
}
