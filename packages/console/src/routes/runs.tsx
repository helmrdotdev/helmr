import { A } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { Select, type SelectOption } from "../ui/Select";
import { formatRelative, StatusBadge } from "../features/runs/display";
import { runHref, useRunRowNavigation } from "../features/runs/navigation";
import { ApiError } from "../lib/api";
import { countRuns, listRuns, type Run, type RunFilter } from "../lib/runs";
import { useScope } from "../lib/scope";
import { ui } from "../ui/styles";

const FILTER_OPTIONS: SelectOption<RunFilter>[] = [
  { value: "live", label: "Live" },
  { value: "all", label: "All" },
  { value: "queued", label: "Queued" },
  { value: "running", label: "Running" },
  { value: "waiting", label: "Waiting" },
  { value: "succeeded", label: "Succeeded" },
  { value: "failed", label: "Failed" },
  { value: "cancelled", label: "Cancelled" },
];

function runErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.errorKind === "forbidden") {
    return "You do not have permission to view runs.";
  }
  return "Could not load runs.";
}

function RunRow(props: { run: Run; environmentName: string }) {
  const rowNavigation = useRunRowNavigation(() => props.run);

  return (
    <tr class={ui.clickableTableRow} {...rowNavigation}>
      <td>
        <A href={runHref(props.run.id, props.run.project_id, props.run.environment_id)} class={"cursor-pointer font-medium text-console-text hover:text-console-accent"}>
          {props.run.task_id}
        </A>
      </td>
      <td><StatusBadge status={props.run.status} /></td>
      <td><span class={ui.muted}>{props.environmentName}</span></td>
      <td>
        <Show when={props.run.pending_waitpoint} fallback={<span class={"text-console-faint"}>—</span>}>
          {(wait) => <span class={ui.muted}>{wait().kind}</span>}
        </Show>
      </td>
      <td><span class={ui.muted}>{formatRelative(props.run.created_at)}</span></td>
      <td><span class={ui.muted}>{formatRelative(props.run.updated_at)}</span></td>
      <td><code>{props.run.id.slice(0, 8)}</code></td>
    </tr>
  );
}

function RunsOnboarding() {
  return (
    <div class={"border border-console-border-strong bg-console-surface"}>
      <div class={"border-b border-console-border bg-console-bg-panel px-4 py-3"}>
        <h2 class={ui.h2}>Start your first run</h2>
        <p class={ui.pageSubtitle}>
          Run a deployment task, then inspect status and logs from this page.
        </p>
      </div>
      <div class={"grid gap-0 divide-y divide-console-border-soft"}>
        <section class={"grid gap-2 px-4 py-3"}>
          <div class={"flex items-center gap-2"}>
            <span class={"grid size-5 shrink-0 place-items-center border border-console-border bg-console-bg-panel font-mono text-[10.5px] font-medium text-console-muted"}>1</span>
            <h3 class={"m-0 text-[13px] font-medium text-console-text"}>Start a deployment task</h3>
          </div>
          <code class={"block overflow-x-auto whitespace-pre border border-console-border bg-console-bg-panel px-3 py-2 font-mono text-[12px] leading-relaxed text-console-text"}>{`helmr run hello`}</code>
        </section>
        <section class={"grid gap-2 px-4 py-3"}>
          <div class={"flex items-center gap-2"}>
            <span class={"grid size-5 shrink-0 place-items-center border border-console-border bg-console-bg-panel font-mono text-[10.5px] font-medium text-console-muted"}>2</span>
            <h3 class={"m-0 text-[13px] font-medium text-console-text"}>Pass payload when the task needs input</h3>
          </div>
          <code class={"block overflow-x-auto whitespace-pre border border-console-border bg-console-bg-panel px-3 py-2 font-mono text-[12px] leading-relaxed text-console-text"}>{`helmr run hello \\
  --payload-json '{"name":"Ada"}'`}</code>
        </section>
        <section class={"grid gap-2 px-4 py-3"}>
          <div class={"flex items-center gap-2"}>
            <span class={"grid size-5 shrink-0 place-items-center border border-console-border bg-console-bg-panel font-mono text-[10.5px] font-medium text-console-muted"}>3</span>
            <h3 class={"m-0 text-[13px] font-medium text-console-text"}>Inspect the run from the CLI</h3>
          </div>
          <code class={"block overflow-x-auto whitespace-pre border border-console-border bg-console-bg-panel px-3 py-2 font-mono text-[12px] leading-relaxed text-console-text"}>{`helmr ps
helmr show RUN_ID
helmr logs RUN_ID`}</code>
        </section>
      </div>
    </div>
  );
}

export function Runs() {
  const scope = useScope();
  const [filter, setFilter] = createSignal<RunFilter>("all");
  const runs = createQuery(() => ({
    queryKey: ["runs", filter(), scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () =>
      listRuns({
        filter: filter(),
        projectID: scope.selectedProjectID(),
        environmentID: scope.selectedEnvironmentID(),
      }),
    enabled: !!scope.selectedProjectID() && !!scope.selectedEnvironmentID(),
    retry: false,
  }));
  const runSummary = createQuery(() => ({
    queryKey: ["runs", "summary", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () =>
      countRuns({
        projectID: scope.selectedProjectID(),
        environmentID: scope.selectedEnvironmentID(),
      }),
    enabled: !!scope.selectedProjectID() && !!scope.selectedEnvironmentID(),
    retry: false,
  }));
  const runItems = createMemo(() => runs.data?.runs ?? []);
  const liveCount = createMemo(() => (runSummary.data?.queued ?? 0) + (runSummary.data?.running ?? 0));
  const waitingCount = createMemo(() => runSummary.data?.waiting ?? 0);
  const completedCount = createMemo(() => runSummary.data?.succeeded ?? 0);
  const failedCount = createMemo(() => (runSummary.data?.failed ?? 0) + (runSummary.data?.cancelled ?? 0));

  return (
    <section class={ui.page}>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Runs</h1>
          <p class={ui.pageSubtitle}>
            Execution state, waitpoints, and terminal outcomes for the selected environment.
          </p>
        </div>
      </div>

      <div class={ui.metricStrip} aria-label="Run summary">
        <div class={ui.metricCard}>
          <span>Live</span>
          <strong class="text-console-info">{liveCount()}</strong>
        </div>
        <div class={ui.metricCard}>
          <span>Waiting</span>
          <strong class="text-console-warning">{waitingCount()}</strong>
        </div>
        <div class={ui.metricCard}>
          <span>Succeeded</span>
          <strong class="text-console-success">{completedCount()}</strong>
        </div>
        <div class={ui.metricCard}>
          <span>Failed</span>
          <strong class="text-console-danger">{failedCount()}</strong>
        </div>
      </div>

      <div class={ui.toolbar}>
        <div class={ui.toolbarSide} />
        <div class={ui.toolbarSide}>
          <span class={ui.filterField}>
            <span>Filter</span>
            <Select<RunFilter>
              value={filter()}
              options={FILTER_OPTIONS}
              onChange={setFilter}
              ariaLabel="Filter runs"
              minWidth="140px"
            />
          </span>
        </div>
      </div>

      <Show when={runs.isError}>
        <p class={ui.error} role="alert">{runErrorMessage(runs.error)}</p>
      </Show>

      <Show when={!runs.isPending} fallback={<p class={ui.muted}>Loading runs…</p>}>
        <Show
          when={(runs.data?.runs.length ?? 0) > 0}
          fallback={<RunsOnboarding />}
        >
          <div class={ui.tableWrap}>
            <table class={ui.dataTable}>
              <thead>
                <tr>
                  <th>Task</th>
                  <th>Status</th>
                  <th>Environment</th>
                  <th>Wait</th>
                  <th>Created</th>
                  <th>Updated</th>
                  <th>ID</th>
                </tr>
              </thead>
              <tbody>
                <For each={runItems()}>
                  {(run) => (
                    <RunRow
                      run={run}
                      environmentName={scope.selectedEnvironment()?.name ?? "Environment"}
                    />
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
