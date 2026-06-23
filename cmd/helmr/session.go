package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cli/format"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

func sessionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Work with task sessions.",
	}
	cmd.AddCommand(
		sessionListCommand(),
		sessionGetCommand(),
		sessionWaitCommand(),
		sessionCancelCommand(),
		sessionInputCommand(),
		sessionOutputCommand(),
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

func sessionWaitCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var timeout string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "wait SESSION",
		Short: "Wait for a task session to finish.",
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
			timeoutSeconds, err := waitTimeoutSeconds(timeout, "--timeout")
			if err != nil {
				return err
			}
			var deadline time.Time
			if timeoutSeconds > 0 {
				deadline = time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
			}
			session, err := waitTaskSessionUntilTerminal(cmd.Context(), control, args[0], deadline, timeoutSeconds, scope)
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
	cmd.Flags().StringVar(&timeout, "timeout", "", "Maximum wait duration, for example 10m or 1h.")
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

func sessionInputCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "input", Short: "Write session channel input."}
	cmd.AddCommand(sessionInputSendCommand())
	return cmd
}

func sessionInputSendCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var dataJSON string
	var correlationID string
	var externalEventID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "send SESSION CHANNEL",
		Short: "Send input to a task session channel.",
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
			response, err := control.AppendTaskSessionInput(cmd.Context(), args[0], args[1], api.AppendChannelRecordRequest{
				Data:            data,
				CorrelationID:   strings.TrimSpace(correlationID),
				ExternalEventID: strings.TrimSpace(externalEventID),
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
	cmd.Flags().StringVar(&externalEventID, "external-event-id", "", "External event ID.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	_ = cmd.MarkFlagRequired("data-json")
	return cmd
}

func sessionOutputCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "output", Short: "Read session channel output."}
	cmd.AddCommand(sessionOutputListCommand(), sessionOutputFollowCommand())
	return cmd
}

func sessionOutputListCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var cursor int64
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list SESSION CHANNEL",
		Short: "List session channel output records.",
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
			response, err := control.ListTaskSessionOutputs(cmd.Context(), args[0], args[1], cursor, scope)
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
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func sessionOutputFollowCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var cursor int64
	var jsonLines bool
	cmd := &cobra.Command{
		Use:   "follow SESSION CHANNEL",
		Short: "Follow session channel output records.",
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
			return followSessionOutput(cmd, control, args[0], args[1], cursor, scope, jsonLines)
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().Int64Var(&cursor, "cursor", 0, "Follow after this sequence.")
	cmd.Flags().BoolVar(&jsonLines, "jsonl", false, "Emit one JSON record per line.")
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

func followSessionOutput(cmd *cobra.Command, control interface {
	FollowTaskSessionOutputs(context.Context, string, string, int64, client.TaskSessionScopeOptions, func(api.ChannelRecordResponse) error) error
	GetTaskSession(context.Context, string, client.TaskSessionScopeOptions) (api.TaskSessionResponse, error)
}, sessionID string, channel string, cursor int64, scope client.TaskSessionScopeOptions, jsonLines bool) error {
	for {
		err := control.FollowTaskSessionOutputs(cmd.Context(), sessionID, channel, cursor, scope, func(record api.ChannelRecordResponse) error {
			if record.Sequence > cursor {
				cursor = record.Sequence
			}
			if jsonLines {
				return format.JSONLines(cmd.OutOrStdout(), []api.ChannelRecordResponse{record})
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), string(record.Data))
			return err
		})
		if errors.Is(err, context.Canceled) || errors.Is(cmd.Context().Err(), context.Canceled) {
			return nil
		}
		if err != nil {
			return err
		}
		session, err := control.GetTaskSession(cmd.Context(), sessionID, scope)
		if err != nil {
			return err
		}
		if taskSessionStatusTerminal(session.Status) {
			return nil
		}
		timer := time.NewTimer(runEventReconnectDelay)
		select {
		case <-cmd.Context().Done():
			timer.Stop()
			if errors.Is(cmd.Context().Err(), context.Canceled) {
				return nil
			}
			return cmd.Context().Err()
		case <-timer.C:
		}
	}
}

func taskSessionStatusTerminal(status string) bool {
	switch strings.TrimSpace(status) {
	case "completed", "failed", "closed", "cancelled", "expired":
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
