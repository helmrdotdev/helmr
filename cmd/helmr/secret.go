package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

func secretCommand() *cobra.Command {
	secret := &cobra.Command{Use: "secret", Short: "Manage remote secrets."}
	secret.AddCommand(secretSetCommand())
	return secret
}

func secretSetCommand() *cobra.Command {
	var valueFlag string
	var projectID string
	var environmentID string
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
			control, err := controlClient()
			if err != nil {
				return err
			}
			secret, err := control.SetSecret(cmd.Context(), args[0], value, client.SetSecretOptions{
				ProjectID:     strings.TrimSpace(projectID),
				EnvironmentID: strings.TrimSpace(environmentID),
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", secret.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&valueFlag, "value", "", "Secret value. Reads stdin if omitted.")
	cmd.Flags().StringVar(&projectID, "project", "", "Project ID for this secret.")
	cmd.Flags().StringVar(&environmentID, "environment", "", "Environment ID for this secret.")
	return cmd
}
