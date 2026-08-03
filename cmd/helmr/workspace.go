package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cli/format"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

func workspaceCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "workspace",
		Short: "Work with durable Workspaces.",
	}
	command.AddCommand(
		workspaceCreateCommand(),
		workspaceGetCommand(),
		workspaceDeleteCommand(),
		workspaceFilesCommand(),
		workspaceExecCommand(),
	)
	return command
}

func workspaceCreateCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var key string
	var secretEnv []string
	var secretFile []string
	var idempotencyKey string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "create DECLARED_ID",
		Short: "Create a Workspace from a deployed declaration.",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			secrets, err := workspaceSecrets(secretEnv, secretFile)
			if err != nil {
				return err
			}
			controlPlane, scope, err := scopedWorkspaceClient(command, projectID, environmentID)
			if err != nil {
				return err
			}
			var keyPointer *string
			if command.Flags().Changed("key") {
				keyPointer = &key
			}
			response, err := controlPlane.CreateWorkspace(command.Context(), args[0], api.CreateWorkspaceRequest{
				Key:            keyPointer,
				Secrets:        secrets,
				IdempotencyKey: idempotencyKey,
			}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(command.OutOrStdout(), response)
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), response.WorkspaceID)
			return err
		},
	}
	addScopeFlags(command, &projectID, &environmentID)
	command.Flags().StringVar(&key, "key", "", "Immutable Workspace key.")
	command.Flags().StringArrayVar(&secretEnv, "secret-env", nil, "Secret placement NAME=ENV. Repeatable.")
	command.Flags().StringArrayVar(&secretFile, "secret-file", nil, "Secret placement NAME=/absolute/path. Repeatable.")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for safe retries.")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON.")
	return command
}

func workspaceGetCommand() *cobra.Command {
	var address workspaceAddressFlags
	var projectID string
	var environmentID string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "get",
		Short: "Retrieve a Workspace.",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			controlPlane, scope, err := scopedWorkspaceClient(command, projectID, environmentID)
			if err != nil {
				return err
			}
			snapshot, err := address.retrieve(command, controlPlane, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(command.OutOrStdout(), snapshot)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\n", snapshot.ID, snapshot.DeclaredID, snapshot.Status)
			return err
		},
	}
	addScopeFlags(command, &projectID, &environmentID)
	address.add(command)
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON.")
	return command
}

func workspaceDeleteCommand() *cobra.Command {
	var address workspaceAddressFlags
	var projectID string
	var environmentID string
	var idempotencyKey string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "delete",
		Short: "Delete a Workspace.",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			controlPlane, scope, err := scopedWorkspaceClient(command, projectID, environmentID)
			if err != nil {
				return err
			}
			workspaceID, err := address.resolveID(command, controlPlane, scope)
			if err != nil {
				return err
			}
			receipt, err := controlPlane.DeleteWorkspace(command.Context(), workspaceID, api.DeleteWorkspaceRequest{
				IdempotencyKey: idempotencyKey,
			}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(command.OutOrStdout(), receipt)
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), receipt.WorkspaceID)
			return err
		},
	}
	addScopeFlags(command, &projectID, &environmentID)
	address.add(command)
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for safe retries.")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON.")
	return command
}

func workspaceFilesCommand() *cobra.Command {
	command := &cobra.Command{Use: "files", Short: "Read the current committed Workspace filesystem."}
	command.AddCommand(workspaceFilesReadCommand(), workspaceFilesListCommand(), workspaceFilesStatCommand())
	return command
}

func workspaceFilesReadCommand() *cobra.Command {
	var address workspaceAddressFlags
	var projectID string
	var environmentID string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "read PATH",
		Short: "Read a regular file.",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			controlPlane, scope, err := scopedWorkspaceClient(command, projectID, environmentID)
			if err != nil {
				return err
			}
			workspaceID, err := address.resolveID(command, controlPlane, scope)
			if err != nil {
				return err
			}
			content, err := controlPlane.ReadWorkspaceFile(command.Context(), workspaceID, args[0], scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(command.OutOrStdout(), content)
			}
			data, err := base64.StdEncoding.DecodeString(content.DataBase64)
			if err != nil {
				return fmt.Errorf("decode Workspace file: %w", err)
			}
			_, err = command.OutOrStdout().Write(data)
			return err
		},
	}
	addScopeFlags(command, &projectID, &environmentID)
	address.add(command)
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON without decoding file bytes.")
	return command
}

func workspaceFilesListCommand() *cobra.Command {
	var address workspaceAddressFlags
	var projectID string
	var environmentID string
	var cursor string
	var limit int32
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "list PATH",
		Short: "List a directory.",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			controlPlane, scope, err := scopedWorkspaceClient(command, projectID, environmentID)
			if err != nil {
				return err
			}
			workspaceID, err := address.resolveID(command, controlPlane, scope)
			if err != nil {
				return err
			}
			page, err := controlPlane.ListWorkspaceFiles(command.Context(), workspaceID, client.WorkspaceFileListOptions{
				Path: args[0], Cursor: cursor, Limit: limit,
			}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(command.OutOrStdout(), page)
			}
			for _, item := range page.Items {
				if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\n", item.Kind, item.Path); err != nil {
					return err
				}
			}
			if page.NextCursor != "" {
				_, err = fmt.Fprintf(command.OutOrStdout(), "next_cursor\t%s\n", page.NextCursor)
			}
			return err
		},
	}
	addScopeFlags(command, &projectID, &environmentID)
	address.add(command)
	command.Flags().StringVar(&cursor, "cursor", "", "Continue a prior directory page.")
	command.Flags().Int32Var(&limit, "limit", 0, "Maximum entries (default 50, maximum 100).")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON.")
	return command
}

func workspaceFilesStatCommand() *cobra.Command {
	var address workspaceAddressFlags
	var projectID string
	var environmentID string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "stat PATH",
		Short: "Stat a committed filesystem path.",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			controlPlane, scope, err := scopedWorkspaceClient(command, projectID, environmentID)
			if err != nil {
				return err
			}
			workspaceID, err := address.resolveID(command, controlPlane, scope)
			if err != nil {
				return err
			}
			entry, err := controlPlane.StatWorkspaceFile(command.Context(), workspaceID, args[0], scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(command.OutOrStdout(), entry)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "%s\t%s\n", entry.Kind, entry.Path)
			return err
		},
	}
	addScopeFlags(command, &projectID, &environmentID)
	address.add(command)
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON.")
	return command
}

func workspaceExecCommand() *cobra.Command {
	var address workspaceAddressFlags
	var projectID string
	var environmentID string
	var cwd string
	var envPairs []string
	var stdinPath string
	var timeout string
	var idempotencyKey string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "exec --idempotency-key KEY -- COMMAND [ARG...]",
		Short: "Execute one bounded command and wait for its terminal result.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			env, err := workspaceEnv(envPairs)
			if err != nil {
				return err
			}
			var stdinBase64 string
			if stdinPath != "" {
				data, err := os.ReadFile(stdinPath)
				if err != nil {
					return err
				}
				stdinBase64 = base64.StdEncoding.EncodeToString(data)
			}
			controlPlane, scope, err := scopedWorkspaceClient(command, projectID, environmentID)
			if err != nil {
				return err
			}
			workspaceID, err := address.resolveID(command, controlPlane, scope)
			if err != nil {
				return err
			}
			result, err := controlPlane.ExecuteWorkspace(command.Context(), workspaceID, api.ExecuteWorkspaceRequest{
				Command: args, Cwd: cwd, Env: env, StdinBase64: stdinBase64,
				Timeout: timeout, IdempotencyKey: idempotencyKey,
			}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(command.OutOrStdout(), result)
			}
			stdout, err := base64.StdEncoding.DecodeString(result.StdoutBase64)
			if err != nil {
				return fmt.Errorf("decode stdout: %w", err)
			}
			stderr, err := base64.StdEncoding.DecodeString(result.StderrBase64)
			if err != nil {
				return fmt.Errorf("decode stderr: %w", err)
			}
			if _, err := command.OutOrStdout().Write(stdout); err != nil {
				return err
			}
			if _, err := command.ErrOrStderr().Write(stderr); err != nil {
				return err
			}
			if result.ExitCode != 0 {
				return exitCodeError{code: int(result.ExitCode)}
			}
			return nil
		},
	}
	addScopeFlags(command, &projectID, &environmentID)
	address.add(command)
	command.Flags().StringVar(&cwd, "cwd", "", "Working directory (defaults to /workspace).")
	command.Flags().StringArrayVar(&envPairs, "set-env", nil, "Environment entry NAME=VALUE. Repeatable.")
	command.Flags().StringVar(&stdinPath, "stdin", "", "Read stdin bytes from a file.")
	command.Flags().StringVar(&timeout, "timeout", "", "Execution timeout (default 5m, maximum 15m).")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Required idempotency key.")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON without decoding output bytes.")
	_ = command.MarkFlagRequired("idempotency-key")
	return command
}

type workspaceAddressFlags struct {
	id         string
	key        string
	declaredID string
}

func (flags *workspaceAddressFlags) add(command *cobra.Command) {
	command.Flags().StringVar(&flags.id, "id", "", "Workspace ID.")
	command.Flags().StringVar(&flags.key, "key", "", "Workspace key.")
	command.Flags().StringVar(&flags.declaredID, "declared-id", "", "Workspace declared ID (required with --key).")
}

func (flags workspaceAddressFlags) retrieve(
	command *cobra.Command,
	controlPlane *client.Client,
	scope client.WorkspaceScopeOptions,
) (api.WorkspaceSnapshot, error) {
	if err := flags.validate(); err != nil {
		return api.WorkspaceSnapshot{}, err
	}
	if flags.id != "" {
		return controlPlane.GetWorkspace(command.Context(), flags.id, scope)
	}
	return controlPlane.GetWorkspaceByKey(command.Context(), flags.declaredID, flags.key, scope)
}

func (flags workspaceAddressFlags) resolveID(
	command *cobra.Command,
	controlPlane *client.Client,
	scope client.WorkspaceScopeOptions,
) (string, error) {
	snapshot, err := flags.retrieve(command, controlPlane, scope)
	if err != nil {
		return "", err
	}
	return snapshot.ID, nil
}

func (flags workspaceAddressFlags) validate() error {
	if (flags.id == "") == (flags.key == "") {
		return errors.New("exactly one of --id or --key is required")
	}
	if flags.key != "" && flags.declaredID == "" {
		return errors.New("--declared-id is required with --key")
	}
	if flags.id != "" && flags.declaredID != "" {
		return errors.New("--declared-id is only accepted with --key")
	}
	return nil
}

func scopedWorkspaceClient(
	command *cobra.Command,
	projectID string,
	environmentID string,
) (*client.Client, client.WorkspaceScopeOptions, error) {
	controlPlane, err := controlPlaneClient(command)
	if err != nil {
		return nil, client.WorkspaceScopeOptions{}, err
	}
	scope, err := workspaceScopeForClient(controlPlane, projectID, environmentID)
	if err != nil {
		return nil, client.WorkspaceScopeOptions{}, err
	}
	return controlPlane, scope, nil
}

func workspaceSecrets(envValues []string, fileValues []string) ([]api.WorkspaceSecret, error) {
	secrets := make([]api.WorkspaceSecret, 0, len(envValues)+len(fileValues))
	for _, raw := range envValues {
		name, target, err := workspacePair(raw, "--secret-env")
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, api.WorkspaceSecret{Name: name, Env: target})
	}
	for _, raw := range fileValues {
		name, target, err := workspacePair(raw, "--secret-file")
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, api.WorkspaceSecret{Name: name, File: target})
	}
	return secrets, nil
}

func workspaceEnv(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(values))
	for _, raw := range values {
		name, value, err := workspacePair(raw, "--set-env")
		if err != nil {
			return nil, err
		}
		if _, exists := env[name]; exists {
			return nil, fmt.Errorf("duplicate --set-env name %q", name)
		}
		env[name] = value
	}
	return env, nil
}

func workspacePair(raw string, flag string) (string, string, error) {
	name, value, ok := strings.Cut(raw, "=")
	if !ok || name == "" || value == "" {
		return "", "", fmt.Errorf("%s must use NAME=VALUE", flag)
	}
	return name, value, nil
}

type exitCodeError struct {
	code int
}

func (e exitCodeError) Error() string {
	return fmt.Sprintf("Workspace command exited with status %d", e.code)
}
