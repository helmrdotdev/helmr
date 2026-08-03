import { A } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { formatRelative, StatusBadge } from "../features/runs/display";
import { runHref } from "../features/runs/navigation";
import { ApiError } from "../lib/api";
import { listRuns, type RunFilter } from "../lib/runs";
import { useScope } from "../lib/scope";
import { Select, type SelectOption } from "../ui/Select";
import { ui } from "../ui/styles";

const FILTERS: SelectOption<RunFilter>[] = [
  { value: "all", label: "All runs" },
  { value: "live", label: "Live" },
  { value: "waiting", label: "Waiting" },
  { value: "succeeded", label: "Succeeded" },
  { value: "failed", label: "Failed" },
  { value: "system-failed", label: "System failed" },
  { value: "cancelled", label: "Cancelled" },
  { value: "expired", label: "Expired" },
];

function runsErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.errorKind === "forbidden") {
    return "You do not have permission to view runs.";
  }
  return "Could not load runs.";
}

export function Runs() {
  const scope = useScope();
  const [filter, setFilter] = createSignal<RunFilter>("all");
  const runs = createQuery(() => ({
    queryKey: ["runs", filter(), scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () => listRuns({
      projectID: scope.selectedProjectID(),
      environmentID: scope.selectedEnvironmentID(),
      filter: filter(),
      limit: 100,
    }),
    enabled: !!scope.selectedProjectID() && !!scope.selectedEnvironmentID(),
    retry: false,
  }));
  const items = createMemo(() => runs.data?.runs ?? []);

  return (
    <section class={ui.page}>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Runs</h1>
          <p class={ui.pageSubtitle}>
            Task and Actor execution history for the selected environment.
          </p>
        </div>
        <div class="w-44">
          <Select<RunFilter>
            value={filter()}
            options={FILTERS}
            onChange={setFilter}
            ariaLabel="Filter runs"
          />
        </div>
      </div>

      <Show when={runs.isError}>
        <p class={ui.error} role="alert">{runsErrorMessage(runs.error)}</p>
      </Show>
      <Show when={!runs.isPending} fallback={<p class={ui.muted}>Loading runs...</p>}>
        <Show
          when={items().length > 0}
          fallback={
            <div class={ui.emptyState}>
              <strong>No runs match this filter.</strong>
              <span>Start a declared Task or Actor to create a Run.</span>
            </div>
          }
        >
          <div class={ui.tableWrap}>
            <table class={ui.dataTable}>
              <thead>
                <tr>
                  <th>Entrypoint</th>
                  <th>Status</th>
                  <th>Workspace</th>
                  <th>Attempt</th>
                  <th>Created</th>
                  <th>Run</th>
                </tr>
              </thead>
              <tbody>
                <For each={items()}>
                  {(run) => (
                    <tr>
                      <td>
                        <A
                          href={runHref(run.id, scope.selectedProjectID(), scope.selectedEnvironmentID())}
                          class="font-medium text-console-text hover:text-console-accent"
                        >
                          {run.entrypoint.id}
                        </A>
                      </td>
                      <td><StatusBadge status={run.status} /></td>
                      <td>
                        <A
                          href={`/workspaces/${run.workspace_id}`}
                          class="font-mono text-[11.5px] text-console-accent hover:text-console-accent-hover"
                        >
                          {run.workspace_id.slice(0, 12)}
                        </A>
                      </td>
                      <td>{run.current_attempt_number}</td>
                      <td><span class={ui.muted}>{formatRelative(run.created_at)}</span></td>
                      <td><code>{run.id.slice(0, 12)}</code></td>
                    </tr>
                  )}
                </For>
              </tbody>
            </table>
          </div>
        </Show>
      </Show>
    </section>
  );
}
