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
		Use:   "create DECLARED_ID",
		Short: "Create a Computer from a deployed declaration.",
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
			var bindings []secretbinding.Binding
			if secretsFile != "" {
				file, e := os.Open(secretsFile)
				if e != nil {
					return e
				}
				defer file.Close()
				decoder := json.NewDecoder(io.LimitReader(file, 65537))
				decoder.DisallowUnknownFields()
				if e := decoder.Decode(&bindings); e != nil {
					return fmt.Errorf("secrets-file must contain a JSON Computer binding array (API fields use allowed_origins): %w", e)
				}
				var trailing any
				if e := decoder.Decode(&trailing); e != io.EOF {
					return errors.New("secrets-file contains trailing JSON")
				}
				if bindings == nil {
					return errors.New("secrets-file must contain an array")
				}
				for _, binding := range bindings {
					if e := secretbinding.ValidateBinding(binding); e != nil {
						return e
					}
				}
			}
			response, err := controlPlane.CreateComputer(command.Context(), args[0], api.CreateComputerRequest{
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
	command.Flags().StringVar(&secretsFile, "secrets-file", "", "API JSON array of Computer Secret bindings (allowed_origins; names and placements, never values).")
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
			_, err = fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\n", snapshot.ID, snapshot.SandboxID, snapshot.Status)
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
	response, err := controlPlane.ListComputers(command.Context(), &flags.key, scope)
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
