package controlplane

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
)

// Exercise the same Go client used by the CLI against actual HTTP admission and
// retained state, then execute the CLI to cover its flag and rendering boundaries.
func assertAgentCLIHTTP(t *testing.T, serverURL, key, owner, project, environment string) {
	t.Helper()
	for _, scoped := range []bool{false, true} {
		options := []client.Option{client.WithBearerToken(key)}
		scope := client.EnvironmentScopeOptions{}
		if scoped {
			options = []client.Option{client.WithBearerToken(owner), client.WithSessionScopedRoutes()}
			scope = client.EnvironmentScopeOptions{ProjectID: project, EnvironmentID: environment}
		}
		cp, err := client.New(serverURL, options...)
		if err != nil {
			t.Fatal(err)
		}
		sessionKey := "go-client-raw"
		if scoped {
			sessionKey = "go-client-scoped"
		}
		first, err := cp.StartAgent(t.Context(), "agent", api.StartAgentRequest{Input: json.RawMessage(`[{"type":"text","text":"{\"initial\":true}"}]`), SessionKey: &sessionKey, IdempotencyKey: sessionKey}, scope)
		if err != nil {
			t.Fatal(err)
		}
		session, err := cp.RetrieveSession(t.Context(), first.SessionID, scope)
		if err != nil || session.AgentID == "" || session.ComputerID == "" || session.InitialTurn == nil || session.InitialTurn.ID != first.TurnID {
			t.Fatalf("session=%+v err=%v", session, err)
		}
		page, err := cp.ListSessions(t.Context(), client.SessionListOptions{AgentID: session.AgentID, Key: &sessionKey, Statuses: []string{"open"}, Limit: 1, EnvironmentScopeOptions: scope})
		if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != session.ID {
			t.Fatalf("sessions=%+v err=%v", page, err)
		}
		request := api.EnqueueSessionRequest{Input: json.RawMessage(`[]`), IdempotencyKey: sessionKey + "-next"}
		next, err := cp.EnqueueSession(t.Context(), session.ID, request, scope)
		if err != nil || next.SessionID != session.ID || next.Sequence != 2 {
			t.Fatalf("enqueue=%+v err=%v", next, err)
		}
		replay, err := cp.EnqueueSession(t.Context(), session.ID, request, scope)
		if err != nil || replay != next {
			t.Fatalf("replay=%+v err=%v", replay, err)
		}
		turns, err := cp.ListSessionTurns(t.Context(), session.ID, client.SessionTurnListOptions{Limit: 1, EnvironmentScopeOptions: scope})
		if err != nil || len(turns.Turns) != 1 || turns.Turns[0].ID != first.TurnID || turns.NextCursor != "1" {
			t.Fatalf("turn page=%+v err=%v", turns, err)
		}
		turns, err = cp.ListSessionTurns(t.Context(), session.ID, client.SessionTurnListOptions{Limit: 1, Cursor: turns.NextCursor, EnvironmentScopeOptions: scope})
		if err != nil || len(turns.Turns) != 1 || turns.Turns[0].ID != next.TurnID || turns.NextCursor != "" {
			t.Fatalf("next page=%+v err=%v", turns, err)
		}
		turn, err := cp.RetrieveSessionTurn(t.Context(), session.ID, next.TurnID, scope)
		if err != nil || string(turn.Input) != "[]" || turn.Sequence != next.Sequence {
			t.Fatalf("turn=%+v err=%v", turn, err)
		}
		hold, err := cp.InterruptSession(t.Context(), session.ID, api.InterruptSessionRequest{}, scope)
		if err != nil || hold.HoldID == "" {
			t.Fatalf("interrupt=%+v err=%v", hold, err)
		}
		session, err = cp.RetrieveSession(t.Context(), session.ID, scope)
		if err != nil || len(session.Holds) != 1 || session.Holds[0].ID != hold.HoldID || session.Holds[0].Scope != "subtree" {
			t.Fatalf("holds=%+v err=%v", session, err)
		}
		if _, err := cp.ResumeSession(t.Context(), session.ID, api.ResumeSessionRequest{HoldID: hold.HoldID}, scope); err != nil {
			t.Fatal(err)
		}
		events, err := cp.ReadSessionEvents(t.Context(), session.ID, client.SessionEventReadOptions{Limit: 1, EnvironmentScopeOptions: scope})
		if err != nil || len(events.Records) != 1 || events.Records[0].SessionID != session.ID || events.NextAfter == 0 || !events.HasMore {
			t.Fatalf("events=%+v err=%v", events, err)
		}
	}
	run := newAgentTestCLI(t, serverURL, key)
	var first api.StartAgentResponse
	if err := json.Unmarshal(run("agent", "start", "agent", "--input-json", `[{"type":"text","text":"cli"}]`, "--idempotency-key", "cli-start", "--json"), &first); err != nil || first.TurnID == "" {
		t.Fatalf("CLI admission=%+v err=%v", first, err)
	}
	var next api.TurnAdmission
	if err := json.Unmarshal(run("session", "enqueue", first.SessionID, "--input-json", "[]", "--idempotency-key", "cli-next", "--json"), &next); err != nil || next.SessionID != first.SessionID || next.Sequence != 2 {
		t.Fatalf("CLI enqueue=%+v err=%v", next, err)
	}
	var turn api.AgentTurn
	if err := json.Unmarshal(run("session", "turn", "get", first.SessionID, next.TurnID, "--json"), &turn); err != nil || string(turn.Input) != "[]" || turn.Status != "queued" {
		t.Fatalf("CLI turn=%+v err=%v", turn, err)
	}
	var turns api.AgentTurnsPage
	if err := json.Unmarshal(run("session", "turn", "list", first.SessionID, "--limit", "1", "--json"), &turns); err != nil || turns.NextCursor != "1" || len(turns.Turns) != 1 {
		t.Fatalf("CLI turns=%+v err=%v", turns, err)
	}
	var observation struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(run("session", "turn", "wait", first.SessionID, next.TurnID, "--timeout", "100ms", "--json"), &observation); err != nil || observation.Status != "timeout" {
		t.Fatalf("CLI bounded wait=%+v err=%v", observation, err)
	}
	var receipt api.SessionInterruptReceipt
	if err := json.Unmarshal(run("session", "interrupt", first.SessionID, "--json"), &receipt); err != nil || receipt.HoldID == "" {
		t.Fatalf("CLI hold=%+v err=%v", receipt, err)
	}
	if output := run("session", "get", first.SessionID); !bytes.Contains(output, []byte("hold_id: "+receipt.HoldID)) {
		t.Fatalf("CLI hold not visible: %s", output)
	}
	run("session", "resume", first.SessionID, "--hold", receipt.HoldID, "--json")
	var sessions api.AgentSessionsPage
	if err := json.Unmarshal(run("session", "list", "--status", "open", "--json"), &sessions); err != nil || len(sessions.Sessions) == 0 {
		t.Fatalf("CLI sessions=%+v err=%v", sessions, err)
	}
}

func newAgentTestCLI(t *testing.T, serverURL, key string) func(...string) []byte {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "helmr")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../cmd/helmr")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	return func(args ...string) []byte {
		t.Helper()
		command := exec.CommandContext(t.Context(), binary, args...)
		command.Env = append(os.Environ(), "HELMR_API_URL="+serverURL, "HELMR_API_KEY="+key)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		if err := command.Run(); err != nil {
			t.Fatalf("CLI %v: %v\n%s", args, err, stderr.Bytes())
		}
		return stdout.Bytes()
	}
}
