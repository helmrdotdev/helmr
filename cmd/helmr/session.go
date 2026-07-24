package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cli/format"
	"github.com/spf13/cobra"
)

func sessionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Work with sessions.",
	}
	cmd.AddCommand(
		sessionStartCommand(),
		sessionListCommand(),
		sessionGetCommand(),
		sessionCloseCommand(),
		sessionCancelCommand(),
	)
	return cmd
}

func sessionListCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var externalID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List sessions.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := sessionScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			scope.ExternalID = strings.TrimSpace(externalID)
			response, err := control.ListSessions(cmd.Context(), scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			for _, session := range response.Sessions {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n", session.ID, session.TaskID, session.Status, session.Activity, session.CurrentRunID)
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&externalID, "external-id", "", "Filter by external session identifier.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func sessionGetCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var externalID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "get [SESSION]",
		Short: "Show session details.",
		Args: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(externalID) != "" {
				if len(args) != 0 {
					return fmt.Errorf("SESSION argument cannot be combined with --external-id")
				}
				return nil
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var session api.SessionResponse
			var err error
			if strings.TrimSpace(externalID) != "" {
				session, err = loadSessionByExternalID(cmd, externalID, projectID, environmentID)
			} else {
				session, err = loadSession(cmd, args[0], projectID, environmentID)
			}
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), session)
			}
			writeSessionSummary(cmd, session)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&externalID, "external-id", "", "Load by external session identifier instead of session ID.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func sessionCancelCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var reason string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "cancel SESSION",
		Short: "Cancel a session.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := sessionScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			session, err := control.CancelSession(cmd.Context(), args[0], api.CancelSessionRequest{Reason: strings.TrimSpace(reason)}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), session)
			}
			writeSessionLifecycleResult(cmd, "cancel", session)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&reason, "reason", "", "Cancellation reason.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func sessionCloseCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var reason string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "close SESSION",
		Short: "Close a session.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := sessionScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			session, err := control.CloseSession(cmd.Context(), args[0], api.CloseSessionRequest{Reason: strings.TrimSpace(reason)}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), session)
			}
			writeSessionLifecycleResult(cmd, "close", session)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&reason, "reason", "", "Close reason.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func loadSession(cmd *cobra.Command, sessionID string, projectID string, environmentID string) (api.SessionResponse, error) {
	control, err := controlClient(cmd)
	if err != nil {
		return api.SessionResponse{}, err
	}
	scope, err := sessionScopeForClient(control, projectID, environmentID)
	if err != nil {
		return api.SessionResponse{}, err
	}
	return control.GetSession(cmd.Context(), sessionID, scope)
}

func loadSessionByExternalID(cmd *cobra.Command, externalID string, projectID string, environmentID string) (api.SessionResponse, error) {
	control, err := controlClient(cmd)
	if err != nil {
		return api.SessionResponse{}, err
	}
	scope, err := sessionScopeForClient(control, projectID, environmentID)
	if err != nil {
		return api.SessionResponse{}, err
	}
	scope.ExternalID = strings.TrimSpace(externalID)
	scope.Limit = 2
	response, err := control.ListSessions(cmd.Context(), scope)
	if err != nil {
		return api.SessionResponse{}, err
	}
	switch len(response.Sessions) {
	case 0:
		return api.SessionResponse{}, fmt.Errorf("session with external id %q not found", strings.TrimSpace(externalID))
	case 1:
		return response.Sessions[0], nil
	default:
		return api.SessionResponse{}, fmt.Errorf("session with external id %q resolved to multiple sessions", strings.TrimSpace(externalID))
	}
}

func writeSessionSummary(cmd *cobra.Command, session api.SessionResponse) {
	fmt.Fprintf(cmd.OutOrStdout(), "Session:   %s\n", session.ID)
	fmt.Fprintf(cmd.OutOrStdout(), "Task:      %s\n", session.TaskID)
	fmt.Fprintf(cmd.OutOrStdout(), "Status:    %s\n", session.Status)
	fmt.Fprintf(cmd.OutOrStdout(), "Activity:  %s\n", session.Activity)
	fmt.Fprintf(cmd.OutOrStdout(), "Run:       %s\n", session.CurrentRunID)
	fmt.Fprintf(cmd.OutOrStdout(), "Workspace: %s\n", session.WorkspaceID)
}

func writeSessionLifecycleResult(cmd *cobra.Command, operation string, session api.SessionResponse) {
	fmt.Fprintf(cmd.OutOrStdout(), "operation: %s\n", operation)
	fmt.Fprintf(cmd.OutOrStdout(), "session_id: %s\n", session.ID)
	fmt.Fprintf(cmd.OutOrStdout(), "session_status: %s\n", session.Status)
	fmt.Fprintf(cmd.OutOrStdout(), "session_activity: %s\n", session.Activity)
	if session.CurrentRunID != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "run_id: %s\n", session.CurrentRunID)
	}
}

func addScopeFlags(cmd *cobra.Command, projectID *string, environmentID *string) {
	cmd.Flags().StringVarP(projectID, "project", "p", "", "Project slug or ID.")
	cmd.Flags().StringVarP(environmentID, "env", "e", "", "Environment slug or ID.")
}

func waitTimeoutSeconds(raw string, label string) (int32, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	duration, err := api.ParsePositiveDuration(raw, label)
	if err != nil {
		return 0, err
	}
	seconds := (duration + time.Second - time.Nanosecond) / time.Second
	if seconds > 1<<31-1 {
		return 0, fmt.Errorf("%s is too large", label)
	}
	return int32(seconds), nil
}
