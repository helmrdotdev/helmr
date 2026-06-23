package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/helmrdotdev/helmr/internal/version"
	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var exitErr exitCodeError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.code)
		}
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "helmr",
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.PersistentFlags().StringP("api-url", "a", "", "Helmr control API URL. Defaults to HELMR_API_URL or saved login.")
	root.AddCommand(
		initCommand(),
		loginCommand(),
		logoutCommand(),
		whoamiCommand(),
		deployCommand(),
		deploymentCommand(),
		sandboxCommand(),
		taskCommand(),
		runCommand(),
		sessionCommand(),
		workspaceCommand(),
		projectCommand(),
		envCommand(),
		secretCommand(),
		waitpointCommand(),
	)
	return root
}

func init() {
	cobra.EnableCommandSorting = false
}
