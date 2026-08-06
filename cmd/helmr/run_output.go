package main

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

func writeRunTable(w io.Writer, runs []api.RunListItem) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN ID\tENTRYPOINT\tSTATUS\tATTEMPT")
	for _, run := range runs {
		entrypoint := run.Entrypoint.Kind + ":" + run.Entrypoint.ID
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", shortID(run.ID), entrypoint, run.Status, run.CurrentAttemptNumber)
	}
	_ = tw.Flush()
}

func writeRunDetails(w io.Writer, run api.RunSnapshotResponse) {
	fmt.Fprintf(w, "ID:          %s\n", run.ID)
	fmt.Fprintf(w, "Entrypoint:  %s %s\n", run.Entrypoint.Kind, run.Entrypoint.ID)
	fmt.Fprintf(w, "Deployment:  %s (%s)\n", run.Deployment.ID, run.Deployment.Version)
	fmt.Fprintf(w, "Workspace:   %s\n", run.WorkspaceID)
	fmt.Fprintf(w, "Status:      %s\n", run.Status)
	fmt.Fprintf(w, "Attempt:     %d\n", run.CurrentAttemptNumber)
	fmt.Fprintf(w, "Cause:       %s\n", run.Cause.Type)
	fmt.Fprintf(w, "Created:     %s\n", run.CreatedAt.Format(time.RFC3339))
	if run.StartedAt != nil {
		fmt.Fprintf(w, "Started:     %s\n", run.StartedAt.Format(time.RFC3339))
	}
	if run.TerminalAt != nil {
		fmt.Fprintf(w, "Terminal:    %s\n", run.TerminalAt.Format(time.RFC3339))
	}
	if run.Failure != nil {
		fmt.Fprintf(w, "Failure:     %s: %s\n", run.Failure.Code, run.Failure.Message)
	}
}

func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
