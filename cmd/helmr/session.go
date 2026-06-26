package main

import (
	"encoding/json"
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
		sessionCancelCommand(),
		sessionStreamCommand(),
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
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", session.ID, session.Status)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&reason, "reason", "", "Cancellation reason.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func sessionStreamCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "stream", Short: "Work with session streams."}
	cmd.AddCommand(
		sessionStreamListCommand(),
		sessionStreamInputCommand(),
		sessionStreamOutputCommand(),
	)
	return cmd
}

func sessionStreamListCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list SESSION",
		Short: "List streams for a session.",
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
			response, err := control.ListSessionStreams(cmd.Context(), args[0], scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			for _, stream := range response.Streams {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%d\n", stream.Name, stream.Direction, stream.Sequence)
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func sessionStreamInputCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "input", Short: "Write and inspect session input streams."}
	cmd.AddCommand(
		sessionStreamInputSendCommand(),
		sessionStreamInputListCommand(),
	)
	return cmd
}

func sessionStreamInputSendCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var dataJSON string
	var correlationID string
	var idempotencyKey string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "send SESSION STREAM",
		Short: "Send input to a session stream.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			data := json.RawMessage(strings.TrimSpace(dataJSON))
			if len(data) == 0 || !json.Valid(data) {
				return fmt.Errorf("--data-json must be valid JSON")
			}
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := sessionScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := control.AppendSessionInput(cmd.Context(), args[0], args[1], api.AppendStreamRecordRequest{
				Data:           data,
				CorrelationID:  strings.TrimSpace(correlationID),
				IdempotencyKey: strings.TrimSpace(idempotencyKey),
			}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %d\n", response.Record.ID, response.Record.Sequence)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&dataJSON, "data-json", "", "JSON payload to send.")
	cmd.Flags().StringVar(&correlationID, "correlation-id", "", "Correlation ID.")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	_ = cmd.MarkFlagRequired("data-json")
	return cmd
}

func sessionStreamInputListCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var cursor int64
	var limit int32
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list SESSION STREAM",
		Short: "List session input stream records.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := sessionScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := control.ListSessionInputs(cmd.Context(), args[0], args[1], cursor, limit, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			return format.JSONLines(cmd.OutOrStdout(), response.Records)
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().Int64Var(&cursor, "cursor", 0, "Return records after this sequence.")
	cmd.Flags().Int32Var(&limit, "limit", 0, "Maximum records to return.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func sessionStreamOutputCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "output", Short: "Read session output streams."}
	cmd.AddCommand(sessionStreamOutputListCommand())
	return cmd
}

func sessionStreamOutputListCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var cursor int64
	var limit int32
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list SESSION STREAM",
		Short: "List session output stream records.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := sessionScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := control.ListSessionOutputs(cmd.Context(), args[0], args[1], cursor, limit, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			return format.JSONLines(cmd.OutOrStdout(), response.Records)
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().Int64Var(&cursor, "cursor", 0, "Return records after this sequence.")
	cmd.Flags().Int32Var(&limit, "limit", 0, "Maximum records to return.")
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
