import { A, useParams } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createEffect, createMemo, For, Show } from "solid-js";
import { formatRelative } from "../features/runs/display";
import { ApiError } from "../lib/api";
import { useScope } from "../lib/scope";
import {
  getWorkspace,
  getWorkspaceExec,
  listWorkspaceExecOutput,
  type WorkspaceExecStreamChunk,
} from "../lib/workspaces";
import { cx, ui } from "../ui/styles";

function isNotFound(error: unknown): boolean {
  return error instanceof ApiError && error.status === 404;
}

function execErrorMessage(error: unknown): string {
  if (isNotFound(error)) return "Workspace exec not found.";
  if (error instanceof ApiError) return error.message;
  return "Could not load this workspace exec.";
}

function formattedJSON(value: unknown): string {
  return JSON.stringify(value ?? null, null, 2);
}

function commandText(command: unknown): string {
  if (Array.isArray(command) && command.every((part) => typeof part === "string")) {
    return command.join(" ");
  }
  return formattedJSON(command);
}

function decodeChunk(data: string): string {
  try {
    const binary = atob(data);
    const bytes = Uint8Array.from(binary, (character) => character.charCodeAt(0));
    return new TextDecoder().decode(bytes);
  } catch {
    return data;
  }
}

function streamText(chunks: WorkspaceExecStreamChunk[]): string {
  return [...chunks]
    .sort((left, right) => left.offset_start - right.offset_start)
    .map((chunk) => decodeChunk(chunk.data))
    .join("");
}

export function WorkspaceExecDetail() {
  const params = useParams();
  const scope = useScope();
  const workspaceID = createMemo(() => params["workspace_id"]?.trim() ?? "");
  const execID = createMemo(() => params["exec_id"]?.trim() ?? "");
  const hasIDs = createMemo(() => workspaceID() !== "" && execID() !== "");
  const workspace = createQuery(() => ({
    queryKey: ["workspace", workspaceID()],
    queryFn: () => getWorkspace(workspaceID()),
    enabled: workspaceID() !== "",
    retry: false,
  }));
  const actualScope = createMemo(() => {
    const current = workspace.data?.workspace;
    return current ? { projectID: current.project_id, environmentID: current.environment_id } : null;
  });
  const exec = createQuery(() => ({
    queryKey: ["workspace-exec", workspaceID(), execID(), actualScope()?.projectID, actualScope()?.environmentID],
    queryFn: () => getWorkspaceExec(workspaceID(), execID(), actualScope()!),
    enabled: hasIDs() && !!actualScope(),
    retry: false,
  }));
  const stdout = createQuery(() => ({
    queryKey: ["workspace-exec-output", workspaceID(), execID(), "stdout", actualScope()?.projectID, actualScope()?.environmentID],
    queryFn: () => listWorkspaceExecOutput(workspaceID(), execID(), "stdout", actualScope()!),
    enabled: !!exec.data && !!actualScope(),
    retry: false,
  }));
  const stderr = createQuery(() => ({
    queryKey: ["workspace-exec-output", workspaceID(), execID(), "stderr", actualScope()?.projectID, actualScope()?.environmentID],
    queryFn: () => listWorkspaceExecOutput(workspaceID(), execID(), "stderr", actualScope()!),
    enabled: !!exec.data && !!actualScope(),
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

  const loadError = createMemo(() => workspace.error ?? exec.error);
  const loading = createMemo(() => workspace.isPending || (!!workspace.data && exec.isPending));
  const retry = () => {
    if (workspace.isError) {
      void workspace.refetch();
      return;
    }
    void Promise.all([exec.refetch(), stdout.refetch(), stderr.refetch()]);
  };

  return (
    <section class={ui.page}>
      <div class={ui.pageHeader}>
        <div>
          <A href={`/workspaces/${encodeURIComponent(workspaceID())}`} class={ui.backLink}>Workspace</A>
          <div class={ui.pageTitle}>
            <h1 class={ui.h1}>Workspace exec</h1>
            <Show when={exec.data?.exec}>
              {(current) => <span class={"inline-flex rounded-xs border border-console-border bg-console-bg-panel px-2 py-0.5 font-mono text-[11px] font-medium text-console-muted"}>{current().state}</span>}
            </Show>
          </div>
          <Show when={exec.data?.exec}>
            {(current) => <p class={"mt-1.5 font-mono text-[12.5px] text-console-muted"}>{current().id}</p>}
          </Show>
        </div>
      </div>

      <Show when={hasIDs()} fallback={<p class={ui.error} role="alert">Workspace and exec IDs are required.</p>}>
        <Show when={!loading()} fallback={<p class={ui.muted}>Loading workspace exec...</p>}>
          <Show
            when={exec.data?.exec}
            fallback={
              <div class={ui.emptyState}>
                <strong class="text-console-text">{execErrorMessage(loadError())}</strong>
                <button class={ui.secondaryButton} type="button" onClick={retry}>Retry</button>
              </div>
            }
          >
            {(current) => (
              <div class={"grid grid-cols-[minmax(0,1fr)_310px] items-start gap-3.5 max-[960px]:grid-cols-1"}>
                <div class={"flex min-w-0 flex-col gap-3"}>
                  <section class={"border border-console-border bg-console-surface p-4"}>
                    <h2 class={cx(ui.h2, "mb-3")}>Command</h2>
                    <pre class={"m-0 overflow-auto whitespace-pre-wrap break-words border border-console-border bg-console-bg-panel px-4 py-3 font-mono text-[12px] leading-normal text-console-text"}>{commandText(current().command)}</pre>
                  </section>
                  <For each={[
                    { name: "stdout", query: stdout },
                    { name: "stderr", query: stderr },
                  ]}>
                    {(stream) => (
                      <section class={"border border-console-border bg-console-surface p-4"}>
                        <div class={"mb-3 flex items-center justify-between gap-3"}>
                          <h2 class={ui.h2}>{stream.name}</h2>
                          <Show when={stream.query.isError}>
                            <button class={ui.ghostButton} type="button" onClick={() => void stream.query.refetch()}>Retry</button>
                          </Show>
                        </div>
                        <Show when={!stream.query.isPending} fallback={<p class={ui.muted}>Loading {stream.name}...</p>}>
                          <Show
                            when={!stream.query.isError}
                            fallback={<p class={ui.error} role="alert">Could not load {stream.name}.</p>}
                          >
                            <pre class={"m-0 min-h-28 max-h-130 overflow-auto whitespace-pre-wrap break-words border border-console-border bg-console-bg-panel px-4 py-3 font-mono text-[12px] leading-normal text-console-text empty:before:text-console-faint empty:before:italic empty:before:content-['(no_output)']"}>
                              {streamText(stream.query.data?.chunks ?? [])}
                            </pre>
                          </Show>
                        </Show>
                      </section>
                    )}
                  </For>
                </div>
                <aside class={"sticky top-13.5 max-[960px]:static"}>
                  <section class={"border border-console-border bg-console-surface px-4 py-3.5"}>
                    <h3 class={cx(ui.h3, "mb-3.5")}>Exec details</h3>
                    <dl class={"m-0 grid gap-2.5 [&>div]:grid [&>div]:gap-0.75 [&_dt]:m-0 [&_dt]:font-mono [&_dt]:text-[10px] [&_dt]:font-medium [&_dt]:uppercase [&_dt]:tracking-[0.06em] [&_dt]:text-console-subtle [&_dd]:m-0 [&_dd]:[overflow-wrap:anywhere] [&_dd]:text-[12.5px] [&_dd]:text-console-text [&_dd_code]:font-mono [&_dd_code]:text-[11.5px]"}>
                      <div><dt>ID</dt><dd><code>{current().id}</code></dd></div>
                      <div><dt>Workspace</dt><dd><code>{current().workspace_id}</code></dd></div>
                      <div><dt>Working directory</dt><dd><code>{current().cwd || "—"}</code></dd></div>
                      <div><dt>Mode</dt><dd>{current().filesystem_mode}</dd></div>
                      <div><dt>Detached</dt><dd>{current().detached ? "yes" : "no"}</dd></div>
                      <div><dt>Exit code</dt><dd>{current().exit_code ?? "—"}</dd></div>
                      <div><dt>Signal</dt><dd>{current().signal || "—"}</dd></div>
                      <div><dt>Created</dt><dd>{formatRelative(current().created_at)}</dd></div>
                      <div><dt>Started</dt><dd>{formatRelative(current().started_at)}</dd></div>
                      <div><dt>Exited</dt><dd>{formatRelative(current().exited_at)}</dd></div>
                    </dl>
                    <Show when={current().error !== undefined}>
                      <div class={"mt-3"}>
                        <h3 class={cx(ui.h3, "mb-1.5")}>Error</h3>
                        <pre class={"m-0 max-h-48 overflow-auto whitespace-pre-wrap break-words border border-console-border bg-console-bg-panel px-3 py-2 font-mono text-[11.5px] text-console-danger"}>{formattedJSON(current().error)}</pre>
                      </div>
                    </Show>
                  </section>
                </aside>
              </div>
            )}
          </Show>
        </Show>
      </Show>
    </section>
  );
}
