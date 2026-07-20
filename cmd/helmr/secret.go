package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cli/format"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

func secretCommand() *cobra.Command {
	secret := &cobra.Command{
		Use:   "secret",
		Short: "Manage remote secrets.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	secret.AddCommand(
		secretListCommand(),
		secretGetCommand(),
		secretSetCommand(),
		secretRevokeCommand(),
	)
	return secret
}

func secretListCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List remote secrets.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			response, err := control.ListSecrets(cmd.Context(), secretOptions(projectID, environmentID))
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			for _, secret := range response.Secrets {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", secret.Name, secret.State, secret.CreatedAt.Format(apiTimeFormat))
			}
			return nil
		},
	}
	addSecretScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func secretGetCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "get NAME",
		Short: "Show remote secret metadata.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			secret, err := control.GetSecret(cmd.Context(), args[0], secretOptions(projectID, environmentID))
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), secret)
			}
			return writeSecret(cmd.OutOrStdout(), secret)
		},
	}
	addSecretScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func secretSetCommand() *cobra.Command {
	var valueFlag string
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "set NAME [VALUE]",
		Short: "Create or update a remote secret.",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 2 && valueFlag != "" {
				return errors.New("secret value cannot be provided both positionally and with --value")
			}
			value := valueFlag
			if len(args) == 2 {
				value = args[1]
			}
			if len(args) == 1 && valueFlag == "" {
				bytes, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return err
				}
				value = string(bytes)
			}
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			secret, err := control.SetSecret(cmd.Context(), args[0], value, client.SecretOptions(secretOptions(projectID, environmentID)))
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), secret)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", secret.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&valueFlag, "value", "", "Secret value. Reads stdin if omitted.")
	addSecretScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func secretRevokeCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var idempotencyKey string
	var yes bool
	cmd := &cobra.Command{
		Use:   "revoke NAME --yes",
		Short: "Revoke a remote secret.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return errors.New("secret revoke requires --yes")
			}
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			if strings.TrimSpace(idempotencyKey) == "" {
				idempotencyKey = uuid.Must(uuid.NewV7()).String()
			}
			secret, err := control.RevokeSecret(
				cmd.Context(),
				args[0],
				idempotencyKey,
				secretOptions(projectID, environmentID),
			)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", secret.Name)
			return nil
		},
	}
	addSecretScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Stable key for retrying this revocation.")
	cmd.Flags().BoolVar(&yes, "yes", false, "Confirm revocation.")
	return cmd
}

const apiTimeFormat = "2006-01-02T15:04:05Z07:00"

func addSecretScopeFlags(cmd *cobra.Command, projectID *string, environmentID *string) {
	cmd.Flags().StringVarP(projectID, "project", "p", "", "Project slug or ID for this secret.")
	cmd.Flags().StringVarP(environmentID, "env", "e", "", "Environment slug or ID for this secret.")
}

func secretOptions(projectID string, environmentID string) client.SecretOptions {
	return client.SecretOptions{
		ProjectID:     strings.TrimSpace(projectID),
		EnvironmentID: strings.TrimSpace(environmentID),
	}
}

func writeSecret(w io.Writer, secret api.SecretResponse) error {
	fmt.Fprintf(w, "Name: %s\n", secret.Name)
	fmt.Fprintf(w, "State: %s\n", secret.State)
	fmt.Fprintf(w, "Created: %s\n", secret.CreatedAt.Format(apiTimeFormat))
	if secret.RotatedAt != nil {
		fmt.Fprintf(w, "Rotated: %s\n", secret.RotatedAt.Format(apiTimeFormat))
	}
	if secret.RevokedAt != nil {
		fmt.Fprintf(w, "Revoked: %s\n", secret.RevokedAt.Format(apiTimeFormat))
	}
	return nil
}
