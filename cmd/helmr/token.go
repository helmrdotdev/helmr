package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

func tokenCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Work with external completion tokens.",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(
		tokenGetCommand(),
		tokenCompleteCommand(),
		tokenCancelCommand(),
	)
	return cmd
}

func tokenGetCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "get TOKEN",
		Short: "Show an external completion token.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			scope, err := environmentScopeForClient(cmd.Context(), controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			token, err := controlPlane.GetToken(cmd.Context(), args[0], client.TokenScopeOptions(scope))
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), token)
			}
			writeTokenSummary(cmd, token)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func tokenCompleteCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var dataJSON string
	var idempotencyKey string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "complete TOKEN --data-json JSON",
		Short: "Complete an external token with JSON data.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data := json.RawMessage(strings.TrimSpace(dataJSON))
			if len(data) == 0 || !json.Valid(data) {
				return fmt.Errorf("--data-json must be valid JSON")
			}
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			scope, err := environmentScopeForClient(cmd.Context(), controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := controlPlane.CompleteToken(cmd.Context(), args[0], api.CompleteTokenRequest{
				Result: data, IdempotencyKey: strings.TrimSpace(idempotencyKey),
			}, client.TokenScopeOptions(scope))
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), response)
			}
			writeTokenSummary(cmd, response)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&dataJSON, "data-json", "", "JSON completion payload.")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for safe retries.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	_ = cmd.MarkFlagRequired("data-json")
	return cmd
}

func tokenCancelCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var idempotencyKey string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "cancel TOKEN",
		Short: "Cancel a pending external completion token.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			scope, err := environmentScopeForClient(cmd.Context(), controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			token, err := controlPlane.CancelToken(cmd.Context(), args[0], api.CancelTokenRequest{
				IdempotencyKey: strings.TrimSpace(idempotencyKey),
			}, client.TokenScopeOptions(scope))
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), token)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", token.ID, token.Status)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for safe retries.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func writeTokenSummary(cmd *cobra.Command, token api.TokenResponse) {
	fmt.Fprintf(cmd.OutOrStdout(), "Token:       %s\n", token.ID)
	fmt.Fprintf(cmd.OutOrStdout(), "Status:      %s\n", token.Status)
	fmt.Fprintf(cmd.OutOrStdout(), "Timeout:     %s\n", tokenTimeoutAt(token))
}

func tokenTimeoutAt(token api.TokenResponse) string {
	return token.TimeoutAt.Format("2006-01-02T15:04:05Z07:00")
}
