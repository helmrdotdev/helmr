package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

func runDiagnosticTest(t *testing.T, args ...string) (diagnosticReport, string, error) {
	t.Helper()
	var output bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&output)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(append(args, "--json"))
	err := cmd.Execute()
	var report diagnosticReport
	if decodeErr := json.Unmarshal(output.Bytes(), &report); decodeErr != nil {
		t.Fatalf("invalid diagnostic JSON: %v: %s", decodeErr, output.String())
	}
	return report, output.String(), err
}

func clearDiagnosticEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{helmrAPIURLEnv, helmrAPIKeyEnv, "HELMR_PROJECT", "HELMR_ENV"} {
		t.Setenv(key, "")
	}
	installTestCLIConfig(t)
}

func TestDiagnosticOfflinePrecedenceAndCredentialPrivacy(t *testing.T) {
	clearDiagnosticEnvironment(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); t.Error("offline made an HTTP request") }))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, "https://ignored.example")
	t.Setenv(helmrAPIKeyEnv, "NEVER_PRINT_THIS_KEY")
	t.Setenv("HELMR_PROJECT", "env-project")
	t.Setenv("HELMR_ENV", "env-environment")
	report, output, err := runDiagnosticTest(t, "status", "--offline", "--api-url", server.URL, "--project", "flag-project")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("network used")
	}
	if report.APIURL.Value != server.URL || report.APIURL.Source != "flag:--api-url" || report.Project.Value != "flag-project" || report.Environment.Source != "env:HELMR_ENV" {
		t.Fatalf("provenance: %+v", report)
	}
	if (report.Credential.Present == nil || !*report.Credential.Present) || report.Credential.ExpiresAt != nil || report.Live != nil || report.Identity != nil {
		t.Fatalf("offline report: %+v", report)
	}
	if strings.Contains(output, "NEVER_PRINT_THIS_KEY") {
		t.Fatal("credential leaked")
	}
}

func TestDiagnosticNoCredentialProducesReportAndFailure(t *testing.T) {
	clearDiagnosticEnvironment(t)
	for _, command := range []string{"status", "doctor"} {
		report, _, err := runDiagnosticTest(t, command)
		if err == nil || (report.Credential.Present != nil && *report.Credential.Present) || len(report.Checks) == 0 {
			t.Fatalf("%s: %+v, %v", command, report, err)
		}
	}
}

func TestDiagnosticInvalidOriginDoesNotLeakCredentials(t *testing.T) {
	clearDiagnosticEnvironment(t)
	t.Setenv(helmrAPIKeyEnv, "NEVER_PRINT_TOKEN")
	for _, origin := range []string{"https://user:NEVER_PRINT_PASSWORD@example.com", "http://example.com", "https://example.com/?token=NEVER_PRINT_QUERY"} {
		report, output, err := runDiagnosticTest(t, "status", "--offline", "--api-url", origin)
		if err == nil || report.APIURL != nil {
			t.Fatalf("accepted %s", origin)
		}
		if strings.Contains(output, "NEVER_PRINT") {
			t.Fatal("sensitive input leaked")
		}
	}
}

func TestDiagnosticSavedLoginOffline(t *testing.T) {
	clearDiagnosticEnvironment(t)
	state := installTestCLIConfig(t)
	_ = state.SaveLogin("https://saved.example", "NEVER_PRINT_LOGIN")
	report, output, err := runDiagnosticTest(t, "status", "--offline")
	if err != nil {
		t.Fatal(err)
	}
	if report.APIURL.Source != "config:default_host" || report.Credential.Kind != "login" || report.Credential.Present != nil {
		t.Fatalf("report=%+v", report)
	}
	if strings.Contains(output, "NEVER_PRINT_LOGIN") {
		t.Fatal("login leaked")
	}
}

func diagnosticFixture(t *testing.T, attention api.AttentionCounts, role string, override func(http.ResponseWriter, *http.Request) bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("diagnostic mutates state: %s", r.Method)
			w.WriteHeader(405)
			return
		}
		if override != nil && override(w, r) {
			return
		}
		switch r.URL.Path {
		case "/readyz":
			if r.Header.Get("Authorization") != "" {
				t.Error("readiness received credential")
			}
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		case "/api/me":
			_ = json.NewEncoder(w).Encode(api.MeResponse{UserID: "user", Role: role})
		case "/api/projects/project":
			_ = json.NewEncoder(w).Encode(api.ProjectSummary{Project: "project", Environments: []api.EnvironmentSummary{{Environment: "environment", Project: "project"}}})
		case "/api/projects/project/environments/environment/deployments/current", "/v1/deployments/current":
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"code":"no_current_deployment","message":"no current deployment"}}`))
		case "/api/projects/project/environments/environment/sessions", "/v1/sessions":
			_, _ = w.Write([]byte(`{"sessions":[]}`))
		case "/api/projects/project/environments/environment/attention":
			_ = json.NewEncoder(w).Encode(api.Attention{Counts: attention})
		default:
			t.Errorf("unexpected diagnostic request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func useDiagnosticLogin(t *testing.T, server *httptest.Server) {
	t.Helper()
	state := installTestCLIConfig(t)
	_ = state.SaveLogin(server.URL, "saved-login")
}

func TestDiagnosticEmptyEnvironmentIsHealthy(t *testing.T) {
	clearDiagnosticEnvironment(t)
	server := diagnosticFixture(t, api.AttentionCounts{}, "owner", nil)
	useDiagnosticLogin(t, server)
	for _, command := range []string{"status", "doctor"} {
		report, _, err := runDiagnosticTest(t, command, "--project", "project", "--env", "environment")
		if err != nil {
			t.Fatal(err)
		}
		if report.Live == nil || report.Live.Deployment != nil || len(report.Live.Agents) != 0 {
			t.Fatalf("report=%+v", report)
		}
		for _, check := range report.Checks {
			if check.Status == "error" || check.Status == "fail" {
				t.Fatalf("absence failed: %+v", check)
			}
		}
	}
}

func TestDiagnosticStatusReportsProblemButDoctorFails(t *testing.T) {
	clearDiagnosticEnvironment(t)
	server := diagnosticFixture(t, api.AttentionCounts{Hold: 1, FailedTurn: 2}, "member", nil)
	useDiagnosticLogin(t, server)
	for _, command := range []string{"status", "doctor"} {
		report, _, err := runDiagnosticTest(t, command, "--project", "project", "--env", "environment")
		if (err != nil) != (command == "doctor") {
			t.Fatalf("%s err=%v", command, err)
		}
		if report.Live.Attention.Hold != 1 || report.Live.Attention.FailedTurn != 2 {
			t.Fatalf("attention=%+v", report.Live.Attention)
		}
	}
}

func TestDiagnosticAPIKeyAndScopeBoundary(t *testing.T) {
	clearDiagnosticEnvironment(t)
	server := diagnosticFixture(t, api.AttentionCounts{}, "", nil)
	t.Setenv(helmrAPIKeyEnv, "api-key")
	t.Setenv(helmrAPIURLEnv, server.URL)
	report, _, err := runDiagnosticTest(t, "doctor")
	if err != nil {
		t.Fatal(err)
	}
	if report.Identity != nil || report.Live == nil {
		t.Fatalf("report=%+v", report)
	}
	_, _, err = runDiagnosticTest(t, "status", "--project", "project", "--env", "environment")
	if err == nil {
		t.Fatal("API key accepted explicit scope")
	}
}

func TestDiagnosticReadFailureNeverEchoesResponseBody(t *testing.T) {
	clearDiagnosticEnvironment(t)
	server := diagnosticFixture(t, api.AttentionCounts{}, "", func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/me" {
			return false
		}
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"NEVER_PRINT_RESPONSE_TOKEN"}}`))
		return true
	})
	useDiagnosticLogin(t, server)
	_, output, err := runDiagnosticTest(t, "status")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(output, "NEVER_PRINT") {
		t.Fatal("response body leaked")
	}
}

func TestDiagnosticJSONAndHumanExitParity(t *testing.T) {
	clearDiagnosticEnvironment(t)
	server := diagnosticFixture(t, api.AttentionCounts{Hold: 1}, "member", nil)
	useDiagnosticLogin(t, server)
	for _, command := range []string{"status", "doctor"} {
		_, _, jsonErr := runDiagnosticTest(t, command, "--project", "project", "--env", "environment")
		var out bytes.Buffer
		cmd := newRootCommand()
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{command, "--project", "project", "--env", "environment"})
		humanErr := cmd.Execute()
		if (humanErr == nil) != (jsonErr == nil) {
			t.Fatalf("exit differs: json=%v, human=%v", jsonErr, humanErr)
		}
		if !strings.Contains(out.String(), "flag:--project") || !strings.Contains(out.String(), "Attention:") {
			t.Fatalf("missing human context: %s", out.String())
		}
	}
}

func TestDiagnosticAgentsPaginationAndRetainedSlack(t *testing.T) {
	clearDiagnosticEnvironment(t)
	zero := int64(0)
	const deploymentID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
	var sawAgentNext, sawRetainedNext, sawLinksNext bool
	server := diagnosticFixture(t, api.AttentionCounts{SlackReauthorization: &zero}, "owner", func(w http.ResponseWriter, r *http.Request) bool {
		prefix := "/api/projects/project/environments/environment/"
		switch r.URL.Path {
		case prefix + "deployments/current":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: deploymentID})
			return true
		case prefix + "agents":
			if r.URL.Query().Get("view") == "all" {
				if r.URL.Query().Get("cursor") == "retained-next" {
					sawRetainedNext = true
					_, _ = w.Write([]byte(`{"agents":[{"agent":"retired"}]}`))
				} else {
					_, _ = w.Write([]byte(`{"agents":[{"agent":"alpha"}],"next_cursor":"retained-next"}`))
				}
			} else {
				if r.URL.Query().Get("deployment_id") != deploymentID {
					t.Errorf("unbound deployment: %s", r.URL)
				}
				if r.URL.Query().Get("cursor") == "agent-next" {
					sawAgentNext = true
					_, _ = w.Write([]byte(`{"agents":[{"agent":"beta"}]}`))
				} else {
					_, _ = w.Write([]byte(`{"agents":[{"agent":"alpha"}],"next_cursor":"agent-next"}`))
				}
			}
			return true
		case prefix + "agents/alpha/slack":
			_, _ = w.Write([]byte(`{"connection":null}`))
			return true
		case prefix + "agents/retired/slack":
			_, _ = w.Write([]byte(`{"connection":{"id":"conn","status":"connected","team_id":"T2","credentials_configured":true},"manifest":"NEVER_PRINT_MANIFEST"}`))
			return true
		case "/api/slack/user-links":
			if r.URL.Query().Get("cursor") == "links-next" {
				sawLinksNext = true
				_, _ = w.Write([]byte(`{"links":[{"team_id":"T2"}]}`))
			} else {
				_, _ = w.Write([]byte(`{"links":[{"team_id":"T1"}],"next_cursor":"links-next"}`))
			}
			return true
		}
		return false
	})
	useDiagnosticLogin(t, server)
	report, output, err := runDiagnosticTest(t, "doctor", "--project", "project", "--env", "environment")
	if err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	if !sawAgentNext || !sawRetainedNext || !sawLinksNext {
		t.Fatalf("pagination: agents=%t retained=%t links=%t", sawAgentNext, sawRetainedNext, sawLinksNext)
	}
	if len(report.Live.Agents) != 2 || len(report.Live.Slack) != 2 || report.Live.Slack[1].Agent != "retired" {
		t.Fatalf("live=%+v", report.Live)
	}
	if strings.Contains(output, "NEVER_PRINT") {
		t.Fatal("manifest leaked")
	}
}

func TestDiagnosticExistingSlackProblems(t *testing.T) {
	for _, status := range []string{"setup_incomplete", "disconnected", "reauthorization_required", "connected"} {
		t.Run(status, func(t *testing.T) {
			clearDiagnosticEnvironment(t)
			zero := int64(0)
			server := diagnosticFixture(t, api.AttentionCounts{SlackReauthorization: &zero}, "owner", func(w http.ResponseWriter, r *http.Request) bool {
				switch {
				case strings.HasSuffix(r.URL.Path, "/agents"):
					_, _ = w.Write([]byte(`{"agents":[{"agent":"retained"}]}`))
					return true
				case strings.HasSuffix(r.URL.Path, "/agents/retained/slack"):
					_ = json.NewEncoder(w).Encode(map[string]any{"connection": map[string]any{"id": "conn", "status": status, "team_id": "T1"}})
					return true
				case r.URL.Path == "/api/slack/user-links":
					_, _ = w.Write([]byte(`{"links":[]}`))
					return true
				}
				return false
			})
			useDiagnosticLogin(t, server)
			report, _, err := runDiagnosticTest(t, "doctor", "--project", "project", "--env", "environment")
			if err == nil {
				t.Fatal("missing Slack connection/link problem")
			}
			found := false
			for _, check := range report.Checks {
				if check.Status == "fail" && check.Remedy != nil && check.Remedy.RequiresHuman {
					found = true
				}
			}
			if !found {
				t.Fatalf("no human remedy: %+v", report.Checks)
			}
		})
	}
}

func TestDiagnosticReadinessFailureAndUnknownDeployment(t *testing.T) {
	clearDiagnosticEnvironment(t)
	server := diagnosticFixture(t, api.AttentionCounts{}, "member", func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/readyz" {
			_, _ = w.Write([]byte(`{"status":"not-ready"}`))
			return true
		}
		if strings.HasSuffix(r.URL.Path, "/deployments/current") {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"route unavailable"}}`))
			return true
		}
		return false
	})
	useDiagnosticLogin(t, server)
	report, _, err := runDiagnosticTest(t, "doctor", "--project", "project", "--env", "environment")
	if err == nil {
		t.Fatal("expected failure")
	}
	errors := map[string]bool{}
	for _, check := range report.Checks {
		if check.Status == "error" {
			errors[check.ID] = true
		}
	}
	if !errors["server.ready"] || !errors["deploy.current"] {
		t.Fatalf("checks=%+v", report.Checks)
	}
}

type diagnosticNoTokenState struct {
	*testCLIState
	t *testing.T
}

func (s diagnosticNoTokenState) Token(string) (string, error) {
	s.t.Fatal("offline read the keyring")
	return "", nil
}

func TestDiagnosticOfflineDoesNotOpenKeyring(t *testing.T) {
	clearDiagnosticEnvironment(t)
	state := installTestCLIConfig(t)
	_ = state.SaveLogin("https://saved.example", "unused")
	newCLIStateStore = func() (cliState, error) { return diagnosticNoTokenState{state, t}, nil }
	report, _, err := runDiagnosticTest(t, "status", "--offline")
	if err != nil || report.Credential.Present != nil {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestDiagnosticAPIKeyRecentTurnsFollowPages(t *testing.T) {
	clearDiagnosticEnvironment(t)
	const sessionID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
	sawNext := false
	server := diagnosticFixture(t, api.AttentionCounts{}, "", func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/v1/sessions" {
			_ = json.NewEncoder(w).Encode(api.AgentSessionsPage{Sessions: []api.AgentSession{{ID: sessionID, AgentDefinitionID: "alpha", Status: "open"}}})
			return true
		}
		if r.URL.Path == "/v1/sessions/"+sessionID+"/turns" {
			if r.URL.Query().Get("cursor") == "1" {
				sawNext = true
				terminal := time.Now().Add(-time.Hour)
				_ = json.NewEncoder(w).Encode(api.AgentTurnsPage{Turns: []api.AgentTurn{{ID: "turn2", Sequence: 2, Status: "failed", TerminalAt: &terminal}}})
			} else {
				_, _ = w.Write([]byte(`{"turns":[{"id":"turn1","status":"completed"}],"next_cursor":"1"}`))
			}
			return true
		}
		return false
	})
	t.Setenv(helmrAPIKeyEnv, "api-key")
	t.Setenv(helmrAPIURLEnv, server.URL)
	report, _, err := runDiagnosticTest(t, "doctor")
	if err == nil || !sawNext {
		t.Fatalf("err=%v sawNext=%t", err, sawNext)
	}
	found := false
	for _, check := range report.Checks {
		if check.ID == "turns.failed" && check.Status == "fail" {
			found = true
		}
	}
	if !found {
		t.Fatalf("checks=%+v", report.Checks)
	}
}

func TestDiagnosticPermissionFailureDoesNotEraseOtherState(t *testing.T) {
	clearDiagnosticEnvironment(t)
	server := diagnosticFixture(t, api.AttentionCounts{}, "member", func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/sessions") {
			w.WriteHeader(403)
			return true
		}
		return false
	})
	useDiagnosticLogin(t, server)
	report, _, err := runDiagnosticTest(t, "status", "--project", "project", "--env", "environment")
	if err == nil || report.Identity == nil || report.Live == nil || report.Live.Attention == nil || report.Live.RecentSessions != nil || report.Live.Slack != nil {
		t.Fatalf("lost partial evidence: %+v, %v", report, err)
	}
}

func TestDiagnosticOfflineRejectsMissingHost(t *testing.T) {
	clearDiagnosticEnvironment(t)
	report, _, err := runDiagnosticTest(t, "status", "--offline", "--api-url", "https://")
	if err == nil || report.APIURL != nil {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestDiagnosticRecoveredClosedAndExpiredTurns(t *testing.T) {
	for _, scenario := range []string{"recovered", "closed", "expired", "pending"} {
		t.Run(scenario, func(t *testing.T) {
			clearDiagnosticEnvironment(t)
			const sessionID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
			server := diagnosticFixture(t, api.AttentionCounts{}, "", func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/v1/sessions" {
					status := "open"
					if scenario == "closed" {
						status = "closed"
					}
					_ = json.NewEncoder(w).Encode(api.AgentSessionsPage{Sessions: []api.AgentSession{{ID: sessionID, Status: status}}})
					return true
				}
				if r.URL.Path == "/v1/sessions/"+sessionID+"/turns" {
					if scenario == "closed" {
						t.Error("read history for closed Session")
					}
					terminal := time.Now().Add(-time.Hour)
					if scenario == "expired" {
						terminal = time.Now().Add(-25 * time.Hour)
					}
					turns := []api.AgentTurn{{ID: "failed", Sequence: 1, Status: "failed", TerminalAt: &terminal}}
					if scenario == "recovered" {
						turns = append(turns, api.AgentTurn{ID: "newer", Sequence: 2, Status: "completed"})
					}
					if scenario == "pending" {
						turns = append(turns, api.AgentTurn{ID: "newer", Sequence: 2, Status: "pending"})
					}
					_ = json.NewEncoder(w).Encode(api.AgentTurnsPage{Turns: turns})
					return true
				}
				return false
			})
			t.Setenv(helmrAPIKeyEnv, "api-key")
			t.Setenv(helmrAPIURLEnv, server.URL)
			report, output, err := runDiagnosticTest(t, "doctor")
			if err != nil {
				t.Fatalf("%v: %s", err, output)
			}
			for _, check := range report.Checks {
				if check.ID == "turns.failed" && check.Status != "pass" {
					t.Fatalf("stale failure: %+v", check)
				}
			}
		})
	}
}

func TestDiagnosticIncompleteAgentListRemainsUnknown(t *testing.T) {
	clearDiagnosticEnvironment(t)
	const deploymentID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
	server := diagnosticFixture(t, api.AttentionCounts{}, "member", func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/deployments/current") {
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: deploymentID})
			return true
		}
		if strings.HasSuffix(r.URL.Path, "/agents") {
			if r.URL.Query().Get("cursor") == "next" {
				w.WriteHeader(503)
			} else {
				_, _ = w.Write([]byte(`{"agents":[{"agent":"partial"}],"next_cursor":"next"}`))
			}
			return true
		}
		return false
	})
	useDiagnosticLogin(t, server)
	report, _, err := runDiagnosticTest(t, "status", "--project", "project", "--env", "environment")
	if err == nil || report.Live.Agents != nil || report.Live.RecentSessions == nil {
		t.Fatalf("report=%+v err=%v", report.Live, err)
	}
}
