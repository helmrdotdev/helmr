import { A } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createMemo, For, Show } from "solid-js";
import { formatRelative } from "../features/runs/display";
import { ApiError } from "../lib/api";
import { getCurrentDeployment, type Deployment, type DeploymentStatus, type DeploymentTask } from "../lib/deployments";
import { useScope } from "../lib/scope";
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
  const sessionsHref = createMemo(() => `/sessions?${new URLSearchParams({ task_id: props.task.task_id }).toString()}`);
  return (
    <tr>
      <td><strong class="font-medium text-console-text">{props.task.task_id}</strong></td>
      <td><code>{props.task.file_path || "-"}</code></td>
      <td><code>{props.task.export_name || "-"}</code></td>
      <td><code>{props.task.handler_entrypoint || "-"}</code></td>
      <td><code>{shortDigest(props.task.bundle_digest || "")}</code></td>
      <td><span class={ui.muted}>{formatRelative(props.task.created_at)}</span></td>
      <td><code>{shortID(props.task.id)}</code></td>
      <td>
        <A href={sessionsHref()} class={"font-mono text-[11.5px] text-console-accent hover:text-console-accent-hover"}>
          Sessions
        </A>
      </td>
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
                          <th>History</th>
                        </tr>
                      </thead>
                      <tbody>
                        <For
                          each={tasks()}
                          fallback={
                            <tr>
                              <td colSpan={8}>
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
        </Show>
      </section>
    );
}
