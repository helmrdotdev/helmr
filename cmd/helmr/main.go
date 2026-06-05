package main

import (
	"fmt"
	"os"

	"github.com/helmrdotdev/helmr/internal/version"
	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
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
	root.AddCommand(
		initCommand(),
		loginCommand(),
		logoutCommand(),
		deployCommand(),
		promoteCommand(),
		runCommand(),
		projectCommand(),
		envCommand(),
		policyCommand(),
		secretCommand(),
		runtimeCommand(),
		psCommand(),
		showCommand(),
		logsCommand(),
		eventsCommand(),
		resumeCommand(),
	)
	return root
}

func init() {
	cobra.EnableCommandSorting = false
}
