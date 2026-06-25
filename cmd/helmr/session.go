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
		Short: "Work with task sessions.",
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
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List task sessions.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := taskSessionScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := control.ListTaskSessions(cmd.Context(), scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			for _, session := range response.Sessions {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", session.ID, session.TaskID, session.Status, session.CurrentRunID)
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func sessionGetCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "get SESSION",
		Short: "Show task session details.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := loadSession(cmd, args[0], projectID, environmentID)
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
		Short: "Cancel a task session.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := taskSessionScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			session, err := control.CancelTaskSession(cmd.Context(), args[0], api.CancelTaskSessionRequest{Reason: strings.TrimSpace(reason)}, scope)
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
		Short: "List streams for a task session.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := taskSessionScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := control.ListTaskSessionStreams(cmd.Context(), args[0], scope)
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
		Short: "Send input to a task session stream.",
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
			scope, err := taskSessionScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := control.AppendTaskSessionInput(cmd.Context(), args[0], args[1], api.AppendStreamRecordRequest{
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
			scope, err := taskSessionScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := control.ListTaskSessionInputs(cmd.Context(), args[0], args[1], cursor, limit, scope)
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
			scope, err := taskSessionScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := control.ListTaskSessionOutputs(cmd.Context(), args[0], args[1], cursor, limit, scope)
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

func loadSession(cmd *cobra.Command, sessionID string, projectID string, environmentID string) (api.TaskSessionResponse, error) {
	control, err := controlClient(cmd)
	if err != nil {
		return api.TaskSessionResponse{}, err
	}
	scope, err := taskSessionScopeForClient(control, projectID, environmentID)
	if err != nil {
		return api.TaskSessionResponse{}, err
	}
	return control.GetTaskSession(cmd.Context(), sessionID, scope)
}

func writeSessionSummary(cmd *cobra.Command, session api.TaskSessionResponse) {
	fmt.Fprintf(cmd.OutOrStdout(), "Session:   %s\n", session.ID)
	fmt.Fprintf(cmd.OutOrStdout(), "Task:      %s\n", session.TaskID)
	fmt.Fprintf(cmd.OutOrStdout(), "Status:    %s\n", session.Status)
	fmt.Fprintf(cmd.OutOrStdout(), "Run:       %s\n", session.CurrentRunID)
	fmt.Fprintf(cmd.OutOrStdout(), "Workspace: %s\n", session.WorkspaceID)
}

func taskSessionStatusTerminal(status string) bool {
	switch strings.TrimSpace(status) {
	case "closed", "cancelled":
		return true
	default:
		return false
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
