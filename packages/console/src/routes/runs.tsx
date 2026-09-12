import { A } from "@solidjs/router";
import { createInfiniteQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { runHref } from "../features/runs/navigation";
import { ApiError } from "../lib/api";
import { listRuns, type RunFilter, type RunKindFilter } from "../lib/runs";
import { useScope } from "../lib/scope";
import { runSessionConsolePath } from "../lib/sessions";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { RelativeTime } from "../ui/RelativeTime";
import { Select, type SelectOption } from "../ui/Select";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";

const FILTERS: SelectOption<RunFilter>[] = [
  { value: "all", label: "All runs" },
  { value: "live", label: "Live" },
  { value: "waiting", label: "Waiting" },
  { value: "succeeded", label: "Succeeded" },
  { value: "failed", label: "Failed" },
  { value: "system_failed", label: "System failed" },
  { value: "cancelled", label: "Cancelled" },
  { value: "expired", label: "Expired" },
];

const KINDS: SelectOption<RunKindFilter>[] = [
  { value: "all", label: "All kinds" },
  { value: "task", label: "Task" },
  { value: "actor", label: "Actor" },
];

function runsErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.code === "forbidden") {
    return "You do not have permission to view runs.";
  }
  return "Could not load runs.";
}

export function Runs() {
  const scope = useScope();
  const projectID = () => scope.selectedProjectID();
  const environmentID = () => scope.selectedEnvironmentID();
  const [filter, setFilter] = createSignal<RunFilter>("all");
  const [kind, setKind] = createSignal<RunKindFilter>("all");
  const runs = createInfiniteQuery(() => ({
    queryKey: ["runs", "list", filter(), kind(), projectID(), environmentID()],
    queryFn: ({ pageParam }) => listRuns({
      projectID: projectID(),
      environmentID: environmentID(),
      filter: filter(),
      kind: kind(),
      cursor: pageParam || undefined,
      limit: 100,
    }),
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor,
    enabled: !!projectID() && !!environmentID(),
    retry: false,
  }));
  const items = createMemo(() => runs.data?.pages.flatMap((page) => page.runs) ?? []);

  return (
    <section class={ui.page}>
      <PageHeader
        title="Runs"
        subtitle="Task and Actor execution history for the selected environment."
        actions={
          <>
            <div class="w-32">
              <Select<RunKindFilter> value={kind()} options={KINDS} onChange={setKind} ariaLabel="Filter runs by kind" />
            </div>
            <div class="w-44">
              <Select<RunFilter> value={filter()} options={FILTERS} onChange={setFilter} ariaLabel="Filter runs" />
            </div>
          </>
        }
      />

      <Show when={runs.isError}>
        <StatePanel error={runsErrorMessage(runs.error)} />
      </Show>
      <Show when={!runs.isPending} fallback={<StatePanel loading="Loading runs..." />}>
        <Show
          when={items().length > 0}
          fallback={<StatePanel empty="No runs match this filter." hint="Start a declared Task or Actor to create a Run." />}
        >
          <DataTable columns={["Entrypoint", "Kind", "Status", "Session", "Workspace", "Attempt", "Created", "Run"]} minWidth="min-w-250">
            <For each={items()}>
              {(run) => (
                <tr>
                  <td>
                    <A
                      href={runHref(run.id, projectID(), environmentID())}
                      class="font-medium text-console-text hover:text-console-accent"
                    >
                      {run.entrypoint.id}
                    </A>
                  </td>
                  <td><span class={ui.muted}>{run.entrypoint.kind}</span></td>
                  <td><StatusBadge resource="run" status={run.status} /></td>
                  <td>
                    <IDText
                      value={run.session_id ?? ""}
                      href={runSessionConsolePath(run, projectID(), environmentID())}
                    />
                  </td>
                  <td><IDText value={run.workspace_id} href={`/workspaces/${run.workspace_id}`} /></td>
                  <td>{run.current_attempt_number}</td>
                  <td><RelativeTime value={run.created_at} /></td>
                  <td><IDText value={run.id} /></td>
                </tr>
              )}
            </For>
          </DataTable>
          <Show when={runs.hasNextPage}>
            <div class={ui.actionRow}>
              <button
                type="button"
                class={ui.secondaryButton}
                disabled={runs.isFetchingNextPage}
                onClick={() => void runs.fetchNextPage()}
              >
                {runs.isFetchingNextPage ? "Loading..." : "Load more"}
              </button>
            </div>
          </Show>
        </Show>
      </Show>
    </section>
  );
}
