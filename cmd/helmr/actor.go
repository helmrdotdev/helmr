package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

type actorAddressFlags struct {
	id  string
	key string
}

func (a *actorAddressFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&a.id, "id", "", "Actor ID.")
	cmd.Flags().StringVar(&a.key, "key", "", "Actor key.")
}

func (a actorAddressFlags) reference() (api.ActorReference, error) {
	reference := api.ActorReference{
		ActorID:  a.id,
		ActorKey: a.key,
	}
	if (reference.ActorID == "") == (reference.ActorKey == "") {
		return api.ActorReference{}, errors.New("exactly one of --id or --key is required")
	}
	if err := api.ValidateActorReference(reference); err != nil {
		return api.ActorReference{}, err
	}
	return reference, nil
}

func actorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "actor",
		Short: "Work with Actors.",
	}
	cmd.AddCommand(
		actorStartCommand(),
		actorGetCommand(),
		actorInputCommand(),
		actorOutputCommand(),
		actorCloseCommand(),
	)
	return cmd
}

func actorStartCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var key string
	var inputFile string
	var inputJSON string
	var workspaceID string
	var idempotencyKey string
	var queue string
	var concurrencyKey string
	var priority int32
	var ttl string
	var retryFile string
	var retryJSON string
	var metadataFile string
	var metadataJSON string
	var tags []string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "start ACTOR",
		Short: "Start an Actor from a deployed declaration.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			input, err := parseOptionalJSON(inputFile, inputJSON, "--input")
			if err != nil {
				return err
			}
			metadata, err := parseOptionalJSON(metadataFile, metadataJSON, "--metadata")
			if err != nil {
				return err
			}
			retryValue, err := parseOptionalJSON(retryFile, retryJSON, "--retry")
			if err != nil {
				return err
			}
			var retry *api.StartActorRetryPolicy
			if len(retryValue) > 0 {
				retry = new(api.StartActorRetryPolicy)
				if err := json.Unmarshal(retryValue, retry); err != nil {
					return fmt.Errorf("parse --retry: %w", err)
				}
			}
			if workspaceID == "" {
				return errors.New("--workspace is required")
			}
			if err := api.ValidateWorkspaceID(workspaceID); err != nil {
				return err
			}
			var actorKey *string
			if cmd.Flags().Changed("key") {
				actorKey = &key
			}
			var run *api.StartActorRunOptions
			if queue != "" ||
				concurrencyKey != "" ||
				priority != 0 ||
				ttl != "" ||
				retry != nil ||
				len(metadata) > 0 ||
				len(tags) > 0 {
				run = &api.StartActorRunOptions{
					Queue: strings.TrimSpace(queue), Priority: priority,
					TTL: strings.TrimSpace(ttl), Retry: retry,
					Metadata: metadata, Tags: cleanTags(tags),
				}
				if concurrencyKey = strings.TrimSpace(concurrencyKey); concurrencyKey != "" {
					run.ConcurrencyKey = &concurrencyKey
				}
			}
			controlPlane, scope, err := scopedActorClient(cmd, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := controlPlane.StartActor(cmd.Context(), args[0], api.StartActorRequest{
				Key: actorKey, Input: input,
				IdempotencyKey: strings.TrimSpace(idempotencyKey),
				Workspace:      api.WorkspaceTarget{ID: &workspaceID},
				Run:            run,
			}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), response)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "actor_id: %s\n", response.ActorID)
			fmt.Fprintf(cmd.OutOrStdout(), "run_id: %s\n", response.RunID)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&key, "key", "", "Stable identity key for the new Actor.")
	cmd.Flags().StringVar(&inputFile, "input-file", "", "Read initial input JSON from a file.")
	cmd.Flags().StringVar(&inputJSON, "input-json", "", "Inline initial input JSON literal.")
	cmd.Flags().StringVar(&workspaceID, "workspace", "", "Existing Workspace ID (required).")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for this Actor start.")
	cmd.Flags().StringVar(&queue, "queue", "", "Queue name for managed Runs.")
	cmd.Flags().StringVar(&concurrencyKey, "concurrency-key", "", "Concurrency key for managed Runs.")
	cmd.Flags().Int32Var(&priority, "priority", 0, "Managed Run priority offset in seconds.")
	cmd.Flags().StringVar(&ttl, "ttl", "", "Queued managed Run time-to-live.")
	cmd.Flags().StringVar(&retryFile, "retry-file", "", "Read managed Run retry policy JSON from a file.")
	cmd.Flags().StringVar(&retryJSON, "retry-json", "", "Inline managed Run retry policy JSON literal.")
	cmd.Flags().StringVar(&metadataFile, "metadata-file", "", "Read managed Run metadata JSON from a file.")
	cmd.Flags().StringVar(&metadataJSON, "metadata-json", "", "Inline managed Run metadata JSON literal.")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "Add a managed Run tag. Repeat for multiple tags.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	cmd.MarkFlagsMutuallyExclusive("input-file", "input-json")
	cmd.MarkFlagsMutuallyExclusive("metadata-file", "metadata-json")
	cmd.MarkFlagsMutuallyExclusive("retry-file", "retry-json")
	return cmd
}

func actorGetCommand() *cobra.Command {
	var address actorAddressFlags
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "get ACTOR",
		Short: "Show Actor status.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reference, err := address.reference()
			if err != nil {
				return err
			}
			controlPlane, scope, err := scopedActorClient(cmd, projectID, environmentID)
			if err != nil {
				return err
			}
			status, err := controlPlane.GetActorStatus(cmd.Context(), args[0], reference, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), status)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "actor_id: %s\n", status.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "actor_status: %s\n", status.Status)
			if status.CurrentRunID != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "run_id: %s\n", *status.CurrentRunID)
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	address.add(cmd)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func actorInputCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "input", Short: "Send durable Actor input."}
	cmd.AddCommand(actorInputSendCommand())
	return cmd
}

func actorInputSendCommand() *cobra.Command {
	var address actorAddressFlags
	var projectID string
	var environmentID string
	var inputFile string
	var inputJSON string
	var idempotencyKey string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "send ACTOR",
		Short: "Append an Actor input record.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reference, err := address.reference()
			if err != nil {
				return err
			}
			input, err := parseOptionalJSON(inputFile, inputJSON, "--input")
			if err != nil {
				return err
			}
			if len(input) == 0 {
				return errors.New("--input-file or --input-json is required")
			}
			controlPlane, scope, err := scopedActorClient(cmd, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := controlPlane.SendActorInput(cmd.Context(), args[0], api.SendActorInputRequest{
				ActorID: reference.ActorID, ActorKey: reference.ActorKey,
				Input: input, IdempotencyKey: strings.TrimSpace(idempotencyKey),
			}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), response)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "sequence: %d\n", response.Sequence)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	address.add(cmd)
	cmd.Flags().StringVar(&inputFile, "input-file", "", "Read input JSON from a file.")
	cmd.Flags().StringVar(&inputJSON, "input-json", "", "Inline input JSON literal.")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for this input.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	cmd.MarkFlagsMutuallyExclusive("input-file", "input-json")
	return cmd
}

func actorOutputCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "output", Short: "Read durable Actor output."}
	cmd.AddCommand(actorOutputReadCommand())
	return cmd
}

func actorOutputReadCommand() *cobra.Command {
	var address actorAddressFlags
	var projectID string
	var environmentID string
	var after int64
	var limit int32
	var jsonOutput bool
	var jsonLines bool
	cmd := &cobra.Command{
		Use:   "read ACTOR",
		Short: "Read one finite Actor output page.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reference, err := address.reference()
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("limit") && limit < 1 {
				return errors.New("--limit must be in [1,100]")
			}
			var afterPointer *int64
			if cmd.Flags().Changed("after") {
				afterPointer = &after
			}
			controlPlane, scope, err := scopedActorClient(cmd, projectID, environmentID)
			if err != nil {
				return err
			}
			page, err := controlPlane.ReadActorOutput(cmd.Context(), args[0], reference, client.ActorOutputReadOptions{
				After: afterPointer, Limit: limit, EnvironmentScopeOptions: scope,
			})
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), page)
			}
			if jsonLines {
				return writeJSONLines(cmd.OutOrStdout(), page.Records)
			}
			for _, record := range page.Records {
				fmt.Fprintf(cmd.OutOrStdout(), "%d\t%s\n", record.Sequence, record.Data)
			}
			if page.HasMore {
				fmt.Fprintf(cmd.OutOrStdout(), "next_after: %d\n", page.NextAfter)
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	address.add(cmd)
	cmd.Flags().Int64Var(&after, "after", 0, "Return records after this durable sequence.")
	cmd.Flags().Int32Var(&limit, "limit", 0, "Maximum records (default 50, maximum 100).")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	cmd.Flags().BoolVar(&jsonLines, "jsonl", false, "Emit one JSON record per line.")
	cmd.MarkFlagsMutuallyExclusive("json", "jsonl")
	return cmd
}

func actorCloseCommand() *cobra.Command {
	var address actorAddressFlags
	var projectID string
	var environmentID string
	var idempotencyKey string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "close ACTOR",
		Short: "Close an Actor.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reference, err := address.reference()
			if err != nil {
				return err
			}
			controlPlane, scope, err := scopedActorClient(cmd, projectID, environmentID)
			if err != nil {
				return err
			}
			receipt, err := controlPlane.CloseActor(cmd.Context(), args[0], api.ActorOperationRequest{
				ActorID: reference.ActorID, ActorKey: reference.ActorKey,
				IdempotencyKey: strings.TrimSpace(idempotencyKey),
			}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), receipt)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "actor_id: %s\n", receipt.ActorID)
			fmt.Fprintf(cmd.OutOrStdout(), "accepted_at: %s\n", receipt.AcceptedAt.UTC().Format("2006-01-02T15:04:05.999999999Z"))
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	address.add(cmd)
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for this close.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func scopedActorClient(
	cmd *cobra.Command,
	projectID string,
	environmentID string,
) (*client.Client, client.EnvironmentScopeOptions, error) {
	controlPlane, err := controlPlaneClient(cmd)
	if err != nil {
		return nil, client.EnvironmentScopeOptions{}, err
	}
	scope, err := environmentScopeForClient(controlPlane, projectID, environmentID)
	return controlPlane, scope, err
}
