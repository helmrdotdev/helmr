package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/lifetime"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/spf13/cobra"
)

func computerCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "computer",
		Short: "Work with durable Computers.",
	}
	command.AddCommand(
		computerCreateCommand(),
		computerGetCommand(),
		computerListCommand(),
		computerUsageCommand("sessions"),
		computerUsageCommand("commands"),
		computerUpdateCommand(),
		computerDeleteCommand(),
		computerCommandCommand(),
	)
	return command
}

func computerCreateCommand() *cobra.Command {
	var secretsFile string
	var projectID string
	var environmentID string
	var key string
	var idempotencyKey string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "create TEMPLATE",
		Short: "Create a Computer from a deployed Template.",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			controlPlane, scope, err := scopedComputerClient(command, projectID, environmentID)
			if err != nil {
				return err
			}
			var keyPointer *string
			if command.Flags().Changed("key") {
				keyPointer = &key
			}
			bindings := []secretbinding.Reference{}
			if secretsFile != "" {
				file, e := os.Open(secretsFile)
				if e != nil {
					return e
				}
				defer file.Close()
				decoder := json.NewDecoder(io.LimitReader(file, 65537))
				decoder.DisallowUnknownFields()
				if e := decoder.Decode(&bindings); e != nil {
					return fmt.Errorf("secrets-file must contain a JSON Secret reference array (secretId and allowedOrigins): %w", e)
				}
				var trailing any
				if e := decoder.Decode(&trailing); e != io.EOF {
					return errors.New("secrets-file contains trailing JSON")
				}
				if bindings == nil {
					return errors.New("secrets-file must contain an array")
				}
				if _, e := secretbinding.NormalizeReferences(bindings); e != nil {
					return e
				}
			}
			response, err := controlPlane.CreateComputer(command.Context(), api.CreateComputerRequest{
				Template:       args[0],
				Key:            keyPointer,
				Secrets:        bindings,
				IdempotencyKey: idempotencyKey,
			}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command.OutOrStdout(), response)
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), response.ID)
			return err
		},
	}
	addScopeFlags(command, &projectID, &environmentID)
	command.Flags().StringVar(&secretsFile, "secrets-file", "", "JSON array of Secret references (secretId, env or file placement, and allowedOrigins; never values).")
	command.Flags().StringVar(&key, "key", "", "Immutable Computer key.")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for safe retries.")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON.")
	return command
}

func computerGetCommand() *cobra.Command {
	var address computerAddressFlags
	var projectID string
	var environmentID string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "get",
		Short: "Retrieve a Computer.",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			controlPlane, scope, err := scopedComputerClient(command, projectID, environmentID)
			if err != nil {
				return err
			}
			snapshot, err := address.retrieve(command, controlPlane, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command.OutOrStdout(), snapshot)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\n", snapshot.ID, snapshot.DefinitionID, snapshot.Status)
			return err
		},
	}
	addScopeFlags(command, &projectID, &environmentID)
	address.add(command)
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON.")
	return command
}

func computerDeleteCommand() *cobra.Command {
	var address computerAddressFlags
	var projectID string
	var environmentID string
	var idempotencyKey string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "delete",
		Short: "Delete a Computer.",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			controlPlane, scope, err := scopedComputerClient(command, projectID, environmentID)
			if err != nil {
				return err
			}
			computerID, err := address.resolveID(command, controlPlane, scope)
			if err != nil {
				return err
			}
			receipt, err := controlPlane.DeleteComputer(command.Context(), computerID, api.DeleteComputerRequest{
				IdempotencyKey: idempotencyKey,
			}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command.OutOrStdout(), receipt)
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), receipt.ComputerID)
			return err
		},
	}
	addScopeFlags(command, &projectID, &environmentID)
	address.add(command)
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for safe retries.")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON.")
	return command
}

func computerCommandCommand() *cobra.Command {
	var address computerAddressFlags
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
		Short: "Admit one command and return its execution ID.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			env, err := computerEnv(envPairs)
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
			controlPlane, scope, err := scopedComputerClient(command, projectID, environmentID)
			if err != nil {
				return err
			}
			computerID, err := address.resolveID(command, controlPlane, scope)
			if err != nil {
				return err
			}
			result, err := controlPlane.ExecuteComputer(command.Context(), computerID, api.ExecuteComputerRequest{
				Command: args, Cwd: cwd, Env: env, StdinBase64: stdinBase64,
				Timeout: timeout, IdempotencyKey: idempotencyKey,
			}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command.OutOrStdout(), result)
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), result.CommandID)
			return err
		},
	}
	addScopeFlags(command, &projectID, &environmentID)
	address.add(command)
	command.Flags().StringVar(&cwd, "cwd", "", "Working directory (defaults to /workspace).")
	command.Flags().StringArrayVar(&envPairs, "set-env", nil, "Environment entry NAME=VALUE. Repeatable.")
	command.Flags().StringVar(&stdinPath, "stdin", "", "Read stdin bytes from a file.")
	command.Flags().StringVar(&timeout, "timeout", "", "Execution timeout (default 5m, maximum 15m).")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Required idempotency key.")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit the execution receipt as JSON.")
	_ = command.MarkFlagRequired("idempotency-key")
	return command
}

type computerAddressFlags struct {
	id  string
	key string
}

func (flags *computerAddressFlags) add(command *cobra.Command) {
	command.Flags().StringVar(&flags.id, "id", "", "Computer UUID.")
	command.Flags().StringVar(&flags.key, "key", "", "Computer key.")
}

func (flags computerAddressFlags) retrieve(
	command *cobra.Command,
	controlPlane *client.Client,
	scope client.ComputerScopeOptions,
) (api.ComputerSnapshot, error) {
	if err := flags.validate(); err != nil {
		return api.ComputerSnapshot{}, err
	}
	if flags.id != "" {
		return controlPlane.GetComputer(command.Context(), flags.id, scope)
	}
	response, err := controlPlane.ListComputers(command.Context(), client.ComputerListOptions{Key: &flags.key}, scope)
	if err != nil {
		return api.ComputerSnapshot{}, err
	}
	if len(response.Computers) == 0 {
		return api.ComputerSnapshot{}, errors.New("computer not found")
	}
	return controlPlane.GetComputer(command.Context(), response.Computers[0].ID, scope)
}

func (flags computerAddressFlags) resolveID(
	command *cobra.Command,
	controlPlane *client.Client,
	scope client.ComputerScopeOptions,
) (string, error) {
	snapshot, err := flags.retrieve(command, controlPlane, scope)
	if err != nil {
		return "", err
	}
	return snapshot.ID, nil
}

func (flags computerAddressFlags) validate() error {
	if (flags.id == "") == (flags.key == "") {
		return errors.New("exactly one of --id or --key is required")
	}
	return nil
}

func scopedComputerClient(
	command *cobra.Command,
	projectID string,
	environmentID string,
) (*client.Client, client.ComputerScopeOptions, error) {
	controlPlane, err := controlPlaneClient(command)
	if err != nil {
		return nil, client.ComputerScopeOptions{}, err
	}
	scope, err := computerScopeForClient(command.Context(), controlPlane, projectID, environmentID)
	if err != nil {
		return nil, client.ComputerScopeOptions{}, err
	}
	return controlPlane, scope, nil
}

func computerEnv(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(values))
	for _, raw := range values {
		name, value, err := computerPair(raw, "--set-env")
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

func computerPair(raw string, flag string) (string, string, error) {
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
	return fmt.Sprintf("computer command exited with status %d", e.code)
}

func computerListCommand() *cobra.Command {
	var project, environment, key, cursor string
	var limit int32
	var jsonOutput bool
	cmd := &cobra.Command{Use: "list", Short: "List Computers.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("limit") && limit < 1 {
			return errors.New("--limit must be in [1,100]")
		}
		cp, scope, err := scopedComputerClient(cmd, project, environment)
		if err != nil {
			return err
		}
		query := client.ComputerListOptions{Cursor: cursor, Limit: limit}
		if cmd.Flags().Changed("key") {
			query.Key = &key
		}
		page, err := cp.ListComputers(cmd.Context(), query, scope)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), page)
		}
		for _, item := range page.Computers {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", item.ID, item.DefinitionID, item.Status)
		}
		if page.NextCursor != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "next_cursor: %s\n", page.NextCursor)
		}
		return nil
	}}
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().StringVar(&key, "key", "", "Find the Computer with this exact key.")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continuation cursor.")
	cmd.Flags().Int32Var(&limit, "limit", 0, "Maximum items (default 50, maximum 100).")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON page.")
	return cmd
}

func computerUsageCommand(kind string) *cobra.Command {
	var project, environment, cursor string
	var address computerAddressFlags
	var limit int32
	var jsonOutput bool
	cmd := &cobra.Command{Use: kind, Short: "List " + kind + " occupying this Computer, including pending cleanup.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("limit") && limit < 1 {
			return errors.New("--limit must be in [1,100]")
		}
		cp, scope, err := scopedComputerClient(cmd, project, environment)
		if err != nil {
			return err
		}
		id, err := address.resolveID(cmd, cp, scope)
		if err != nil {
			return err
		}
		query := client.ComputerUsageOptions{Cursor: cursor, Limit: limit}
		var items []api.ComputerUsage
		var next string
		if kind == "sessions" {
			page, err := cp.ListComputerSessions(cmd.Context(), id, query, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), page)
			}
			items, next = page.Sessions, page.NextCursor
		} else {
			page, err := cp.ListComputerCommands(cmd.Context(), id, query, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), page)
			}
			items, next = page.Commands, page.NextCursor
		}
		for _, item := range items {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", item.ID, item.UsageState)
		}
		if next != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "next_cursor: %s\n", next)
		}
		return nil
	}}
	addScopeFlags(cmd, &project, &environment)
	address.add(cmd)
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continuation cursor for this collection.")
	cmd.Flags().Int32Var(&limit, "limit", 0, "Maximum items (default 50, maximum 100).")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON page.")
	return cmd
}

func computerUpdateCommand() *cobra.Command {
	var project, environment, retention string
	var address computerAddressFlags
	var jsonOutput bool
	cmd := &cobra.Command{Use: "update", Short: "Update Computer retention.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		policy := lifetime.Policy{Set: true}
		if retention != "never" {
			duration, err := parseDuration(retention)
			if err != nil || duration.Milliseconds() <= 0 {
				return errors.New("--delete-after-unused must be a positive duration or never")
			}
			ms := duration.Milliseconds()
			policy.Milliseconds = &ms
		}
		cp, scope, err := scopedComputerClient(cmd, project, environment)
		if err != nil {
			return err
		}
		id, err := address.resolveID(cmd, cp, scope)
		if err != nil {
			return err
		}
		updated, err := cp.UpdateComputer(cmd.Context(), id, api.UpdateComputerRequest{DeleteAfterUnused: policy}, scope)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), updated)
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), updated.ID)
		return err
	}}
	addScopeFlags(cmd, &project, &environment)
	address.add(cmd)
	cmd.Flags().StringVar(&retention, "delete-after-unused", "", "Retention after unused, such as 1h or never.")
	_ = cmd.MarkFlagRequired("delete-after-unused")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON.")
	return cmd
}
