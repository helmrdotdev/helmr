package ui

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/helmrdotdev/helmr/internal/api"
)

func RunTable(w io.Writer, runs []api.RunResponse) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN ID\tTASK\tSTATUS\tWAIT")
	for _, run := range runs {
		wait := ""
		if run.PendingWait != nil {
			wait = run.PendingWait.Kind + ":" + run.PendingWait.ID
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", shortID(run.ID), run.TaskID, run.Status, wait)
	}
	_ = tw.Flush()
}

func RunDetails(w io.Writer, run api.RunResponse) {
	fmt.Fprintf(w, "ID:       %s\n", run.ID)
	fmt.Fprintf(w, "Task:     %s\n", run.TaskID)
	fmt.Fprintf(w, "Status:   %s\n", run.Status)
	if run.ExitCode != nil {
		fmt.Fprintf(w, "Exit:     %d\n", *run.ExitCode)
	}
	fmt.Fprintf(w, "Created:  %s\n", run.CreatedAt.Format("2006-01-02T15:04:05Z07:00"))
	fmt.Fprintf(w, "Updated:  %s\n", run.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"))
	if run.PendingWait != nil {
		fmt.Fprintf(w, "Wait:     %s %s\n", run.PendingWait.Kind, run.PendingWait.ID)
	}
}

func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
