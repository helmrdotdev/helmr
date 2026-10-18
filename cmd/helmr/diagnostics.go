package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/clistate"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/version"
	"github.com/spf13/cobra"
)

type diagnosticValue struct {
	Value  string `json:"value"`
	Source string `json:"source"`
}

type diagnosticCredential struct {
	Present   *bool      `json:"present"`
	Kind      string     `json:"kind"`
	Source    string     `json:"source"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type diagnosticRemedy struct {
	Title         string `json:"title"`
	RequiresHuman bool   `json:"requires_human"`
}

type diagnosticCheck struct {
	ID      string            `json:"id"`
	Status  string            `json:"status"`
	Message string            `json:"message"`
	Remedy  *diagnosticRemedy `json:"remedy,omitempty"`
}

type diagnosticSlack struct {
	Agent      string                        `json:"agent"`
	Connection *client.SlackConnectionStatus `json:"connection"`
}

type diagnosticLive struct {
	Deployment         *api.DeploymentResponse `json:"deployment"`
	Agents             []string                `json:"agents"`
	RecentSessions     []diagnosticSession     `json:"recent_sessions"`
	SessionsNextCursor string                  `json:"sessions_next_cursor,omitempty"`
	Attention          *api.AttentionCounts    `json:"attention"`
	Slack              []diagnosticSlack       `json:"slack"`
}

type diagnosticSession struct {
	ID     string `json:"id"`
	Agent  string `json:"agent"`
	Status string `json:"status"`
	Holds  int    `json:"holds"`
}

type diagnosticReport struct {
	Schema      string               `json:"schema"`
	CLIVersion  string               `json:"cli_version"`
	Offline     bool                 `json:"offline"`
	APIURL      *diagnosticValue     `json:"api_url"`
	Project     *diagnosticValue     `json:"project"`
	Environment *diagnosticValue     `json:"environment"`
	Credential  diagnosticCredential `json:"credential"`
	Identity    *api.MeResponse      `json:"identity"`
	Live        *diagnosticLive      `json:"live"`
	Checks      []diagnosticCheck    `json:"checks"`
}

func statusCommand() *cobra.Command { return diagnosticCommand(false) }
func doctorCommand() *cobra.Command { return diagnosticCommand(true) }

func diagnosticCommand(doctor bool) *cobra.Command {
	var jsonOutput, offline bool
	var project, environment string
	name, description := "status", "Show CLI context and existing Helmr resources."
	if doctor {
		name, description = "doctor", "Check the health of existing Helmr resources."
	}
	cmd := &cobra.Command{
		Use: name, Short: description, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			report := collectDiagnostics(ctx, cmd, project, environment, offline, doctor)
			if jsonOutput {
				if err := writeJSON(cmd.OutOrStdout(), report); err != nil {
					return err
				}
			} else {
				printDiagnostics(cmd, report)
			}
			for _, check := range report.Checks {
				if check.Status == "error" || (doctor && check.Status == "fail") {
					return errors.New(name + " found problems; see the report above")
				}
			}
			return nil
		},
	}
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON report, including on failure.")
	if !doctor {
		cmd.Flags().BoolVar(&offline, "offline", false, "Read local context and credential presence without network requests.")
	}
	return cmd
}

func diagnosticScopeValue(cmd *cobra.Command, flag, env, value string) *diagnosticValue {
	if cmd.Flags().Changed(flag) {
		return &diagnosticValue{strings.TrimSpace(value), "flag:--" + flag}
	}
	if value := strings.TrimSpace(os.Getenv(env)); value != "" {
		return &diagnosticValue{value, "env:" + env}
	}
	return nil
}

func (r *diagnosticReport) check(id, status, message string, remedy *diagnosticRemedy) {
	r.Checks = append(r.Checks, diagnosticCheck{id, status, message, remedy})
}

func (r *diagnosticReport) readError(id string, err error) {
	message := "Could not read this state; check the server connection and retry."
	remedy := &diagnosticRemedy{"Check the server connection and retry.", false}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		message = "The diagnostic request timed out or was cancelled."
	case httpclient.IsStatus(err, http.StatusUnauthorized):
		message = "The server rejected the credential."
		remedy = &diagnosticRemedy{"Sign in with helmr login for this server, or replace the configured API key.", true}
	case httpclient.IsStatus(err, http.StatusForbidden):
		message = "The credential cannot read this state."
		remedy = &diagnosticRemedy{"Ask an owner or admin to confirm the required access.", true}
	case httpclient.IsStatus(err, http.StatusNotFound):
		message = "The requested resource or API route was not found."
		remedy = &diagnosticRemedy{"Check the explicit target and that the server supports this operation.", false}
	}
	// Upstream errors can contain arbitrary response text; never echo credentials
	// or request/response bodies as part of a diagnostic report.
	r.check(id, "error", message, remedy)
}

func collectDiagnostics(ctx context.Context, cmd *cobra.Command, project, environment string, offline, doctor bool) diagnosticReport {
	report := diagnosticReport{Schema: "helmr.status/v1", CLIVersion: version.String(), Offline: offline, Checks: []diagnosticCheck{}}
	if doctor {
		report.Schema = "helmr.doctor/v1"
	}
	report.Project = diagnosticScopeValue(cmd, "project", "HELMR_PROJECT", project)
	report.Environment = diagnosticScopeValue(cmd, "env", "HELMR_ENV", environment)
	cp := diagnosticConnection(cmd, &report)
	if cp == nil {
		return report
	}
	if offline {
		report.check("server.state", "skip", "Offline: server identity, resource state and credential validity are not checked.", nil)
		return report
	}
	if doctor {
		// Readiness is public. Do not send the saved credential to this probe.
		probe, _ := client.New(cp.BaseURL())
		if err := probe.CheckReadiness(ctx); err != nil {
			report.readError("server.ready", err)
		} else {
			report.check("server.ready", "pass", "Server database readiness check passed; this does not establish Worker capacity.", nil)
		}
	}
	if cp.UsesSessionScopedRoutes() {
		identity, err := cp.GetMe(ctx)
		if err != nil {
			report.readError("auth.identity", err)
			return report
		}
		report.Identity = &identity
		report.check("auth.identity", "pass", "Saved login accepted by the server.", nil)
		if report.Project == nil && report.Environment == nil {
			report.check("scope.environment", "skip", "No Environment selected. Pass --project and --env, or HELMR_PROJECT and HELMR_ENV, to inspect resources.", nil)
			return report
		}
		if report.Project == nil || report.Environment == nil || report.Project.Value == "" || report.Environment.Value == "" {
			report.check("scope.environment", "error", "Select both a Project and an Environment.", &diagnosticRemedy{"Pass --project and --env, or HELMR_PROJECT and HELMR_ENV.", false})
			return report
		}
	} else if report.Project != nil || report.Environment != nil {
		report.check("scope.environment", "error", "API keys are already Environment scoped; explicit scope is not supported.", &diagnosticRemedy{"Remove --project/--env and HELMR_PROJECT/HELMR_ENV, or use a saved login without HELMR_API_KEY.", false})
		return report
	}
	p, e := "", ""
	if report.Project != nil {
		p = report.Project.Value
	}
	if report.Environment != nil {
		e = report.Environment.Value
	}
	scope, err := environmentScopeForClient(ctx, cp, p, e)
	if err != nil {
		report.readError("scope.environment", err)
		return report
	}
	if cp.UsesSessionScopedRoutes() {
		report.check("scope.environment", "pass", "Selected Project and Environment are accessible.", nil)
	} else {
		report.check("scope.environment", "skip", "API-key scope is enforced by the server; its Project and Environment names are not available through introspection.", nil)
	}
	report.Live = &diagnosticLive{}
	deployment, err := cp.GetCurrentDeployment(ctx, scope)
	var apiErr *httpclient.Error
	noDeployment := errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound && apiErr.Code == "no_current_deployment"
	switch {
	case err == nil:
		report.Live.Deployment = &deployment
		report.check("deploy.current", "pass", "Current deployment is readable.", nil)
	case noDeployment:
		report.Live.Agents = []string{}
		report.check("deploy.current", "skip", "No current deployment.", nil)
	default:
		report.readError("deploy.current", err)
	}
	if err == nil {
		collectDiagnosticAgents(ctx, cp, scope, &report)
	} else {
		report.check("agents.registered", "skip", "No readable current deployment to list Agents from.", nil)
	}
	sessions, err := cp.ListSessions(ctx, client.SessionListOptions{EnvironmentScopeOptions: scope, Limit: 10})
	if err != nil {
		report.readError("sessions.recent", err)
	} else {
		report.Live.RecentSessions = []diagnosticSession{}
		for _, s := range sessions.Sessions {
			report.Live.RecentSessions = append(report.Live.RecentSessions, diagnosticSession{s.ID, s.AgentDefinitionID, s.Status, len(s.Holds)})
		}
		report.Live.SessionsNextCursor = sessions.NextCursor
		report.check("sessions.recent", "pass", fmt.Sprintf("Read %d recent Sessions (up to 10).", len(sessions.Sessions)), nil)
	}
	if cp.UsesSessionScopedRoutes() {
		attention, err := cp.GetAttention(ctx, scope)
		if err != nil {
			report.readError("runtime.attention", err)
		} else {
			report.Live.Attention = &attention.Counts
			report.check("runtime.attention", "pass", "Environment attention counts are readable.", nil)
			if doctor {
				diagnosticCount(&report, "sessions.holds", attention.Counts.Hold, "Sessions on hold", "Inspect held Sessions and resolve the recorded hold reason.", true)
				diagnosticCount(&report, "turns.failed", attention.Counts.FailedTurn, "failed Turns requiring attention", "Read the affected Session events before retrying.", false)
				if attention.Counts.SlackReauthorization != nil {
					diagnosticCount(&report, "slack.authorization", *attention.Counts.SlackReauthorization, "Slack connections needing authorization", "Reauthorize the existing Slack connection in the Console.", true)
				}
			}
		}
	} else {
		report.check("runtime.attention", "skip", "Environment-wide attention and Slack management reads require a saved login.", nil)
		if doctor && err == nil {
			collectDiagnosticRecentTurns(ctx, cp, scope, sessions.Sessions, &report)
		}
	}
	if report.Identity != nil && (report.Identity.Role == "owner" || report.Identity.Role == "admin") && report.Live.Attention != nil && report.Live.Attention.SlackReauthorization != nil {
		collectDiagnosticSlack(ctx, cp, scope, doctor, &report)
	} else {
		report.check("slack.connections", "skip", "Slack management inspection is not available for this server or credential.", nil)
	}
	if doctor {
		report.check("secrets.bindings", "skip", "Deployed Secret binding references are not exposed by the current definition reads; no Secret names or harness requirements are inferred.", nil)
		report.check("capacity.workers", "skip", "Worker admission capacity is not exposed by the current CLI-readable APIs; no runtime was started.", nil)
		report.check("deploy.attempt", "skip", "Failed local build and upload attempts are not persisted in deployment listings; inspect the deploy command output.", nil)
	}
	return report
}

func diagnosticCount(report *diagnosticReport, id string, count int64, description, fix string, human bool) {
	status := "pass"
	var remedy *diagnosticRemedy
	if count > 0 {
		status = "fail"
		remedy = &diagnosticRemedy{fix, human}
	}
	report.check(id, status, fmt.Sprintf("%d %s.", count, description), remedy)
}

func diagnosticConnection(cmd *cobra.Command, report *diagnosticReport) *client.Client {
	rawURL, source := explicitAPIURL(cmd), "flag:--api-url"
	if rawURL == "" {
		rawURL, source = strings.TrimSpace(os.Getenv(helmrAPIURLEnv)), "env:HELMR_API_URL"
	}
	token := os.Getenv(helmrAPIKeyEnv)
	session := token == ""
	present := token != ""
	report.Credential = diagnosticCredential{Present: &present, Kind: "api_key", Source: "env:HELMR_API_KEY"}
	if strings.TrimSpace(token) != token {
		report.check("auth.credential", "error", "HELMR_API_KEY has surrounding whitespace.", &diagnosticRemedy{"Correct HELMR_API_KEY without printing its value.", true})
		return nil
	}
	var state cliState
	if session {
		report.Credential = diagnosticCredential{Present: &present, Kind: "login", Source: "keyring"}
		var err error
		state, err = newCLIStateStore()
		if err != nil {
			report.check("auth.credential", "error", "Could not open the CLI credential store.", &diagnosticRemedy{"Check access to the OS credential store in your terminal.", true})
			return nil
		}
		if rawURL == "" {
			cfg, err := state.Load()
			if err != nil && !errors.Is(err, clistate.ErrNotFound) {
				report.check("cli.config", "error", "Could not read saved CLI configuration.", &diagnosticRemedy{"Check the CLI configuration file.", true})
				return nil
			}
			rawURL, source = cfg.DefaultHost, "config:default_host"
		}
	} else if rawURL == "" {
		rawURL, source = managedCloudAPIOrigin, "default"
	}
	if rawURL == "" {
		report.check("auth.credential", "error", "No server or saved login is configured.", &diagnosticRemedy{"Run helmr login for the intended server.", true})
		return nil
	}
	parsed, err := parseControlPlaneURL(rawURL)
	if err != nil {
		report.check("server.origin", "error", "Server must be an HTTPS origin (HTTP is allowed only for loopback), without credentials, path, query or fragment.", &diagnosticRemedy{"Correct --api-url or HELMR_API_URL.", false})
		return nil
	}
	report.APIURL = &diagnosticValue{parsed.String(), source}
	if session && report.Offline {
		report.Credential.Present = nil
		report.check("auth.credential", "skip", "Offline: saved credential presence is unknown; the OS credential store was not opened.", nil)
		cp, err := client.New(parsed.String())
		if err != nil {
			report.APIURL = nil
			report.readError("server.origin", err)
		}
		return cp
	}
	if session {
		token, err = state.Token(parsed.String())
		if err != nil {
			message := "No saved credential for this server."
			if !errors.Is(err, clistate.ErrNotFound) {
				message = "Could not read the saved credential from the OS credential store."
			}
			report.check("auth.credential", "error", message, &diagnosticRemedy{"Run helmr login for this server in your terminal.", true})
			return nil
		}
		present = token != ""
	}
	if token == "" {
		report.check("auth.credential", "error", "No credential is configured.", &diagnosticRemedy{"Run helmr login for this server.", true})
		return nil
	}
	report.check("auth.credential", "pass", "Credential is present; expiry is not exposed by the current credential store/API.", nil)
	opts := []client.Option{client.WithBearerToken(token)}
	if session {
		opts = append(opts, client.WithSessionScopedRoutes())
	}
	cp, err := client.New(parsed.String(), opts...)
	if err != nil {
		report.readError("server.origin", err)
		return nil
	}
	return cp
}

func printDiagnostics(cmd *cobra.Command, report diagnosticReport) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "helmr %s\n", report.CLIVersion)
	for _, row := range []struct {
		label string
		value *diagnosticValue
	}{{"Server", report.APIURL}, {"Project", report.Project}, {"Environment", report.Environment}} {
		if row.value == nil {
			fmt.Fprintf(out, "%s: not selected\n", row.label)
		} else {
			fmt.Fprintf(out, "%s: %s (%s)\n", row.label, row.value.Value, row.value.Source)
		}
	}
	presence := "unknown"
	if report.Credential.Present != nil {
		presence = fmt.Sprint(*report.Credential.Present)
	}
	fmt.Fprintf(out, "Credential: %s, present=%s (%s); expiry unknown\n", report.Credential.Kind, presence, report.Credential.Source)
	if report.Identity != nil {
		fmt.Fprintf(out, "Identity: %s · %s · %s\n", report.Identity.DisplayName, report.Identity.OrgName, report.Identity.Role)
	}
	if live := report.Live; live != nil {
		if live.Deployment == nil {
			fmt.Fprintln(out, "Deployment: none or unavailable (see checks)")
		} else {
			fmt.Fprintf(out, "Deployment: %s\n", live.Deployment.ID)
		}
		if live.Agents == nil {
			fmt.Fprintln(out, "Agents: unknown (see checks)")
		} else {
			fmt.Fprintf(out, "Agents: %s\n", strings.Join(live.Agents, ", "))
		}
		if live.RecentSessions == nil {
			fmt.Fprintln(out, "Sessions: unknown (see checks)")
		}
		if live.Slack == nil {
			fmt.Fprintln(out, "Slack: unknown (see checks)")
		}
		for _, s := range live.RecentSessions {
			fmt.Fprintf(out, "Session: %s · %s · %s · %d holds\n", s.ID, s.Agent, s.Status, s.Holds)
		}
		for _, s := range live.Slack {
			if s.Connection == nil {
				fmt.Fprintf(out, "Slack: %s · no connection\n", s.Agent)
			} else {
				fmt.Fprintf(out, "Slack: %s · %s\n", s.Agent, s.Connection.Status)
			}
		}
		if live.Attention != nil {
			fmt.Fprintf(out, "Attention: %d questions, %d holds, %d failed Turns\n", live.Attention.Question, live.Attention.Hold, live.Attention.FailedTurn)
		}
	}
	for _, check := range report.Checks {
		fmt.Fprintf(out, "[%s] %s: %s\n", check.Status, check.ID, check.Message)
		if check.Remedy != nil {
			fmt.Fprintf(out, "  Remedy (requires_human=%t): %s\n", check.Remedy.RequiresHuman, check.Remedy.Title)
		}
	}
}
