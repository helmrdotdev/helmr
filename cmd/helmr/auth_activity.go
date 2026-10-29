package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"os"
	"time"
)

var interactiveCLI = func(cmd *cobra.Command) bool {
	in, ok := cmd.InOrStdin().(*os.File)
	return ok && term.IsTerminal(int(in.Fd()))
}

func recordCLIActivity(cmd *cobra.Command) {
	if cmd == nil || !interactiveCLI(cmd) {
		return
	}
	human, ok := cmd.Context().Value(humanCLIKey{}).(humanCLIClient)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
	defer cancel()
	// The operation has already succeeded. Failure to extend idle expiry must not
	// turn that success into an error that encourages replaying a mutation.
	if err := human.client.RecordHumanActivity(ctx); err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "The command succeeded, but login activity could not be recorded.")
	}
}

func confirmCLIIfNeeded(cmd *cobra.Command, err error) {
	if cmd == nil {
		return
	}
	remote, ok := errors.AsType[*httpclient.Error](err)
	if !ok || remote.Code != "recent_authentication_required" {
		return
	}
	human, ok := cmd.Context().Value(humanCLIKey{}).(humanCLIClient)
	if !ok {
		return
	}
	if !interactiveCLI(cmd) {
		fmt.Fprintln(cmd.ErrOrStderr(), "Run helmr login --confirm, then retry the command explicitly.")
		return
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "Confirm this CLI login in your browser. The command will not be retried automatically.")
	if confirmErr := deviceLogin(cmd, human.origin, true, false, human.client); confirmErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Confirmation failed: %v\n", confirmErr)
		return
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "Login confirmed. Retry the command explicitly.")
}
