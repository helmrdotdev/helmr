package controlplane

import (
	"encoding/json"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
)

func assertDefinitionCLIHTTP(t *testing.T, serverURL, key, deployment string) {
	t.Helper()
	cp, err := client.New(serverURL, client.WithBearerToken(key))
	if err != nil {
		t.Fatal(err)
	}
	page, err := cp.ListAgents(t.Context(), client.DefinitionListOptions{DeploymentID: deployment, Limit: 1})
	if err != nil || len(page.Agents) != 1 || page.Agents[0].ID != "agent" || page.NextCursor == "" {
		t.Fatalf("Go catalog=%+v err=%v", page, err)
	}
	next, err := cp.ListAgents(t.Context(), client.DefinitionListOptions{Cursor: page.NextCursor})
	if err != nil || len(next.Agents) != 1 || next.Agents[0].ID != "zebra" || next.DeploymentID != deployment {
		t.Fatalf("Go catalog continuation=%+v err=%v", next, err)
	}
	item, err := cp.GetAgent(t.Context(), "agent", client.DefinitionGetOptions{DeploymentID: deployment})
	if err != nil || item.ID != "agent" || item.DeploymentID != deployment {
		t.Fatalf("Go definition=%+v err=%v", item, err)
	}
	computers, err := cp.ListComputerDefinitions(t.Context(), client.DefinitionListOptions{DeploymentID: deployment})
	if err != nil || len(computers.ComputerDefinitions) != 1 {
		t.Fatalf("Go Computer definitions=%+v err=%v", computers, err)
	}
	computer, err := cp.GetComputerDefinition(t.Context(), "fixture-computer", client.DefinitionGetOptions{DeploymentID: deployment})
	if err != nil || computer.ID != "fixture-computer" {
		t.Fatalf("Go Computer definition=%+v err=%v", computer, err)
	}
	run := newAgentTestCLI(t, serverURL, key)
	if err := json.Unmarshal(run("agent", "list", "--limit", "1", "--deployment", deployment, "--json"), &page); err != nil || page.NextCursor == "" {
		t.Fatalf("CLI catalog=%+v err=%v", page, err)
	}
	if err := json.Unmarshal(run("agent", "get", "agent", "--deployment", deployment, "--json"), &item); err != nil || item.DeploymentID != deployment {
		t.Fatalf("CLI definition=%+v err=%v", item, err)
	}
	if err := json.Unmarshal(run("computer", "definition", "list", "--deployment", deployment, "--json"), &computers); err != nil || len(computers.ComputerDefinitions) != 1 {
		t.Fatalf("CLI Computer catalog=%+v err=%v", computers, err)
	}
	if err := json.Unmarshal(run("computer", "definition", "get", "fixture-computer", "--deployment", deployment, "--json"), &computer); err != nil || computer.ID != "fixture-computer" {
		t.Fatalf("CLI Computer definition=%+v err=%v", computer, err)
	}
}

func assertAskCLIHTTP(t *testing.T, serverURL, key, session, turn, goAsk, cliAsk string) {
	t.Helper()
	cp, err := client.New(serverURL, client.WithBearerToken(key))
	if err != nil {
		t.Fatal(err)
	}
	page, err := cp.ListTurnAsks(t.Context(), session, turn, client.AskListOptions{Limit: 1})
	if err != nil || len(page.Asks) != 1 || page.NextCursor == "" {
		t.Fatalf("Go asks=%+v err=%v", page, err)
	}
	next, err := cp.ListTurnAsks(t.Context(), session, turn, client.AskListOptions{Limit: 1, Cursor: page.NextCursor})
	if err != nil || len(next.Asks) != 1 || next.Asks[0].ID == page.Asks[0].ID {
		t.Fatalf("Go ask continuation=%+v err=%v", next, err)
	}
	ask, err := cp.GetTurnAsk(t.Context(), session, turn, goAsk, client.EnvironmentScopeOptions{})
	if err != nil || ask.ID != goAsk || ask.Status != "pending" || len(ask.Prompt) == 0 || len(ask.AnswerControl) == 0 {
		t.Fatalf("Go ask=%+v err=%v", ask, err)
	}
	request := api.RespondAgentAskRequest{Answer: json.RawMessage(`""`), ResponseID: "go-response"}
	receipt, err := cp.RespondTurnAsk(t.Context(), session, turn, goAsk, request, client.EnvironmentScopeOptions{})
	if err != nil || receipt.Status != "responded" || string(receipt.Answer) != `""` || receipt.RespondedByAPIKeyID == nil {
		t.Fatalf("Go answer=%+v err=%v", receipt, err)
	}
	retry, err := cp.RespondTurnAsk(t.Context(), session, turn, goAsk, request, client.EnvironmentScopeOptions{})
	if err != nil || retry.ID != receipt.ID || retry.RespondedAt == nil || !retry.RespondedAt.Equal(*receipt.RespondedAt) {
		t.Fatalf("Go retry=%+v err=%v", retry, err)
	}
	run := newAgentTestCLI(t, serverURL, key)
	var cliPage api.AgentAsksPage
	if err := json.Unmarshal(run("session", "turn", "ask", "list", session, turn, "--limit", "1", "--json"), &cliPage); err != nil || cliPage.NextCursor == "" {
		t.Fatalf("CLI asks=%+v err=%v", cliPage, err)
	}
	var cliQuestion api.AgentAsk
	if err := json.Unmarshal(run("session", "turn", "ask", "get", session, turn, cliAsk, "--json"), &cliQuestion); err != nil || cliQuestion.ID != cliAsk || cliQuestion.Status != "pending" {
		t.Fatalf("CLI ask=%+v err=%v", cliQuestion, err)
	}
	var cliReceipt api.AgentAsk
	for range 2 {
		if err := json.Unmarshal(run("session", "turn", "ask", "respond", session, turn, cliAsk, "--answer-json", `"Approve"`, "--response-id", "cli-answer", "--json"), &cliReceipt); err != nil || cliReceipt.Status != "responded" || string(cliReceipt.Answer) != `"Approve"` || cliReceipt.RespondedByAPIKeyID == nil {
			t.Fatalf("CLI answer=%+v err=%v", cliReceipt, err)
		}
	}
}
