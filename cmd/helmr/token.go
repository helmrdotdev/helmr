package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cli/format"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

func tokenCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Work with external completion tokens.",
	}
	cmd.AddCommand(
		tokenCreateCommand(),
		tokenGetCommand(),
		tokenCompleteCommand(),
		tokenCancelCommand(),
	)
	return cmd
}

func tokenCreateCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var timeout string
	var metadataJSON string
	var tags []string
	var idempotencyKey string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create an external completion token.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			metadata, err := parseTokenMetadataJSON(metadataJSON)
			if err != nil {
				return err
			}
			timeoutJSON, err := tokenTimeoutJSON(timeout)
			if err != nil {
				return err
			}
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			token, err := control.CreateToken(cmd.Context(), api.CreateTokenRequest{
				ProjectID:      projectID,
				EnvironmentID:  environmentID,
				Timeout:        timeoutJSON,
				Tags:           cleanTags(tags),
				Metadata:       metadata,
				IdempotencyKey: strings.TrimSpace(idempotencyKey),
			})
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), token)
			}
			writeTokenSummary(cmd, token)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&timeout, "timeout", "", "Token timeout duration, for example 7d or 30m.")
	cmd.Flags().StringVar(&metadataJSON, "metadata-json", "", "Inline metadata JSON literal.")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "Token tag. May be repeated.")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for safe retries.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
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
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			token, err := control.GetToken(cmd.Context(), args[0], client.TokenScopeOptions{ProjectID: projectID, EnvironmentID: environmentID})
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), token)
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
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			response, err := control.CompleteToken(cmd.Context(), args[0], api.CompleteTokenRequest{Data: data}, client.TokenScopeOptions{ProjectID: projectID, EnvironmentID: environmentID})
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "token_id: %s\n", response.Token.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "token_status: %s\n", response.Token.Status)
			fmt.Fprintf(cmd.OutOrStdout(), "completion_status: %s\n", response.Status)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&dataJSON, "data-json", "", "JSON completion payload.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	_ = cmd.MarkFlagRequired("data-json")
	return cmd
}

func tokenCancelCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "cancel TOKEN",
		Short: "Cancel a pending external completion token.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			token, err := control.CancelToken(cmd.Context(), args[0], client.TokenScopeOptions{ProjectID: projectID, EnvironmentID: environmentID})
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), token)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", token.ID, token.Status)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func tokenTimeoutJSON(timeout string) (json.RawMessage, error) {
	timeout = strings.TrimSpace(timeout)
	if timeout == "" {
		return nil, nil
	}
	encoded, err := json.Marshal(timeout)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func parseTokenMetadataJSON(raw string) (json.RawMessage, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	value := json.RawMessage(raw)
	if !json.Valid(value) {
		return nil, fmt.Errorf("--metadata-json must be valid JSON")
	}
	return value, nil
}

func writeTokenSummary(cmd *cobra.Command, token api.TokenResponse) {
	fmt.Fprintf(cmd.OutOrStdout(), "Token:       %s\n", token.ID)
	fmt.Fprintf(cmd.OutOrStdout(), "Status:      %s\n", token.Status)
	fmt.Fprintf(cmd.OutOrStdout(), "Timeout:     %s\n", tokenTimeoutAt(token))
	if token.CallbackURL != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "Callback:    %s\n", token.CallbackURL)
	}
	if token.PublicAccessToken != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "PublicToken: %s\n", token.PublicAccessToken)
	}
}

func tokenTimeoutAt(token api.TokenResponse) string {
	if token.TimeoutAt == nil {
		return "-"
	}
	return token.TimeoutAt.Format("2006-01-02T15:04:05Z07:00")
}
