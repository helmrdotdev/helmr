import { A } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { For, Show } from "solid-js";
import { formatRelative, StatusBadge } from "../features/runs/display";
import { runHref, useRunRowNavigation } from "../features/runs/navigation";
import { ApiError } from "../lib/api";
import { listRuns } from "../lib/runs";
import { useScope } from "../lib/scope";
import { ui } from "../ui/styles";

function approvalsErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.errorKind === "forbidden") {
    return "You do not have permission to view waiting runs.";
  }
  return "Could not load waiting runs.";
}

export function Approvals() {
  const scope = useScope();
  const runs = createQuery(() => ({
    queryKey: ["runs", "waiting", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () =>
      listRuns({
        filter: "waiting",
        projectID: scope.selectedProjectID(),
        environmentID: scope.selectedEnvironmentID(),
      }),
    enabled: !!scope.selectedProjectID() && !!scope.selectedEnvironmentID(),
    retry: false,
  }));

  return (
    <section class={ui.page}>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Waiting runs</h1>
          <p class={ui.pageSubtitle}>
            Open waitpoints currently blocking runs in the selected environment.
          </p>
        </div>
      </div>

      <Show when={runs.isError}>
        <p class={ui.error} role="alert">{approvalsErrorMessage(runs.error)}</p>
      </Show>

      <Show when={!runs.isPending} fallback={<p class={ui.muted}>Loading approvals…</p>}>
        <Show
          when={(runs.data?.runs.length ?? 0) > 0}
          fallback={
            <div class={ui.emptyState}>
              <strong class="text-console-text">Nothing waiting.</strong>
              <span>Open waitpoints requested by runs will show up here.</span>
            </div>
          }
        >
          <div class={ui.tableWrap}>
            <table class={ui.dataTable}>
              <thead>
                <tr>
                  <th>Task</th>
                  <th>Status</th>
                  <th>Request</th>
                  <th>Requested</th>
                </tr>
              </thead>
              <tbody>
                <For each={runs.data?.runs ?? []}>
                  {(run) => {
                    const rowNavigation = useRunRowNavigation(() => run);
                    return (
                      <tr class={ui.clickableTableRow} {...rowNavigation}>
                        <td>
                          <A
                            href={runHref(run.id, run.project_id, run.environment_id)}
                            class={"cursor-pointer font-medium text-console-text hover:text-console-accent"}
                          >
                            {run.task_id}
                          </A>
                        </td>
                        <td><StatusBadge status={run.status} /></td>
                        <td>
                          <span class={ui.muted}>
                            {run.pending_waitpoint?.display_text ?? "—"}
                          </span>
                        </td>
                        <td>
                          <span class={ui.muted}>
                            {formatRelative(run.pending_waitpoint?.requested_at ?? run.updated_at)}
                          </span>
                        </td>
                      </tr>
                    );
                  }}
                </For>
              </tbody>
            </table>
          </div>
        </Show>
      </Show>
    </section>
  );
}
