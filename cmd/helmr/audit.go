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
func platformCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "platform", Short: "Manage the platform using an administrator login"}
	cmd.AddCommand(auditNamespace(true))
	return cmd
}
func auditNamespace(adminView bool) *cobra.Command {
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
	if adminView {
		flags.StringVar(&options.OrganizationID, "organization-id", "", "Original organization UUID; omit to list only platform events that belong to no organization")
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
		if adminView {
			c, err = sessionControlPlaneClient(cmd)
		} else {
			c, err = controlPlaneClient(cmd)
		}
		if err != nil {
			return err
		}
		var rows []auditTableRow
		var nextBefore *api.AuditBefore
		if adminView {
			result, err := c.ListAdminAuditEvents(cmd.Context(), options)
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			nextBefore = result.NextBefore
			for _, event := range result.Events {
				fallback := "Unidentified"
				if event.ActorComponent != nil {
					fallback = *event.ActorComponent
				}
				actor := auditActorLabel(event.ActorIdentity, fallback, event.ActorUserID, event.ActorAPIKeyID, event.ActorSessionID, event.ActorWorkerHostID)
				rows = append(rows, auditTableRow{event.RecordedAt, event.Action, event.Outcome, actor, event.TargetKind, event.TargetID})
			}
		} else {
			result, err := c.ListAuditEvents(cmd.Context(), options)
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			nextBefore = result.NextBefore
			for _, event := range result.Events {
				fallback := "Unidentified"
				if event.ActorKind == api.AuditActorPlatformService {
					fallback = "Platform service"
				}
				actor := auditActorLabel(event.ActorIdentity, fallback, event.ActorUserID, event.ActorAPIKeyID, event.ActorSessionID)
				rows = append(rows, auditTableRow{event.RecordedAt, event.Action, event.Outcome, actor, event.TargetKind, event.TargetID})
			}
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "RECORDED AT\tACTION\tOUTCOME\tACTOR\tTARGET")
		for _, row := range rows {
			target := row.targetKind
			if row.targetID != nil {
				target += "/" + row.targetID.String()
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", row.recordedAt.Format(time.RFC3339Nano), row.action, row.outcome, row.actor, target)
		}
		if err = w.Flush(); err != nil {
			return err
		}
		if nextBefore != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "Next page: --before-recorded-at %s --before-id %s\n", nextBefore.RecordedAt.Format(time.RFC3339Nano), nextBefore.ID)
		}
		return nil
	}
	cmd.AddCommand(list)
	return cmd
}

type auditTableRow struct {
	recordedAt                         time.Time
	action, outcome, actor, targetKind string
	targetID                           *uuid.UUID
}

func auditActorLabel(identity *api.AuditIdentity, fallback string, ids ...*uuid.UUID) string {
	if identity != nil && identity.DisplayName != "" {
		return identity.DisplayName
	}
	for _, id := range ids {
		if id != nil {
			return id.String()
		}
	}
	return fallback
}
