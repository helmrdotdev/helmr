package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/spf13/cobra"
)

func commandCommand() *cobra.Command {
	root := &cobra.Command{Use: "command", Short: "Observe and control admitted Computer commands."}
	for _, action := range []string{"get", "wait", "logs", "cancel"} {
		root.AddCommand(commandAction(action))
	}
	return root
}

func commandAction(action string) *cobra.Command {
	var project, environment, timeout, cursor string
	var jsonOutput, follow bool
	var limit int
	cmd := &cobra.Command{Use: action + " COMMAND_ID", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		var duration time.Duration
		if action == "wait" && cmd.Flags().Changed("timeout") {
			var err error
			duration, err = parseDuration(timeout)
			if err != nil {
				return fmt.Errorf("--timeout: %w", err)
			}
		}
		if action == "logs" && cmd.Flags().Changed("limit") && (limit < 1 || limit > 100) {
			return errors.New("--limit must be in [1,100]")
		}
		cp, scope, err := scopedComputerClient(cmd, project, environment)
		if err != nil {
			return err
		}
		id := args[0]
		switch action {
		case "get":
			info, err := cp.RetrieveCommand(cmd.Context(), id, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), info)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", info.ID, info.Status)
			return err
		case "cancel":
			receipt, err := cp.CancelCommand(cmd.Context(), id, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), receipt)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", receipt.TargetID, receipt.Status)
			return err
		case "wait":
			ctx, cancel := cmd.Context(), context.CancelFunc(func() {})
			if duration > 0 {
				ctx, cancel = context.WithTimeout(cmd.Context(), duration)
			}
			defer cancel()
			var outcome *api.CommandOutcome
			for {
				if err = ctx.Err(); err != nil {
					break
				}
				var info api.CommandInfo
				info, err = cp.RetrieveCommand(ctx, id, scope)
				if err != nil {
					break
				}
				if err = ctx.Err(); err != nil {
					break
				}
				if info.Outcome != nil {
					outcome = info.Outcome
					break
				}
				if err = commandPoll(ctx); err != nil {
					break
				}
			}
			if cmd.Context().Err() != nil {
				return cmd.Context().Err()
			}
			if err != nil && !(errors.Is(err, context.DeadlineExceeded) && errors.Is(ctx.Err(), context.DeadlineExceeded)) {
				return err
			}
			result := struct {
				Status  string              `json:"status"`
				Outcome *api.CommandOutcome `json:"outcome,omitempty"`
			}{Status: "timeout", Outcome: outcome}
			if outcome != nil {
				result.Status = "settled"
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), result)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "status: %s\n", result.Status)
			if outcome != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "kind: %s\n", outcome.Kind)
			}
			return nil
		case "logs":
			for {
				page, err := cp.ListCommandLogs(cmd.Context(), id, cursor, limit, scope)
				if err != nil {
					var apiError *httpclient.Error
					if follow && errors.As(err, &apiError) && apiError.Code == "telemetry_lagging" {
						if err := commandPoll(cmd.Context()); err != nil {
							return err
						}
						continue
					}
					return err
				}
				if jsonOutput {
					if !follow || len(page.Logs) > 0 || page.OutputState != "open" {
						if err = writeJSON(cmd.OutOrStdout(), page); err != nil {
							return err
						}
					}
				} else {
					for _, record := range page.Logs {
						if record.Kind == "gap" {
							fmt.Fprintf(cmd.ErrOrStderr(), "[%s output missing: %s–%s]\n", record.Stream, record.FromSequence, record.ThroughSequence)
							continue
						}
						data, err := base64.StdEncoding.DecodeString(record.ContentBase64)
						if err != nil {
							return err
						}
						writer := cmd.OutOrStdout()
						if record.Stream == "stderr" {
							writer = cmd.ErrOrStderr()
						}
						if _, err = writer.Write(data); err != nil {
							return err
						}
					}
					if !follow {
						fmt.Fprintf(cmd.ErrOrStderr(), "output_state: %s\n", page.OutputState)
						if page.NextCursor != "" {
							fmt.Fprintf(cmd.ErrOrStderr(), "next_cursor: %s\n", page.NextCursor)
						}
					}
				}
				if !follow {
					return nil
				}
				if len(page.Logs) > 0 {
					if page.NextCursor == "" || page.NextCursor == cursor {
						return errors.New("command log cursor did not advance")
					}
					cursor = page.NextCursor
					continue
				}
				switch page.OutputState {
				case "closed":
					return nil
				case "unavailable":
					return errors.New("command output completeness is unavailable")
				case "open":
				default:
					return errors.New("invalid command output state")
				}
				if err = commandPoll(cmd.Context()); err != nil {
					return err
				}
			}
		}
		return nil
	}}
	addScopeFlags(cmd, &project, &environment)
	jsonHelp := "Emit one JSON object."
	if action == "logs" {
		jsonHelp = "Emit one JSON page, or pages with output and the final state when following logs."
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, jsonHelp)
	switch action {
	case "wait":
		cmd.Short = "Observe settlement without cancelling work."
		cmd.Flags().StringVar(&timeout, "timeout", "", "Maximum observation duration; without it, wait until the command settles.")
	case "logs":
		cmd.Short = "Read command output with explicit gaps and completeness."
		cmd.Flags().StringVar(&cursor, "cursor", "", "Continue after this cursor.")
		cmd.Flags().IntVar(&limit, "limit", 0, "Page size (1–100).")
		cmd.Flags().BoolVar(&follow, "follow", false, "Follow until output closes; fail if completeness is unavailable.")
	case "get":
		cmd.Short = "Get command status and its settled outcome."
	case "cancel":
		cmd.Short = "Request cancellation and return acceptance."
	}
	return cmd
}

func commandPoll(ctx context.Context) error {
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
