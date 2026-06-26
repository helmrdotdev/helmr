import { A } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createMemo, For, Show, type JSX } from "solid-js";
import { formatRelative, StatusBadge } from "../features/runs/display";
import { runHref } from "../features/runs/navigation";
import { SessionStatusBadge } from "../features/sessions/display";
import { sessionHref } from "../features/sessions/navigation";
import { listRuns } from "../lib/runs";
import { listSchedules } from "../lib/schedules";
import { useScope } from "../lib/scope";
import { listSessions, type Session } from "../lib/sessions";
import { ui } from "../ui/styles";

function shortID(id: string | undefined): string {
  return id ? id.slice(0, 8) : "—";
}

function DashboardSessionRow(props: { session: Session }) {
  return (
    <tr>
      <td>
        <A href={sessionHref(props.session.id, props.session.project_id, props.session.environment_id)} class={"font-medium text-console-text hover:text-console-accent"}>
          {props.session.task_id}
        </A>
      </td>
      <td><SessionStatusBadge status={props.session.status} /></td>
      <td><code>{shortID(props.session.workspace_id)}</code></td>
      <td><code>{shortID(props.session.current_run_id)}</code></td>
      <td><span class={ui.muted}>{formatRelative(props.session.updated_at)}</span></td>
    </tr>
  );
}

function DashboardPanel(props: { title: string; action?: JSX.Element; children: JSX.Element }) {
  return (
    <section class={"min-w-0"}>
      <div class={"mb-2 flex min-h-7 items-center justify-between gap-3"}>
        <h2 class={ui.h2}>{props.title}</h2>
        {props.action}
      </div>
      {props.children}
    </section>
  );
}

function DashboardPlaceholder(props: { children: JSX.Element }) {
  return (
    <div class={"m-0 flex min-h-27 flex-col items-center justify-center gap-1.5 border border-dashed border-console-border bg-console-bg-panel px-5 py-6 text-center text-[12.5px] text-console-muted"}>
      {props.children}
    </div>
  );
}

export function Dashboard() {
  const scope = useScope();
  const runtimeScope = () => ({
    projectID: scope.selectedProjectID(),
    environmentID: scope.selectedEnvironmentID(),
  });
  const openSessions = createQuery(() => ({
    queryKey: ["sessions", "dashboard", "open", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () => listSessions({ ...runtimeScope(), status: "open", limit: 8 }),
    enabled: !!scope.selectedProjectID() && !!scope.selectedEnvironmentID(),
    retry: false,
  }));
  const recentSessions = createQuery(() => ({
    queryKey: ["sessions", "dashboard", "recent", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () => listSessions({ ...runtimeScope(), status: "all", limit: 8 }),
    enabled: !!scope.selectedProjectID() && !!scope.selectedEnvironmentID(),
    retry: false,
  }));
  const failedRuns = createQuery(() => ({
    queryKey: ["runs", "dashboard", "failed", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () => listRuns({ ...runtimeScope(), filter: "failed", limit: 6 }),
    enabled: !!scope.selectedProjectID() && !!scope.selectedEnvironmentID(),
    retry: false,
  }));
  const schedules = createQuery(() => ({
    queryKey: ["schedules", "dashboard", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () => listSchedules(runtimeScope()),
    enabled: !!scope.selectedProjectID() && !!scope.selectedEnvironmentID(),
    retry: false,
  }));
  const openSessionItems = createMemo(() => openSessions.data?.sessions ?? []);
  const recentSessionItems = createMemo(() => recentSessions.data?.sessions ?? []);
  const failedRunItems = createMemo(() => failedRuns.data?.runs ?? []);
  const scheduleItems = createMemo(() => (schedules.data?.schedules ?? []).slice(0, 6));

  return (
    <section class={ui.page}>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Dashboard</h1>
          <p class={ui.pageSubtitle}>
            Current environment activity across sessions, schedules, deployment state, and failed attempts.
          </p>
        </div>
      </div>

      <div class={"grid grid-cols-2 gap-4 max-[980px]:grid-cols-1"}>
        <DashboardPanel title="Open sessions" action={<A class={ui.ghostButton} href="/sessions">View sessions</A>}>
          <Show when={!openSessions.isPending} fallback={<p class={ui.muted}>Loading sessions...</p>}>
            <Show
              when={openSessionItems().length > 0}
              fallback={
                <DashboardPlaceholder>
                  <strong class="text-console-text">No open sessions.</strong>
                  <span>Active sessions will appear here.</span>
                </DashboardPlaceholder>
              }
            >
              <div class={ui.tableWrap}>
                <table class={"min-w-180"}>
                  <thead>
                    <tr>
                      <th>Task</th>
                      <th>Status</th>
                      <th>Workspace</th>
                      <th>Current run</th>
                      <th>Updated</th>
                    </tr>
                  </thead>
                  <tbody>
                    <For each={openSessionItems()}>
                      {(session) => <DashboardSessionRow session={session} />}
                    </For>
                  </tbody>
                </table>
              </div>
            </Show>
          </Show>
        </DashboardPanel>

        <DashboardPanel title="Recent sessions" action={<A class={ui.ghostButton} href="/sessions">View all</A>}>
          <Show when={!recentSessions.isPending} fallback={<p class={ui.muted}>Loading recent sessions...</p>}>
            <Show
              when={recentSessionItems().length > 0}
              fallback={
                <DashboardPlaceholder>
                  <strong class="text-console-text">No sessions yet.</strong>
                  <span>Started sessions will appear here.</span>
                </DashboardPlaceholder>
              }
            >
              <div class={ui.tableWrap}>
                <table class={"min-w-180"}>
                  <thead>
                    <tr>
                      <th>Task</th>
                      <th>Status</th>
                      <th>Workspace</th>
                      <th>Current run</th>
                      <th>Updated</th>
                    </tr>
                  </thead>
                  <tbody>
                    <For each={recentSessionItems()}>
                      {(session) => <DashboardSessionRow session={session} />}
                    </For>
                  </tbody>
                </table>
              </div>
            </Show>
          </Show>
        </DashboardPanel>

        <DashboardPanel title="Failed attempts">
          <Show when={!failedRuns.isPending} fallback={<p class={ui.muted}>Loading failed attempts...</p>}>
            <Show
              when={failedRunItems().length > 0}
              fallback={
                <DashboardPlaceholder>
                  <strong class="text-console-text">No failed attempts.</strong>
                  <span>Failed execution attempts will appear here.</span>
                </DashboardPlaceholder>
              }
            >
              <div class={ui.tableWrap}>
                <table class={"min-w-140"}>
                  <thead>
                    <tr>
                      <th>Task</th>
                      <th>Status</th>
                      <th>Session</th>
                      <th>Updated</th>
                    </tr>
                  </thead>
                  <tbody>
                    <For each={failedRunItems()}>
                      {(run) => (
                        <tr>
                          <td>
                            <A href={runHref(run.id, run.session_id, run.project_id, run.environment_id)} class={"font-medium text-console-text hover:text-console-accent"}>
                              {run.task_id}
                            </A>
                          </td>
                          <td><StatusBadge status={run.status} /></td>
                          <td>
                            <A href={sessionHref(run.session_id, run.project_id, run.environment_id)} class={"font-mono text-[11.5px] text-console-accent hover:text-console-accent-hover"}>
                              {shortID(run.session_id)}
                            </A>
                          </td>
                          <td><span class={ui.muted}>{formatRelative(run.updated_at)}</span></td>
                        </tr>
                      )}
                    </For>
                  </tbody>
                </table>
              </div>
            </Show>
          </Show>
        </DashboardPanel>

        <DashboardPanel title="Schedules" action={<A class={ui.ghostButton} href="/schedules">View schedules</A>}>
          <Show when={!schedules.isPending} fallback={<p class={ui.muted}>Loading schedules...</p>}>
            <Show
              when={scheduleItems().length > 0}
              fallback={
                <DashboardPlaceholder>
                  <strong class="text-console-text">No schedules configured.</strong>
                  <span>Scheduled sessions will appear here.</span>
                </DashboardPlaceholder>
              }
            >
              <div class={ui.tableWrap}>
                <table class={"min-w-120"}>
                  <thead>
                    <tr>
                      <th>Task</th>
                      <th>Status</th>
                      <th>Next</th>
                    </tr>
                  </thead>
                  <tbody>
                    <For each={scheduleItems()}>
                      {(schedule) => (
                        <tr>
                          <td><strong class={"font-medium text-console-text"}>{schedule.task}</strong></td>
                          <td>{schedule.active ? "active" : "inactive"}</td>
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
