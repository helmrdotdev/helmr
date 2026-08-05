import { A } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createMemo, For, Show, type JSX } from "solid-js";
import { formatRelative, StatusBadge } from "../features/runs/display";
import { runHref } from "../features/runs/navigation";
import { listRuns, type Run } from "../lib/runs";
import { listSchedules } from "../lib/schedules";
import { useScope } from "../lib/scope";
import { ui } from "../ui/styles";

function DashboardPanel(props: { title: string; action?: JSX.Element; children: JSX.Element }) {
  return (
    <section class="min-w-0">
      <div class="mb-2 flex min-h-7 items-center justify-between gap-3">
        <h2 class={ui.h2}>{props.title}</h2>
        {props.action}
      </div>
      {props.children}
    </section>
  );
}

function DashboardPlaceholder(props: { children: JSX.Element }) {
  return (
    <div class="m-0 flex min-h-27 flex-col items-center justify-center gap-1.5 border border-dashed border-console-border bg-console-bg-panel px-5 py-6 text-center text-[12.5px] text-console-muted">
      {props.children}
    </div>
  );
}

function RunTable(props: { runs: Run[]; projectID: string; environmentID: string }) {
  return (
    <div class={ui.tableWrap}>
      <table class="min-w-140">
        <thead>
          <tr>
            <th>Entrypoint</th>
            <th>Status</th>
            <th>Workspace</th>
            <th>Created</th>
          </tr>
        </thead>
        <tbody>
          <For each={props.runs}>
            {(run) => (
              <tr>
                <td>
                  <A
                    href={runHref(run.id, props.projectID, props.environmentID)}
                    class="font-medium text-console-text hover:text-console-accent"
                  >
                    {run.entrypoint.id}
                  </A>
                </td>
                <td><StatusBadge status={run.status} /></td>
                <td><code>{run.workspace_id.slice(0, 12)}</code></td>
                <td><span class={ui.muted}>{formatRelative(run.created_at)}</span></td>
              </tr>
            )}
          </For>
        </tbody>
      </table>
    </div>
  );
}

export function Dashboard() {
  const scope = useScope();
  const runtimeScope = () => ({
    projectID: scope.selectedProjectID(),
    environmentID: scope.selectedEnvironmentID(),
  });
  const enabled = () => !!scope.selectedProjectID() && !!scope.selectedEnvironmentID();
  const liveRuns = createQuery(() => ({
    queryKey: ["runs", "dashboard", "live", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () => listRuns({ ...runtimeScope(), filter: "live", limit: 8 }),
    enabled: enabled(),
    retry: false,
  }));
  const recentRuns = createQuery(() => ({
    queryKey: ["runs", "dashboard", "recent", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () => listRuns({ ...runtimeScope(), filter: "all", limit: 8 }),
    enabled: enabled(),
    retry: false,
  }));
  const failedRuns = createQuery(() => ({
    queryKey: ["runs", "dashboard", "failed", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () => listRuns({
      ...runtimeScope(),
      statuses: ["failed", "system_failed"],
      limit: 6,
    }),
    enabled: enabled(),
    retry: false,
  }));
  const schedules = createQuery(() => ({
    queryKey: ["schedules", "dashboard", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () => listSchedules(runtimeScope()),
    enabled: enabled(),
    retry: false,
  }));
  const liveItems = createMemo(() => liveRuns.data?.runs ?? []);
  const recentItems = createMemo(() => recentRuns.data?.runs ?? []);
  const failedItems = createMemo(() => failedRuns.data?.runs ?? []);
  const scheduleItems = createMemo(() => (schedules.data?.schedules ?? []).slice(0, 6));

  return (
    <section class={ui.page}>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Dashboard</h1>
          <p class={ui.pageSubtitle}>
            Current environment activity across Runs, Workspaces, schedules, and failures.
          </p>
        </div>
      </div>

      <div class="grid grid-cols-2 gap-4 max-[980px]:grid-cols-1">
        <DashboardPanel title="Live runs" action={<A class={ui.ghostButton} href="/runs">View runs</A>}>
          <Show when={!liveRuns.isPending} fallback={<p class={ui.muted}>Loading live runs...</p>}>
            <Show
              when={liveItems().length > 0}
              fallback={<DashboardPlaceholder><strong>No live runs.</strong><span>Queued, running, or waiting Runs appear here.</span></DashboardPlaceholder>}
            >
              <RunTable runs={liveItems()} projectID={scope.selectedProjectID()} environmentID={scope.selectedEnvironmentID()} />
            </Show>
          </Show>
        </DashboardPanel>

        <DashboardPanel title="Recent runs" action={<A class={ui.ghostButton} href="/runs">View all</A>}>
          <Show when={!recentRuns.isPending} fallback={<p class={ui.muted}>Loading recent runs...</p>}>
            <Show
              when={recentItems().length > 0}
              fallback={<DashboardPlaceholder><strong>No runs yet.</strong><span>Started Tasks and Actors appear here.</span></DashboardPlaceholder>}
            >
              <RunTable runs={recentItems()} projectID={scope.selectedProjectID()} environmentID={scope.selectedEnvironmentID()} />
            </Show>
          </Show>
        </DashboardPanel>

        <DashboardPanel title="Failed runs">
          <Show when={!failedRuns.isPending} fallback={<p class={ui.muted}>Loading failed runs...</p>}>
            <Show
              when={failedItems().length > 0}
              fallback={<DashboardPlaceholder><strong>No failed runs.</strong><span>Application and system failures appear here.</span></DashboardPlaceholder>}
            >
              <RunTable runs={failedItems()} projectID={scope.selectedProjectID()} environmentID={scope.selectedEnvironmentID()} />
            </Show>
          </Show>
        </DashboardPanel>

        <DashboardPanel title="Schedules" action={<A class={ui.ghostButton} href="/schedules">View schedules</A>}>
          <Show when={!schedules.isPending} fallback={<p class={ui.muted}>Loading schedules...</p>}>
            <Show
              when={scheduleItems().length > 0}
              fallback={<DashboardPlaceholder><strong>No schedules configured.</strong><span>Source-declared Task schedules appear here.</span></DashboardPlaceholder>}
            >
              <div class={ui.tableWrap}>
                <table class="min-w-120">
                  <thead><tr><th>Task</th><th>Status</th><th>Next</th></tr></thead>
                  <tbody>
                    <For each={scheduleItems()}>
                      {(schedule) => (
                        <tr>
						  <td><strong>{schedule.task_id}</strong></td>
                          <td>{schedule.status}</td>
                          <td><span class={ui.muted}>{formatRelative(schedule.next_fire_at)}</span></td>
                        </tr>
                      )}
                    </For>
                  </tbody>
                </table>
              </div>
            </Show>
          </Show>
        </DashboardPanel>
      </div>
    </section>
  );
}
