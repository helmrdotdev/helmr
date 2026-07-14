import { A, useParams } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, Show } from "solid-js";
import { formatRelative } from "../features/runs/display";
import { ApiError } from "../lib/api";
import { useScope } from "../lib/scope";
import {
  getWorkspace,
  listWorkspaceExecs,
  materializeWorkspace,
  stopWorkspace,
  type Workspace,
  type WorkspaceExec,
} from "../lib/workspaces";
import { cx, ui } from "../ui/styles";

function isNotFound(error: unknown): boolean {
  return error instanceof ApiError && error.status === 404;
}

function workspaceErrorMessage(error: unknown): string {
  if (isNotFound(error)) return "Workspace not found.";
  if (error instanceof ApiError) return error.message;
  return "Could not load this workspace.";
}

function formattedJSON(value: unknown): string {
  return JSON.stringify(value ?? {}, null, 2);
}

function commandText(command: unknown): string {
  if (Array.isArray(command) && command.every((part) => typeof part === "string")) {
    return command.join(" ");
  }
  return formattedJSON(command);
}

function stateBadge(state: string): string {
  const tone = state === "active"
    ? "border-[#a8c3ad] bg-[#eef7f0] text-console-success"
    : state === "recovery_required"
      ? "border-[#e6aaa4] bg-[#fff1ef] text-console-danger"
      : "border-console-border bg-console-bg-panel text-console-muted";
  return cx(
    "inline-flex items-center whitespace-nowrap rounded-xs border px-2 py-0.5 font-mono text-[11px] font-medium leading-normal",
    tone,
  );
}

function WorkspaceExecHistory(props: { workspace: Workspace; execs: WorkspaceExec[] }) {
  return (
    <section class={"border border-console-border bg-console-surface p-4"}>
      <div class={"mb-3 flex items-center justify-between gap-3"}>
        <h2 class={ui.h2}>Exec history</h2>
        <span class={ui.muted}>{props.execs.length} execs</span>
      </div>
      <Show
        when={props.execs.length > 0}
        fallback={<p class={ui.emptyState}>No commands have been run in this workspace.</p>}
      >
        <div class={ui.tableWrap}>
          <table class={"min-w-190"}>
            <thead>
              <tr>
                <th>Command</th>
                <th>State</th>
                <th>Exit</th>
                <th>Started</th>
                <th>ID</th>
              </tr>
            </thead>
            <tbody>
              <For each={props.execs}>
                {(exec) => (
                  <tr>
                    <td>
                      <A
                        class={"block max-w-110 truncate font-mono text-[11.5px] text-console-accent hover:text-console-accent-hover"}
                        href={`/workspaces/${encodeURIComponent(props.workspace.id)}/execs/${encodeURIComponent(exec.id)}`}
                        title={commandText(exec.command)}
                      >
                        {commandText(exec.command)}
                      </A>
                    </td>
                    <td>{exec.state}</td>
                    <td>{exec.exit_code ?? "—"}</td>
                    <td><span class={ui.muted}>{formatRelative(exec.started_at ?? exec.created_at)}</span></td>
                    <td><code>{exec.id.slice(0, 8)}</code></td>
                  </tr>
                )}
              </For>
            </tbody>
          </table>
        </div>
      </Show>
    </section>
  );
}

function WorkspaceDetails(props: { workspace: Workspace }) {
  return (
    <aside class={"sticky top-13.5 flex flex-col gap-3 max-[960px]:static"}>
      <section class={"border border-console-border bg-console-surface px-4 py-3.5"}>
        <h3 class={cx(ui.h3, "mb-3.5")}>Workspace details</h3>
        <dl class={"m-0 grid gap-2.5 [&>div]:grid [&>div]:gap-0.75 [&_dt]:m-0 [&_dt]:font-mono [&_dt]:text-[10px] [&_dt]:font-medium [&_dt]:uppercase [&_dt]:tracking-[0.06em] [&_dt]:text-console-subtle [&_dd]:m-0 [&_dd]:[overflow-wrap:anywhere] [&_dd]:text-[12.5px] [&_dd]:text-console-text [&_dd_code]:font-mono [&_dd_code]:text-[11.5px]"}>
          <div><dt>ID</dt><dd><code>{props.workspace.id}</code></dd></div>
          <div><dt>Sandbox</dt><dd><code>{props.workspace.sandbox_id}</code></dd></div>
          <div><dt>Current version</dt><dd><code>{props.workspace.current_version_id ?? "—"}</code></dd></div>
          <div><dt>Desired state</dt><dd>{props.workspace.desired_state}</dd></div>
          <div><dt>Dirty state</dt><dd>{props.workspace.dirty_state}</dd></div>
          <div><dt>Last activity</dt><dd>{formatRelative(props.workspace.last_activity_at)}</dd></div>
          <div><dt>Created</dt><dd>{formatRelative(props.workspace.created_at)}</dd></div>
          <div><dt>Updated</dt><dd>{formatRelative(props.workspace.updated_at)}</dd></div>
        </dl>
      </section>
    </aside>
  );
}

export function WorkspaceDetail() {
  const params = useParams();
  const scope = useScope();
  const queryClient = useQueryClient();
  const workspaceID = createMemo(() => params["workspace_id"]?.trim() ?? "");
  const hasWorkspaceID = createMemo(() => workspaceID() !== "");
  const [action, setAction] = createSignal<"materialize" | "stop" | null>(null);
  const [actionError, setActionError] = createSignal<string | null>(null);
  const workspace = createQuery(() => ({
    queryKey: ["workspace", workspaceID()],
    queryFn: () => getWorkspace(workspaceID()),
    enabled: hasWorkspaceID(),
    retry: false,
  }));
  const actualScope = createMemo(() => {
    const current = workspace.data?.workspace;
    return current ? { projectID: current.project_id, environmentID: current.environment_id } : null;
  });
  const execs = createQuery(() => ({
    queryKey: ["workspace-execs", workspaceID(), actualScope()?.projectID, actualScope()?.environmentID],
    queryFn: () => listWorkspaceExecs(workspaceID(), actualScope()!),
    enabled: !!actualScope(),
    retry: false,
  }));

  createEffect(() => {
    const current = workspace.data?.workspace;
    if (!current) return;
    if (scope.selectedProjectID() !== current.project_id) {
      scope.setSelectedProjectID(current.project_id);
    }
    if (scope.selectedEnvironmentID() !== current.environment_id) {
      scope.setSelectedEnvironmentID(current.environment_id);
    }
  });

  createEffect(() => {
    workspaceID();
    setAction(null);
    setActionError(null);
  });

  async function runLifecycleAction(kind: "materialize" | "stop") {
    const current = workspace.data?.workspace;
    const currentScope = actualScope();
    if (!current || !currentScope || action()) return;
    setAction(kind);
    setActionError(null);
    try {
      if (kind === "materialize") {
        await materializeWorkspace(current.id, currentScope);
      } else {
        await stopWorkspace(current.id, currentScope);
      }
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["workspace", current.id] }),
        queryClient.invalidateQueries({ queryKey: ["workspace-execs", current.id] }),
      ]);
    } catch (error) {
      setActionError(workspaceErrorMessage(error));
    } finally {
      setAction(null);
    }
  }

  const retry = () => {
    void workspace.refetch();
    if (workspace.data) void execs.refetch();
  };

  return (
    <section class={ui.page}>
      <div class={ui.pageHeader}>
        <div>
          <div class={ui.pageTitle}>
            <h1 class={ui.h1}>Workspace</h1>
            <Show when={workspace.data?.workspace}>
              {(current) => <span class={stateBadge(current().state)}>{current().state}</span>}
            </Show>
          </div>
          <Show when={workspace.data?.workspace}>
            {(current) => <p class={"mt-1.5 font-mono text-[12.5px] text-console-muted"}>{current().id}</p>}
          </Show>
        </div>
        <Show when={workspace.data?.workspace}>
          <div class={"flex flex-wrap items-center gap-1.5"}>
            <button class={ui.secondaryButton} type="button" disabled={!!action()} onClick={() => void runLifecycleAction("materialize")}>
              {action() === "materialize" ? "Materializing..." : "Materialize"}
            </button>
            <button class={ui.secondaryButton} type="button" disabled={!!action()} onClick={() => void runLifecycleAction("stop")}>
              {action() === "stop" ? "Stopping..." : "Stop"}
            </button>
          </div>
        </Show>
      </div>

      <Show when={actionError()}>
        {(message) => <p class={ui.error} role="alert">{message()}</p>}
      </Show>

      <Show when={hasWorkspaceID()} fallback={<p class={ui.error} role="alert">Workspace ID is required.</p>}>
        <Show when={!workspace.isPending} fallback={<p class={ui.muted}>Loading workspace...</p>}>
          <Show
            when={workspace.data?.workspace}
            fallback={
              <div class={ui.emptyState}>
                <strong class="text-console-text">{workspaceErrorMessage(workspace.error)}</strong>
                <button class={ui.secondaryButton} type="button" onClick={retry}>Retry</button>
              </div>
            }
          >
            {(current) => (
              <div class={"grid grid-cols-[minmax(0,1fr)_310px] items-start gap-3.5 max-[960px]:grid-cols-1"}>
                <div class={"flex min-w-0 flex-col gap-3"}>
                  <section class={"border border-console-border bg-console-surface p-4"}>
                    <div class={"mb-3 flex items-center justify-between gap-3"}>
                      <h2 class={ui.h2}>Metadata</h2>
                    </div>
                    <pre class={"m-0 max-h-80 overflow-auto whitespace-pre-wrap break-words border border-console-border bg-console-bg-panel px-4 py-3 font-mono text-[12px] leading-normal text-console-text"}>
                      {formattedJSON(current().metadata)}
                    </pre>
                    <Show when={(current().tags?.length ?? 0) > 0}>
                      <div class={"mt-3 flex flex-wrap gap-1.5"}>
                        <For each={current().tags}>{(tag) => <code class={"border border-console-border bg-console-bg-panel px-1.5 py-0.5 text-[10.5px]"}>{tag}</code>}</For>
                      </div>
                    </Show>
                  </section>
                  <Show when={execs.isError}>
                    <div class={ui.emptyState}>
                      <strong class="text-console-text">Could not load workspace execs.</strong>
                      <button class={ui.secondaryButton} type="button" onClick={() => void execs.refetch()}>Retry</button>
                    </div>
                  </Show>
                  <Show when={!execs.isPending} fallback={<p class={ui.muted}>Loading exec history...</p>}>
                    <Show when={!execs.isError}>
                      <WorkspaceExecHistory workspace={current()} execs={execs.data?.execs ?? []} />
                    </Show>
                  </Show>
                </div>
                <WorkspaceDetails workspace={current()} />
              </div>
            )}
          </Show>
        </Show>
      </Show>
    </section>
  );
}
