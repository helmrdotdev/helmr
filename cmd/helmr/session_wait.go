package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

type turnWaitOutcome struct {
	Status           string              `json:"status"`
	Result           json.RawMessage     `json:"result,omitempty"`
	Response         json.RawMessage     `json:"response,omitempty"`
	Error            *api.AgentTurnError `json:"error,omitempty"`
	PayloadExpiredAt *time.Time          `json:"payload_expired_at,omitempty"`
}

type turnWaitResult struct {
	Status  string           `json:"status"`
	Outcome *turnWaitOutcome `json:"outcome,omitempty"`
}

func sessionTurnWaitCommand() *cobra.Command {
	var project, environment, timeout string
	var jsonOutput bool
	cmd := &cobra.Command{Use: "wait SESSION_ID TURN_ID --timeout DURATION", Short: "Observe a Turn until settlement or timeout without cancelling work.", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		duration, err := time.ParseDuration(timeout)
		if err != nil || duration <= 0 {
			return errors.New("--timeout must be a positive duration, such as 30s or 10m")
		}
		cp, scope, err := scopedSessionClient(cmd, project, environment)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), duration)
		defer cancel()
		turn, err := waitForSessionTurn(ctx, cp, args[0], args[1], scope)
		result := turnWaitResult{Status: "timeout"}
		if err != nil {
			if cmd.Context().Err() != nil {
				return cmd.Context().Err()
			}
			if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return err
			}
		} else {
			result.Status = "settled"
			result.Outcome = &turnWaitOutcome{Status: turn.Status, Result: turn.Result, Response: turn.Response, Error: turn.Error, PayloadExpiredAt: turn.PayloadExpiredAt}
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), result)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "status: %s\n", result.Status)
		if result.Outcome != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "turn_status: %s\n", result.Outcome.Status)
			if len(result.Outcome.Result) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "result: %s\n", result.Outcome.Result)
			}
			if len(result.Outcome.Response) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "response: %s\n", result.Outcome.Response)
			}
			if result.Outcome.Error != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "error_code: %s\n", result.Outcome.Error.Code)
			}
			if result.Outcome.PayloadExpiredAt != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "payload_expired_at: %s\n", result.Outcome.PayloadExpiredAt.Format(time.RFC3339Nano))
			}
		}
		return nil
	}}
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().StringVar(&timeout, "timeout", "", "Maximum observation time (for example 30s or 10m).")
	_ = cmd.MarkFlagRequired("timeout")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit timeout or the settled outcome as JSON.")
	return cmd
}

func waitForSessionTurn(ctx context.Context, cp *client.Client, sessionID, turnID string, scope client.EnvironmentScopeOptions) (api.AgentTurn, error) {
	for {
		if err := ctx.Err(); err != nil {
			return api.AgentTurn{}, err
		}
		turn, err := cp.RetrieveSessionTurn(ctx, sessionID, turnID, scope)
		if err != nil {
			return api.AgentTurn{}, err
		}
		if err := ctx.Err(); err != nil {
			return api.AgentTurn{}, err
		}
		if turn.ID != turnID || turn.SessionID != sessionID {
			return api.AgentTurn{}, errors.New("turn response changed identity")
		}
		switch turn.Status {
		case "completed", "failed", "interrupted", "cancelled":
			return turn, nil
		case "queued", "running", "finalizing":
		default:
			return api.AgentTurn{}, fmt.Errorf("unknown Turn status %q", turn.Status)
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return api.AgentTurn{}, ctx.Err()
		case <-timer.C:
		}
	}
}
