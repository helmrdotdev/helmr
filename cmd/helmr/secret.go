package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
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
		secretCreateCommand(),
		secretRotateCommand(),
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
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			scope, err := environmentScopeForClient(cmd.Context(), controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := controlPlane.ListSecrets(cmd.Context(), secretScope(scope))
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), response)
			}
			for _, secret := range response.Secrets {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", secret.Name, secret.Status, secret.CreatedAt.Format(apiTimeFormat))
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
		Use:   "get SECRET_ID",
		Short: "Show remote secret metadata.",
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
			secret, err := controlPlane.RetrieveSecret(cmd.Context(), args[0], secretScope(scope))
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), secret)
			}
			return writeSecret(cmd.OutOrStdout(), secret)
		},
	}
	addSecretScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func secretCreateCommand() *cobra.Command {
	var valueFlag string
	var projectID string
	var environmentID string
	var idempotencyKey string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "create NAME [VALUE]",
		Short: "Create a remote secret.",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			value, err := readSecretValue(cmd, args, valueFlag)
			if err != nil {
				return err
			}
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			scope, err := environmentScopeForClient(cmd.Context(), controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			secret, err := controlPlane.CreateSecret(
				cmd.Context(),
				args[0],
				value,
				idempotencyKey,
				secretScope(scope),
			)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), secret)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", secret.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&valueFlag, "value", "", "Secret value. Reads stdin if omitted.")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Stable key for retrying this creation.")
	addSecretScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func secretRotateCommand() *cobra.Command {
	var valueFlag string
	var projectID string
	var environmentID string
	var idempotencyKey string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "rotate SECRET_ID [VALUE]",
		Short: "Rotate a remote secret.",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			value, err := readSecretValue(cmd, args, valueFlag)
			if err != nil {
				return err
			}
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			scope, err := environmentScopeForClient(cmd.Context(), controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			if strings.TrimSpace(idempotencyKey) == "" {
				idempotencyKey = uuid.NewV7().String()
			}
			record, err := controlPlane.RotateSecret(
				cmd.Context(),
				args[0],
				value,
				idempotencyKey,
				secretScope(scope),
			)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), record)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", record.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&valueFlag, "value", "", "Secret value. Reads stdin if omitted.")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Stable key for retrying this rotation.")
	addSecretScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func readSecretValue(cmd *cobra.Command, args []string, valueFlag string) (string, error) {
	if len(args) == 2 && valueFlag != "" {
		return "", errors.New("secret value cannot be provided both positionally and with --value")
	}
	if len(args) == 2 {
		return args[1], nil
	}
	if valueFlag != "" {
		return valueFlag, nil
	}
	value, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return "", err
	}
	return string(value), nil
}

func secretRevokeCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var idempotencyKey string
	var yes bool
	cmd := &cobra.Command{
		Use:   "revoke SECRET_ID --yes",
		Short: "Revoke a remote secret.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return errors.New("secret revoke requires --yes")
			}
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			scope, err := environmentScopeForClient(cmd.Context(), controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			if strings.TrimSpace(idempotencyKey) == "" {
				idempotencyKey = uuid.NewV7().String()
			}
			secret, err := controlPlane.RevokeSecret(
				cmd.Context(),
				args[0],
				idempotencyKey,
				secretScope(scope),
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

func secretScope(scope client.EnvironmentScopeOptions) client.SecretOptions {
	return client.SecretOptions{ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID}
}

func writeSecret(w io.Writer, secret api.SecretResponse) error {
	fmt.Fprintf(w, "Name: %s\n", secret.Name)
	fmt.Fprintf(w, "Status: %s\n", secret.Status)
	fmt.Fprintf(w, "Created: %s\n", secret.CreatedAt.Format(apiTimeFormat))
	if secret.RotatedAt != nil {
		fmt.Fprintf(w, "Rotated: %s\n", secret.RotatedAt.Format(apiTimeFormat))
	}
	if secret.RevokedAt != nil {
		fmt.Fprintf(w, "Revoked: %s\n", secret.RevokedAt.Format(apiTimeFormat))
	}
	return nil
}
