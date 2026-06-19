import { A } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createMemo, For, Show } from "solid-js";
import { formatRelative } from "../features/runs/display";
import { runHref } from "../features/runs/navigation";
import { TaskSessionStatusBadge } from "../features/sessions/display";
import { sessionHref } from "../features/sessions/navigation";
import { ApiError } from "../lib/api";
import { getCurrentDeployment, type Deployment, type DeploymentStatus, type DeploymentTask } from "../lib/deployments";
import { listRuns, type Run } from "../lib/runs";
import { listSchedules, type Schedule } from "../lib/schedules";
import { useScope } from "../lib/scope";
import { listTaskSessions, type TaskSession } from "../lib/task-sessions";
import { statusBadgeClass, ui } from "../ui/styles";

function tasksErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.errorKind === "forbidden") {
    return "You do not have permission to view deployment tasks.";
  }
  return "Could not load deployment tasks.";
}

function shortID(id: string): string {
  return id.slice(0, 8);
}

function shortDigest(digest: string): string {
  if (!digest) return "-";
  const [algorithm, value] = digest.split(":", 2);
  if (!value) return digest.length > 18 ? `${digest.slice(0, 18)}...` : digest;
  return `${algorithm}:${value.slice(0, 12)}`;
}

function statusLabel(status: DeploymentStatus): string {
  return status.charAt(0).toUpperCase() + status.slice(1);
}

function statusTone(status: DeploymentStatus): "active" | "waiting" | "succeeded" | "revoked" {
  if (status === "queued") return "active";
  if (status === "building") return "waiting";
  if (status === "failed") return "revoked";
  return "succeeded";
}

function DeploymentStatusBadge(props: { status: DeploymentStatus }) {
  return (
    <span class={statusBadgeClass(statusTone(props.status))}>
      {statusLabel(props.status)}
    </span>
  );
}

function deploymentTime(deployment: Deployment): { label: string; value: string } {
  if (deployment.deployed_at) return { label: "Deployed", value: deployment.deployed_at };
  if (deployment.failed_at) return { label: "Failed", value: deployment.failed_at };
  if (deployment.built_at) return { label: "Built", value: deployment.built_at };
  if (deployment.building_at) return { label: "Building", value: deployment.building_at };
  return { label: "Created", value: deployment.created_at };
}

function TaskRow(props: { task: DeploymentTask }) {
  return (
    <tr>
      <td><strong class="font-medium text-console-text">{props.task.task_id}</strong></td>
      <td><code>{props.task.file_path || "-"}</code></td>
      <td><code>{props.task.export_name || "-"}</code></td>
      <td><code>{props.task.handler_entrypoint || "-"}</code></td>
      <td><code>{shortDigest(props.task.bundle_digest || "")}</code></td>
      <td><span class={ui.muted}>{formatRelative(props.task.created_at)}</span></td>
      <td><code>{shortID(props.task.id)}</code></td>
    </tr>
  );
}

function ScheduleRow(props: { schedule: Schedule }) {
  return (
    <tr>
      <td><strong class={"font-medium text-console-text"}>{props.schedule.task}</strong></td>
      <td>{props.schedule.active ? "active" : "inactive"}</td>
      <td><code>{props.schedule.cron}</code></td>
      <td><span class={ui.muted}>{formatRelative(props.schedule.next_fire_at)}</span></td>
      <td><span class={ui.muted}>{formatRelative(props.schedule.last_fire_at)}</span></td>
    </tr>
  );
}

function SessionRow(props: { session: TaskSession }) {
  return (
    <tr>
      <td>
        <A href={sessionHref(props.session.id, props.session.project_id, props.session.environment_id)} class={"font-medium text-console-text hover:text-console-accent"}>
          {props.session.task_id}
        </A>
      </td>
      <td><TaskSessionStatusBadge status={props.session.status} /></td>
      <td><code>{props.session.external_id || "—"}</code></td>
      <td><code>{props.session.current_run_id ? props.session.current_run_id.slice(0, 8) : "—"}</code></td>
      <td><span class={ui.muted}>{formatRelative(props.session.updated_at)}</span></td>
      <td><code>{props.session.id.slice(0, 8)}</code></td>
    </tr>
  );
}

function RunRow(props: { run: Run }) {
  return (
    <tr>
      <td>
        <A href={runHref(props.run.id, props.run.project_id, props.run.environment_id)} class={"font-medium text-console-text hover:text-console-accent"}>
          {props.run.task_id}
        </A>
      </td>
      <td>{props.run.status}</td>
      <td>
        <A href={sessionHref(props.run.task_session_id, props.run.project_id, props.run.environment_id)} class={"font-mono text-[11.5px] text-console-accent hover:text-console-accent-hover"}>
          {props.run.task_session_id.slice(0, 8)}
        </A>
      </td>
      <td><span class={ui.muted}>{formatRelative(props.run.updated_at)}</span></td>
      <td><code>{props.run.id.slice(0, 8)}</code></td>
    </tr>
  );
}

function TasksOnboarding() {
  return (
    <div class={"border border-console-border-strong bg-console-surface"}>
      <div class={"border-b border-console-border bg-console-bg-panel px-4 py-3"}>
        <h2 class={ui.h2}>Set up your first task</h2>
        <p class={ui.pageSubtitle}>
          Create a task project, add a task file, then deploy it to the selected project and environment.
        </p>
      </div>
      <div class={"grid gap-0 divide-y divide-console-border-soft"}>
        <section class={"grid gap-2 px-4 py-3"}>
          <div class={"flex items-center gap-2"}>
            <span class={"grid size-5 shrink-0 place-items-center border border-console-border bg-console-bg-panel font-mono text-[10.5px] font-medium text-console-muted"}>1</span>
            <h3 class={"m-0 text-[13px] font-medium text-console-text"}>Initialize a task project</h3>
          </div>
          <code class={"block overflow-x-auto border border-console-border bg-console-bg-panel px-3 py-2 font-mono text-[12px] text-console-text"}>
            helmr init --dir ./my-helmr-tasks
          </code>
        </section>
        <section class={"grid gap-2 px-4 py-3"}>
          <div class={"flex items-center gap-2"}>
            <span class={"grid size-5 shrink-0 place-items-center border border-console-border bg-console-bg-panel font-mono text-[10.5px] font-medium text-console-muted"}>2</span>
            <h3 class={"m-0 text-[13px] font-medium text-console-text"}>Define tasks under your configured directory</h3>
          </div>
          <code class={"block overflow-x-auto whitespace-pre border border-console-border bg-console-bg-panel px-3 py-2 font-mono text-[12px] leading-relaxed text-console-text"}>{`import { defineConfig } from "@helmr/sdk"

export default defineConfig({
  project: "my-helmr-tasks",
  dirs: ["./tasks"],
})`}</code>
        </section>
        <section class={"grid gap-2 px-4 py-3"}>
          <div class={"flex items-center gap-2"}>
            <span class={"grid size-5 shrink-0 place-items-center border border-console-border bg-console-bg-panel font-mono text-[10.5px] font-medium text-console-muted"}>3</span>
            <h3 class={"m-0 text-[13px] font-medium text-console-text"}>Deploy task definitions</h3>
          </div>
          <code class={"block overflow-x-auto border border-console-border bg-console-bg-panel px-3 py-2 font-mono text-[12px] text-console-text"}>
            helmr deploy ./my-helmr-tasks
          </code>
        </section>
      </div>
    </div>
  );
}

export function Tasks() {
  const scope = useScope();
  const query = createQuery(() => ({
    queryKey: ["deployments", "current", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () =>
      getCurrentDeployment({
        projectID: scope.selectedProjectID(),
        environmentID: scope.selectedEnvironmentID(),
      }),
    enabled: !!scope.selectedProjectID() && !!scope.selectedEnvironmentID(),
    retry: false,
  }));
  const deployment = createMemo(() => query.data?.deployment ?? null);
  const tasks = createMemo(() => deployment()?.tasks ?? []);
  const runtimeScope = () => ({
    projectID: scope.selectedProjectID(),
    environmentID: scope.selectedEnvironmentID(),
  });
  const sessions = createQuery(() => ({
    queryKey: ["task-sessions", "recent", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () => listTaskSessions({ ...runtimeScope(), limit: 8 }),
    enabled: !!scope.selectedProjectID() && !!scope.selectedEnvironmentID(),
    retry: false,
  }));
  const runs = createQuery(() => ({
    queryKey: ["runs", "recent", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () => listRuns({ ...runtimeScope(), limit: 8, filter: "all" }),
    enabled: !!scope.selectedProjectID() && !!scope.selectedEnvironmentID(),
    retry: false,
  }));
  const schedules = createQuery(() => ({
    queryKey: ["schedules", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () => listSchedules(runtimeScope()),
    enabled: !!scope.selectedProjectID() && !!scope.selectedEnvironmentID(),
    retry: false,
  }));
  const recentSessions = createMemo(() => sessions.data?.sessions ?? []);
  const recentRuns = createMemo(() => runs.data?.runs ?? []);
  const scheduleItems = createMemo(() => schedules.data?.schedules ?? []);

  return (
    <section class={ui.page}>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Tasks</h1>
          <p class={ui.pageSubtitle}>
            Current task definitions deployed for the selected environment.
          </p>
        </div>
      </div>

      <Show when={query.isError}>
        <p class={ui.error} role="alert">{tasksErrorMessage(query.error)}</p>
      </Show>

      <Show when={query.isPending}>
        <p class={ui.muted}>Loading tasks...</p>
      </Show>

        <Show when={!query.isPending && !query.isError}>
          <Show when={deployment()} fallback={<TasksOnboarding />}>
            {(currentDeployment) => (
              <>
                <div class={"mb-3 flex flex-wrap items-center gap-x-4 gap-y-1.5 text-[12.5px] text-console-muted"}>
                  <span><DeploymentStatusBadge status={currentDeployment().status} /></span>
                  <span><strong class="font-medium text-console-text">{tasks().length}</strong> tasks</span>
                  <span>
                    {deploymentTime(currentDeployment()).label}{" "}
                    <strong class="font-medium text-console-text">{formatRelative(deploymentTime(currentDeployment()).value)}</strong>
                  </span>
                  <span>Source <code>{shortDigest(currentDeployment().deployment_source.digest)}</code></span>
                  <Show when={currentDeployment().build_manifest_digest}>
                    {(digest) => <span>Build manifest <code>{shortDigest(digest())}</code></span>}
                  </Show>
                  <Show when={currentDeployment().deployment_manifest_digest}>
                    {(digest) => <span>Deployment manifest <code>{shortDigest(digest())}</code></span>}
                  </Show>
                    <span>Deployment <code>{shortID(currentDeployment().id)}</code></span>
                  </div>

                  <div class={ui.tableWrap}>
                    <table class={ui.dataTable}>
                      <thead>
                        <tr>
                          <th>Task</th>
                          <th>File</th>
                          <th>Export</th>
                          <th>Handler</th>
                          <th>Bundle</th>
                          <th>Created</th>
                          <th>ID</th>
                        </tr>
                      </thead>
                      <tbody>
                        <For
                          each={tasks()}
                          fallback={
                            <tr>
                              <td colSpan={7}>
                                <span class={ui.muted}>No task entries are available for this deployment yet.</span>
                              </td>
                            </tr>
                          }
                        >
                          {(task) => <TaskRow task={task} />}
                        </For>
                      </tbody>
                    </table>
                  </div>
              </>
            )}
          </Show>

          <div class={"mt-5 grid gap-5"}>
            <section>
              <div class={"mb-2 flex items-center justify-between gap-3"}>
                <h2 class={ui.h2}>Schedules</h2>
                <A class={ui.ghostButton} href="/schedules">Open schedules</A>
              </div>
              <div class={ui.tableWrap}>
                <table class={"min-w-175"}>
                  <thead>
                    <tr>
                      <th>Task</th>
                      <th>Status</th>
                      <th>Cron</th>
                      <th>Next</th>
                      <th>Last</th>
                    </tr>
                  </thead>
                  <tbody>
                    <For
                      each={scheduleItems()}
                      fallback={
                        <tr>
                          <td colSpan={5}><span class={ui.muted}>No schedules are configured.</span></td>
                        </tr>
                      }
                    >
                      {(schedule) => <ScheduleRow schedule={schedule} />}
                    </For>
                  </tbody>
                </table>
              </div>
            </section>

            <section>
              <div class={"mb-2 flex items-center justify-between gap-3"}>
                <h2 class={ui.h2}>Recent sessions</h2>
              </div>
              <div class={ui.tableWrap}>
                <table class={"min-w-200"}>
                  <thead>
                    <tr>
                      <th>Task</th>
                      <th>Status</th>
                      <th>External ID</th>
                      <th>Current run</th>
                      <th>Updated</th>
                      <th>ID</th>
                    </tr>
                  </thead>
                  <tbody>
                    <For
                      each={recentSessions()}
                      fallback={
                        <tr>
                          <td colSpan={6}><span class={ui.muted}>No task sessions yet.</span></td>
                        </tr>
                      }
                    >
                      {(session) => <SessionRow session={session} />}
                    </For>
                  </tbody>
                </table>
              </div>
            </section>

            <section>
              <div class={"mb-2 flex items-center justify-between gap-3"}>
                <h2 class={ui.h2}>Recent runs</h2>
                <A class={ui.ghostButton} href="/runs">Open runs</A>
              </div>
              <div class={ui.tableWrap}>
                <table class={"min-w-175"}>
                  <thead>
                    <tr>
                      <th>Task</th>
                      <th>Status</th>
                      <th>Session</th>
                      <th>Updated</th>
                      <th>ID</th>
                    </tr>
                  </thead>
                  <tbody>
                    <For
                      each={recentRuns()}
                      fallback={
                        <tr>
                          <td colSpan={5}><span class={ui.muted}>No runs yet.</span></td>
                        </tr>
                      }
                    >
                      {(run) => <RunRow run={run} />}
                    </For>
                  </tbody>
                </table>
              </div>
            </section>
          </div>
        </Show>
      </section>
    );
}
