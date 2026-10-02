package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/spf13/cobra"
)

func actorCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "actor", Short: "Work with deployed Actors."}
	cmd.AddCommand(actorStartCommand())
	return cmd
}

func actorStartCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var key string
	var computerID string
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
		Short: "Start a Session from a deployed Actor.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			if computerID == "" {
				return errors.New("--computer is required")
			}
			if err := api.ValidateComputerID(computerID); err != nil {
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
				Key:            actorKey,
				IdempotencyKey: strings.TrimSpace(idempotencyKey),
				Computer:       api.ComputerIDTarget{ID: computerID},
				Run:            run,
			}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), response)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "session_id: %s\n", response.SessionID)
			fmt.Fprintf(cmd.OutOrStdout(), "run_id: %s\n", response.RunID)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&key, "key", "", "Stable identity key for the new Session.")
	cmd.Flags().StringVar(&computerID, "computer", "", "Existing Computer ID (required).")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for this Session start.")
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
	cmd.MarkFlagsMutuallyExclusive("metadata-file", "metadata-json")
	cmd.MarkFlagsMutuallyExclusive("retry-file", "retry-json")
	return cmd
}
