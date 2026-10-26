package main

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/spf13/cobra"
)

func auditCommand() *cobra.Command { return auditNamespace(false) }
func installationCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "installation", Short: "Manage the installation using an administrator login"}
	cmd.AddCommand(auditNamespace(true))
	return cmd
}
func auditNamespace(installation bool) *cobra.Command {
	cmd := &cobra.Command{Use: "audit", Short: "Inspect retained security metadata"}
	list := &cobra.Command{Use: "list", Short: "Search one bounded page of audit events", Args: cobra.NoArgs}
	var from, to, beforeTime, beforeID string
	var options client.AuditSearchOptions
	var jsonOutput bool
	flags := list.Flags()
	flags.StringVar(&from, "from", "", "Inclusive recorded time (RFC3339; required)")
	flags.StringVar(&to, "to", "", "Exclusive recorded time (RFC3339; required; range at most 31 days)")
	flags.StringVar(&beforeTime, "before-recorded-at", "", "Recorded time from next_before; repeat the original filters")
	flags.StringVar(&beforeID, "before-id", "", "Event ID from next_before")
	flags.IntVar(&options.Limit, "limit", 100, "Page size (1–500)")
	flags.StringVar(&options.EnvironmentID, "environment-id", "", "Narrow to an authorized Environment UUID")
	flags.StringVar(&options.ActorID, "actor-id", "", "Filter by actor UUID")
	flags.StringVar(&options.ActorKind, "actor-kind", "", "Filter by actor kind")
	flags.StringVar(&options.Action, "action", "", "Filter by action")
	flags.StringVar(&options.TargetKind, "target-kind", "", "Filter by target kind")
	flags.StringVar(&options.TargetID, "target-id", "", "Filter by target UUID")
	flags.StringVar(&options.TargetUserID, "target-user-id", "", "Filter by target user UUID")
	flags.StringVar(&options.SlackTeamID, "slack-team-id", "", "Filter by linked Slack workspace")
	flags.StringVar(&options.SlackUserID, "slack-user-id", "", "Filter by linked Slack user")
	flags.StringVar(&options.Outcome, "outcome", "", "Filter by outcome")
	flags.BoolVar(&jsonOutput, "json", false, "Print the response and next_before as JSON")
	if installation {
		flags.StringVar(&options.OrganizationID, "organization-id", "", "Original organization UUID; omit for installation events")
	}
	list.RunE = func(cmd *cobra.Command, _ []string) error {
		var err error
		options.From, err = time.Parse(time.RFC3339Nano, from)
		if err != nil {
			return fmt.Errorf("--from requires RFC3339 time")
		}
		options.To, err = time.Parse(time.RFC3339Nano, to)
		if err != nil {
			return fmt.Errorf("--to requires RFC3339 time")
		}
		if !options.To.After(options.From) || options.To.Sub(options.From) > 31*24*time.Hour || options.Limit < 1 || options.Limit > 500 {
			return fmt.Errorf("use an increasing range of at most 31 days and a limit of 1–500")
		}
		if beforeTime != "" || beforeID != "" {
			key := api.AuditBefore{}
			key.RecordedAt, err = time.Parse(time.RFC3339Nano, beforeTime)
			if err != nil {
				return fmt.Errorf("--before-recorded-at and --before-id must be supplied together")
			}
			key.ID, err = ids.Parse(beforeID)
			if err != nil {
				return err
			}
			options.Before = &key
		}
		var c *client.Client
		if installation {
			c, err = sessionControlPlaneClient(cmd)
		} else {
			c, err = controlPlaneClient(cmd)
		}
		if err != nil {
			return err
		}
		var result api.AuditEventsResponse
		if installation {
			result, err = c.ListInstallationAuditEvents(cmd.Context(), options)
		} else {
			result, err = c.ListAuditEvents(cmd.Context(), options)
		}
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "RECORDED AT\tACTION\tOUTCOME\tACTOR\tTARGET")
		for _, event := range result.Events {
			actor := "Unidentified"
			if event.ActorIdentity != nil {
				actor = event.ActorIdentity.DisplayName
			} else if event.ActorComponent != nil {
				actor = *event.ActorComponent
			} else {
				for _, id := range []*uuid.UUID{event.ActorUserID, event.ActorAPIKeyID, event.ActorSessionID, event.ActorWorkerHostID} {
					if id != nil {
						actor = id.String()
						break
					}
				}
			}

			target := event.TargetKind
			if event.TargetID != nil {
				target += "/" + event.TargetID.String()
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", event.RecordedAt.Format(time.RFC3339Nano), event.Action, event.Outcome, actor, target)
		}
		if err = w.Flush(); err != nil {
			return err
		}
		if result.NextBefore != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "Next page: --before-recorded-at %s --before-id %s\n", result.NextBefore.RecordedAt.Format(time.RFC3339Nano), result.NextBefore.ID)
		}
		return nil
	}
	cmd.AddCommand(list)
	return cmd
}
