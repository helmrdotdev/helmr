package main

import (
	"fmt"
	"os"

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
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		initCommand(),
		loginCommand(),
		logoutCommand(),
		deployCommand(),
		runCommand(),
		secretCommand(),
		workerCommand(),
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
