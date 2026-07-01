package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cli/format"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	"golang.org/x/term"
)

func workspaceCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workspace",
		Short: "Work with durable workspaces.",
	}
	cmd.AddCommand(
		workspaceCreateCommand(),
		workspaceListCommand(),
		workspaceGetCommand(),
		workspaceUpdateCommand(),
		workspaceDeleteCommand(),
		workspaceOpenCommand(),
		workspaceMaterializeCommand(),
		workspaceConnectCommand(),
		workspaceStopCommand(),
		workspaceExecCommand(),
		workspaceShellCommand(),
		workspacePtyCommand(),
	)
	return cmd
}

func workspaceCreateCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var sandboxID string
	var externalID string
	var metadataJSON string
	var tags []string
	var idempotencyKey string
	var idempotencyKeyTTL string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a durable workspace.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			metadata, err := parseOptionalJSON("", metadataJSON, "--metadata")
			if err != nil {
				return err
			}
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := workspaceScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := control.CreateWorkspace(cmd.Context(), api.WorkspaceCreateRequest{
				ProjectID:         scope.ProjectID,
				EnvironmentID:     scope.EnvironmentID,
				SandboxID:         strings.TrimSpace(sandboxID),
				ExternalID:        strings.TrimSpace(externalID),
				Metadata:          metadata,
				Tags:              cleanTags(tags),
				IdempotencyKey:    strings.TrimSpace(idempotencyKey),
				IdempotencyKeyTTL: strings.TrimSpace(idempotencyKeyTTL),
			})
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			writeWorkspaceHandle(cmd, control, response.Workspace)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&sandboxID, "sandbox", "", "Sandbox ID.")
	cmd.Flags().StringVar(&externalID, "external-id", "", "External ID.")
	cmd.Flags().StringVar(&metadataJSON, "metadata-json", "", "Inline metadata JSON literal.")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "Workspace tag. Repeat for multiple tags.")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for safe retries.")
	cmd.Flags().StringVar(&idempotencyKeyTTL, "idempotency-key-ttl", "", "Duration to retain the idempotency key.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	_ = cmd.MarkFlagRequired("sandbox")
	return cmd
}

func workspaceListCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List workspaces.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := workspaceScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := control.ListWorkspaces(cmd.Context(), scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			for _, workspace := range response.Workspaces {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", workspace.ID, workspace.State, workspace.SandboxID, workspace.CurrentVersionID)
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func workspaceGetCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "get WORKSPACE",
		Short: "Show workspace details.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			workspace, err := loadWorkspace(cmd, args[0], projectID, environmentID)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), workspace)
			}
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			writeWorkspaceHandle(cmd, control, workspace.Workspace)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func workspaceUpdateCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var metadataJSON string
	var tags []string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "update WORKSPACE",
		Short: "Update workspace metadata.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			metadata, err := parseOptionalJSON("", metadataJSON, "--metadata")
			if err != nil {
				return err
			}
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := workspaceScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := control.UpdateWorkspace(cmd.Context(), args[0], api.WorkspacePatchRequest{Metadata: metadata, Tags: cleanTags(tags)}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			writeWorkspaceHandle(cmd, control, response.Workspace)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&metadataJSON, "metadata-json", "", "Inline metadata JSON literal.")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "Workspace tag. Repeat for multiple tags.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func workspaceDeleteCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete WORKSPACE --yes",
		Short: "Delete a workspace.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return fmt.Errorf("workspace delete requires --yes")
			}
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := workspaceScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			if err := control.DeleteWorkspace(cmd.Context(), args[0], scope); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s deleted\n", args[0])
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&yes, "yes", false, "Confirm deletion.")
	return cmd
}

func workspaceOpenCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "open WORKSPACE",
		Short: "Print the workspace console URL.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			workspace, err := loadWorkspace(cmd, args[0], projectID, environmentID)
			if err != nil {
				return err
			}
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			url := consoleURL(cmd, control, "/workspaces/"+workspace.Workspace.ID)
			if url == "" {
				return errors.New("console URL is not available for this authentication context")
			}
			response := struct {
				Workspace  api.WorkspaceResponse `json:"workspace"`
				ConsoleURL string                `json:"console_url,omitempty"`
			}{
				Workspace:  workspace.Workspace,
				ConsoleURL: url,
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			fmt.Fprintln(cmd.OutOrStdout(), response.ConsoleURL)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func workspaceMaterializeCommand() *cobra.Command {
	return workspaceMountCommand("materialize", "Ensure a workspace mount exists.", func(ctx context.Context, control *client.Client, workspaceID string, scope client.WorkspaceScopeOptions) (api.WorkspaceMountResponse, error) {
		return control.MaterializeWorkspace(ctx, workspaceID, scope)
	})
}

func workspaceConnectCommand() *cobra.Command {
	return workspaceMountCommand("connect", "Connect to a workspace mount.", func(ctx context.Context, control *client.Client, workspaceID string, scope client.WorkspaceScopeOptions) (api.WorkspaceMountResponse, error) {
		return control.ConnectWorkspace(ctx, workspaceID, scope)
	})
}

func workspaceMountCommand(use string, short string, call func(context.Context, *client.Client, string, client.WorkspaceScopeOptions) (api.WorkspaceMountResponse, error)) *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   use + " WORKSPACE",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := workspaceScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := call(cmd.Context(), control, args[0], scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s\n", response.ID, response.WorkspaceID, response.State)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func workspaceStopCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var idempotencyKey string
	var idempotencyKeyTTL string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "stop WORKSPACE",
		Short: "Stop the live workspace mount for a workspace.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := workspaceScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := control.StopWorkspace(cmd.Context(), args[0], api.WorkspaceStopRequest{
				IdempotencyKey:    strings.TrimSpace(idempotencyKey),
				IdempotencyKeyTTL: strings.TrimSpace(idempotencyKeyTTL),
			}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", response.WorkspaceID, response.State)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for safe retries.")
	cmd.Flags().StringVar(&idempotencyKeyTTL, "idempotency-key-ttl", "", "Duration to retain the idempotency key.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func workspaceExecCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exec",
		Short: "Run commands in a workspace.",
	}
	cmd.RunE = runWorkspaceExec
	cmd.Args = cobra.MinimumNArgs(2)
	cmd.Flags().Bool("detach", false, "Start the exec and return immediately.")
	cmd.Flags().String("cwd", "", "Working directory.")
	cmd.Flags().StringArray("set-env", nil, "Set an environment variable as KEY=VALUE. Repeat for multiple values.")
	cmd.Flags().String("idempotency-key", "", "Idempotency key for safe retries.")
	cmd.Flags().StringP("project", "p", "", "Project slug or ID.")
	cmd.Flags().StringP("env", "e", "", "Environment slug or ID.")
	cmd.Flags().Bool("json", false, "Emit one JSON object.")
	cmd.AddCommand(workspaceExecListCommand(), workspaceExecGetCommand(), workspaceExecLogsCommand(), workspaceExecWaitCommand())
	return cmd
}

func runWorkspaceExec(cmd *cobra.Command, args []string) error {
	dashIndex := cmd.Flags().ArgsLenAtDash()
	if dashIndex != 1 || len(args) <= dashIndex {
		return fmt.Errorf("usage: helmr workspace exec WORKSPACE -- COMMAND [ARGS...]")
	}
	workspaceID := args[0]
	remoteCommand := args[dashIndex:]
	control, err := controlClient(cmd)
	if err != nil {
		return err
	}
	projectID, _ := cmd.Flags().GetString("project")
	environmentID, _ := cmd.Flags().GetString("env")
	scope, err := workspaceScopeForClient(control, projectID, environmentID)
	if err != nil {
		return err
	}
	envPairs, _ := cmd.Flags().GetStringArray("set-env")
	env, err := envMap(envPairs)
	if err != nil {
		return err
	}
	detached, _ := cmd.Flags().GetBool("detach")
	jsonOutput, _ := cmd.Flags().GetBool("json")
	if jsonOutput && !detached {
		return errors.New("workspace exec --json requires --detach")
	}
	cwd, _ := cmd.Flags().GetString("cwd")
	idempotencyKey, _ := cmd.Flags().GetString("idempotency-key")
	response, err := control.CreateWorkspaceExec(cmd.Context(), workspaceID, api.WorkspaceExecCreateRequest{
		Command:        remoteCommand,
		Cwd:            strings.TrimSpace(cwd),
		Env:            env,
		Detached:       detached,
		IdempotencyKey: strings.TrimSpace(idempotencyKey),
	}, scope)
	if err != nil {
		return err
	}
	if detached {
		if jsonOutput {
			return format.JSON(cmd.OutOrStdout(), response)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "workspace_id: %s\nexec_id: %s\n", response.Exec.WorkspaceID, response.Exec.ID)
		if url := consoleURL(cmd, control, "/workspaces/"+response.Exec.WorkspaceID+"/execs/"+response.Exec.ID); url != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "console_url: %s\n", url)
		}
		return nil
	}
	if err := runWorkspaceExecForeground(cmd, control, workspaceID, response.Exec, scope); err != nil {
		return err
	}
	final, err := control.GetWorkspaceExec(cmd.Context(), workspaceID, response.Exec.ID, scope)
	if err != nil {
		return err
	}
	if !workspaceExecStateTerminal(final.Exec.State) {
		return fmt.Errorf("workspace exec stream ended before terminal state: %s", final.Exec.State)
	}
	if final.Exec.ExitCode != nil {
		if code := int(*final.Exec.ExitCode); code != 0 {
			return exitCodeError{code: code}
		}
		return nil
	}
	return fmt.Errorf("workspace exec ended without an exit code: %s", final.Exec.State)
}

func workspaceExecListCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list WORKSPACE",
		Short: "List workspace execs.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := workspaceScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := control.ListWorkspaceExecs(cmd.Context(), args[0], scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			for _, exec := range response.Execs {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", exec.ID, exec.State, exec.Cwd)
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func workspaceExecGetCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "get WORKSPACE EXEC",
		Short: "Show workspace exec details.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			exec, err := loadWorkspaceExec(cmd, args[0], args[1], projectID, environmentID)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), exec)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Exec:      %s\n", exec.Exec.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "Workspace: %s\n", exec.Exec.WorkspaceID)
			fmt.Fprintf(cmd.OutOrStdout(), "State:     %s\n", exec.Exec.State)
			if exec.Exec.ExitCode != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Exit:      %d\n", *exec.Exec.ExitCode)
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func workspaceExecLogsCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var cursor int64
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs WORKSPACE EXEC",
		Short: "Read workspace exec logs.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := workspaceScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			stdoutCursor, err := printWorkspaceExecStream(cmd, control, args[0], args[1], "stdout", cursor, scope, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			stderrCursor, err := printWorkspaceExecStream(cmd, control, args[0], args[1], "stderr", cursor, scope, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if follow {
				return followWorkspaceExecOutput(cmd.Context(), cmd, control, args[0], args[1], stdoutCursor, stderrCursor, scope, false)
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().Int64Var(&cursor, "cursor", 0, "Read chunks after this offset.")
	cmd.Flags().BoolVar(&follow, "follow", false, "Follow live output.")
	return cmd
}

func workspaceExecWaitCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "wait WORKSPACE EXEC",
		Short: "Wait for a workspace exec to finish.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := workspaceScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			if _, _, err := followWorkspaceExecStreamUntilTerminal(cmd.Context(), control, args[0], args[1], "stdout", 0, scope, io.Discard); err != nil {
				return err
			}
			response, err := control.GetWorkspaceExec(cmd.Context(), args[0], args[1], scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", response.Exec.ID, response.Exec.State)
			if response.Exec.ExitCode != nil && *response.Exec.ExitCode != 0 {
				return exitCodeError{code: int(*response.Exec.ExitCode)}
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func workspaceShellCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var cwd string
	cmd := &cobra.Command{
		Use:   "shell WORKSPACE",
		Short: "Open a shell in a workspace.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cols, rows := terminalSize()
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := workspaceScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := control.CreateWorkspacePty(cmd.Context(), args[0], api.WorkspacePtyCreateRequest{Cwd: strings.TrimSpace(cwd), Cols: cols, Rows: rows}, scope)
			if err != nil {
				return err
			}
			return connectWorkspacePty(cmd, control, args[0], response.Pty.ID, response.Pty.InputCursor, response.Pty.OutputCursor, scope)
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&cwd, "cwd", "", "Working directory.")
	return cmd
}

func workspacePtyCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "pty", Short: "Work with workspace PTY sessions."}
	cmd.AddCommand(workspacePtyCreateCommand(), workspacePtyConnectCommand(), workspacePtyCloseCommand())
	return cmd
}

func workspacePtyCreateCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var cwd string
	var cols int32
	var rows int32
	var idempotencyKey string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "create WORKSPACE",
		Short: "Create a workspace PTY.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := workspaceScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			if cols == 0 || rows == 0 {
				cols, rows = terminalSize()
			}
			response, err := control.CreateWorkspacePty(cmd.Context(), args[0], api.WorkspacePtyCreateRequest{
				Cwd:            strings.TrimSpace(cwd),
				Cols:           cols,
				Rows:           rows,
				IdempotencyKey: strings.TrimSpace(idempotencyKey),
			}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "workspace_id: %s\npty_id: %s\n", response.Pty.WorkspaceID, response.Pty.ID)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&cwd, "cwd", "", "Working directory.")
	cmd.Flags().Int32Var(&cols, "cols", 0, "PTY columns.")
	cmd.Flags().Int32Var(&rows, "rows", 0, "PTY rows.")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for safe retries.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func workspacePtyConnectCommand() *cobra.Command {
	var projectID string
	var environmentID string
	cmd := &cobra.Command{
		Use:   "connect WORKSPACE PTY",
		Short: "Connect to a workspace PTY.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := workspaceScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			pty, err := control.GetWorkspacePty(cmd.Context(), args[0], args[1], scope)
			if err != nil {
				return err
			}
			return connectWorkspacePty(cmd, control, args[0], args[1], pty.Pty.InputCursor, pty.Pty.OutputCursor, scope)
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	return cmd
}

func workspacePtyCloseCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "close WORKSPACE PTY",
		Short: "Close a workspace PTY.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := workspaceScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := control.CloseWorkspacePty(cmd.Context(), args[0], args[1], scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", response.Pty.ID, response.Pty.State)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func loadWorkspace(cmd *cobra.Command, workspaceID string, projectID string, environmentID string) (api.WorkspaceEnvelope, error) {
	control, err := controlClient(cmd)
	if err != nil {
		return api.WorkspaceEnvelope{}, err
	}
	scope, err := workspaceScopeForClient(control, projectID, environmentID)
	if err != nil {
		return api.WorkspaceEnvelope{}, err
	}
	return control.GetWorkspace(cmd.Context(), workspaceID, scope)
}

func loadWorkspaceExec(cmd *cobra.Command, workspaceID string, execID string, projectID string, environmentID string) (api.WorkspaceExecEnvelope, error) {
	control, err := controlClient(cmd)
	if err != nil {
		return api.WorkspaceExecEnvelope{}, err
	}
	scope, err := workspaceScopeForClient(control, projectID, environmentID)
	if err != nil {
		return api.WorkspaceExecEnvelope{}, err
	}
	return control.GetWorkspaceExec(cmd.Context(), workspaceID, execID, scope)
}

func writeWorkspaceHandle(cmd *cobra.Command, control *client.Client, workspace api.WorkspaceResponse) {
	fmt.Fprintf(cmd.OutOrStdout(), "workspace_id: %s\nstate: %s\nsandbox_id: %s\n", workspace.ID, workspace.State, workspace.SandboxID)
	if url := consoleURL(cmd, control, "/workspaces/"+workspace.ID); url != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "console_url: %s\n", url)
	}
}

func consoleURL(cmd *cobra.Command, control *client.Client, path string) string {
	if strings.TrimSpace(os.Getenv(helmrAPIKeyEnv)) != "" {
		return ""
	}
	me, err := control.GetMe(cmd.Context())
	if err != nil || strings.TrimSpace(me.PublicURL) == "" {
		return ""
	}
	base := strings.TrimRight(me.PublicURL, "/")
	return base + "/" + strings.TrimLeft(path, "/")
}

func envMap(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, err := splitKeyValue(pair, "set-env")
		if err != nil {
			return nil, err
		}
		env[key] = value
	}
	return env, nil
}

func printWorkspaceExecStream(cmd *cobra.Command, control *client.Client, workspaceID string, execID string, stream string, cursor int64, scope client.WorkspaceScopeOptions, writer io.Writer) (int64, error) {
	response, err := control.ListWorkspaceExecStream(cmd.Context(), workspaceID, execID, stream, cursor, scope)
	if err != nil {
		return cursor, err
	}
	nextCursor := cursor
	for _, chunk := range response.Chunks {
		if _, err := writer.Write(chunk.Data); err != nil {
			return nextCursor, err
		}
		if chunk.OffsetEnd > nextCursor {
			nextCursor = chunk.OffsetEnd
		}
	}
	return nextCursor, nil
}

func followWorkspaceExecOutput(ctx context.Context, cmd *cobra.Command, control *client.Client, workspaceID string, execID string, stdoutCursor int64, stderrCursor int64, scope client.WorkspaceScopeOptions, stopOnTerminal bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var sawTerminal bool
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		cursor, terminal, err := followWorkspaceExecStreamUntilTerminal(groupCtx, control, workspaceID, execID, "stdout", stdoutCursor, scope, cmd.OutOrStdout())
		mu.Lock()
		stdoutCursor = cursor
		if terminal {
			sawTerminal = true
		}
		cancelledByTerminal := stopOnTerminal && sawTerminal && errors.Is(err, context.Canceled)
		mu.Unlock()
		if stopOnTerminal && terminal {
			cancel()
		}
		if cancelledByTerminal {
			return nil
		}
		return err
	})
	group.Go(func() error {
		cursor, terminal, err := followWorkspaceExecStreamUntilTerminal(groupCtx, control, workspaceID, execID, "stderr", stderrCursor, scope, cmd.ErrOrStderr())
		mu.Lock()
		stderrCursor = cursor
		if terminal {
			sawTerminal = true
		}
		cancelledByTerminal := stopOnTerminal && sawTerminal && errors.Is(err, context.Canceled)
		mu.Unlock()
		if stopOnTerminal && terminal {
			cancel()
		}
		if cancelledByTerminal {
			return nil
		}
		return err
	})
	if err := group.Wait(); err != nil {
		return err
	}
	mu.Lock()
	terminal := sawTerminal
	stdout := stdoutCursor
	stderr := stderrCursor
	mu.Unlock()
	if stopOnTerminal && terminal {
		if _, err := printWorkspaceExecStream(cmd, control, workspaceID, execID, "stdout", stdout, scope, cmd.OutOrStdout()); err != nil {
			return err
		}
		if _, err := printWorkspaceExecStream(cmd, control, workspaceID, execID, "stderr", stderr, scope, cmd.ErrOrStderr()); err != nil {
			return err
		}
	}
	return nil
}

func followWorkspaceExecStreamUntilTerminal(ctx context.Context, control *client.Client, workspaceID string, execID string, stream string, cursor int64, scope client.WorkspaceScopeOptions, writer io.Writer) (int64, bool, error) {
	for {
		terminal := false
		err := control.FollowWorkspaceExecStream(ctx, workspaceID, execID, stream, cursor, scope, func(event client.WorkspaceStreamEvent) error {
			nextCursor, eventTerminal, err := writeWorkspaceExecStreamEvent(writer, event, cursor)
			if err != nil {
				return err
			}
			if nextCursor > cursor {
				cursor = nextCursor
			}
			if eventTerminal {
				terminal = true
			}
			return nil
		})
		if err != nil {
			return cursor, terminal, err
		}
		if terminal {
			return cursor, true, nil
		}
		select {
		case <-ctx.Done():
			return cursor, false, ctx.Err()
		case <-time.After(runEventReconnectDelay):
		}
	}
}

func writeWorkspaceExecStreamEvent(writer io.Writer, event client.WorkspaceStreamEvent, cursor int64) (int64, bool, error) {
	if event.Error != nil {
		return cursor, false, workspaceStreamEventError(event.Error)
	}
	if event.Terminal != nil {
		if event.Terminal.Cursor > cursor {
			cursor = event.Terminal.Cursor
		}
		return cursor, true, nil
	}
	if len(event.Chunk) == 0 {
		return cursor, false, nil
	}
	var chunk api.WorkspaceExecStreamChunkResponse
	if err := json.Unmarshal(event.Chunk, &chunk); err != nil {
		return cursor, false, err
	}
	if _, err := writer.Write(chunk.Data); err != nil {
		return cursor, false, err
	}
	if chunk.OffsetEnd > cursor {
		cursor = chunk.OffsetEnd
	}
	return cursor, false, nil
}

func runWorkspaceExecForeground(cmd *cobra.Command, control *client.Client, workspaceID string, exec api.WorkspaceExecResponse, scope client.WorkspaceScopeOptions) error {
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	stdinErr := make(chan error, 1)
	outputErr := make(chan error, 1)
	waitForStdin := !commandInputIsTerminal(cmd)
	go func() {
		stdinErr <- pumpWorkspaceExecStdin(ctx, cmd, control, workspaceID, exec.ID, exec.StdinCursor, scope)
	}()
	go func() {
		outputErr <- followWorkspaceExecOutput(ctx, cmd, control, workspaceID, exec.ID, exec.StdoutCursor, exec.StderrCursor, scope, true)
	}()
	for {
		select {
		case err := <-stdinErr:
			if err != nil {
				cancel()
				return err
			}
			stdinErr = nil
		case err := <-outputErr:
			if err == nil && waitForStdin && stdinErr != nil {
				if stdinPumpErr := <-stdinErr; stdinPumpErr != nil {
					cancel()
					return stdinPumpErr
				}
				stdinErr = nil
			}
			cancel()
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return ctx.Err()
		}
	}
}

func commandInputIsTerminal(cmd *cobra.Command) bool {
	file, ok := cmd.InOrStdin().(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func pumpWorkspaceExecStdin(ctx context.Context, cmd *cobra.Command, control *client.Client, workspaceID string, execID string, cursor int64, scope client.WorkspaceScopeOptions) error {
	buf := make([]byte, 4096)
	for {
		n, err := cmd.InOrStdin().Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			chunk, writeErr := control.WriteWorkspaceExecStdin(ctx, workspaceID, execID, api.WorkspaceExecStdinWriteRequest{Offset: cursor, Data: data}, scope)
			if writeErr != nil {
				return writeErr
			}
			cursor = chunk.OffsetEnd
		}
		if err != nil {
			if err == io.EOF {
				_, closeErr := control.CloseWorkspaceExecStdin(ctx, workspaceID, execID, scope)
				return closeErr
			}
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		default:
		}
	}
}

func connectWorkspacePty(cmd *cobra.Command, control *client.Client, workspaceID string, ptyID string, inputCursor int64, outputCursor int64, scope client.WorkspaceScopeOptions) error {
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	var rawState *term.State
	if file, ok := cmd.InOrStdin().(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		state, err := term.MakeRaw(int(file.Fd()))
		if err != nil {
			return err
		}
		rawState = state
		defer term.Restore(int(file.Fd()), rawState)
	}
	outputErr := make(chan error, 1)
	inputErr := make(chan error, 1)
	go func() {
		outputErr <- control.FollowWorkspacePtyOutput(ctx, workspaceID, ptyID, outputCursor, scope, func(event client.WorkspaceStreamEvent) error {
			if event.Error != nil {
				cancel()
				return workspaceStreamEventError(event.Error)
			}
			if event.Terminal != nil {
				cancel()
				return nil
			}
			if len(event.Chunk) == 0 {
				return nil
			}
			var chunk api.WorkspacePtyStreamChunkResponse
			if err := json.Unmarshal(event.Chunk, &chunk); err != nil {
				return err
			}
			_, err := cmd.OutOrStdout().Write(chunk.Data)
			return err
		})
	}()
	go func() {
		buf := make([]byte, 4096)
		cursor := inputCursor
		for {
			n, err := cmd.InOrStdin().Read(buf)
			if n > 0 {
				data := append([]byte(nil), buf[:n]...)
				_, writeErr := control.WriteWorkspacePtyInput(ctx, workspaceID, ptyID, api.WorkspacePtyInputWriteRequest{Offset: cursor, Data: data}, scope)
				if writeErr == nil {
					cursor += int64(len(data))
				}
				if writeErr != nil {
					inputErr <- writeErr
					return
				}
			}
			if err != nil {
				if err == io.EOF {
					inputErr <- nil
					return
				}
				inputErr <- err
				return
			}
		}
	}()
	select {
	case err := <-outputErr:
		cancel()
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	case err := <-inputErr:
		if err != nil {
			cancel()
			return err
		}
		if _, closeErr := control.CloseWorkspacePty(cmd.Context(), workspaceID, ptyID, scope); closeErr != nil {
			cancel()
			return closeErr
		}
		select {
		case err := <-outputErr:
			cancel()
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return ctx.Err()
		}
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil
		}
		return ctx.Err()
	}
}

func workspaceExecStateTerminal(state string) bool {
	switch strings.TrimSpace(state) {
	case "exited", "lost", "failed", "terminated":
		return true
	default:
		return false
	}
}

func workspaceStreamEventError(streamErr *api.WorkspaceStreamErrorResponse) error {
	if streamErr == nil {
		return nil
	}
	if strings.TrimSpace(streamErr.Message) != "" {
		return fmt.Errorf("%s: %s", streamErr.Code, streamErr.Message)
	}
	return fmt.Errorf("%s", streamErr.Code)
}

func terminalSize() (int32, int32) {
	if width, height, err := term.GetSize(int(os.Stdin.Fd())); err == nil && width > 0 && height > 0 {
		return int32(width), int32(height)
	}
	return 80, 24
}

type exitCodeError struct {
	code int
}

func (e exitCodeError) Error() string {
	return fmt.Sprintf("remote command exited with status %d", e.code)
}
