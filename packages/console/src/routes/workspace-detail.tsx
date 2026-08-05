import { useParams } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createMemo, For, Show } from "solid-js";
import { formatRelative } from "../features/runs/display";
import { ApiError } from "../lib/api";
import { useScope } from "../lib/scope";
import { getWorkspace, type Workspace } from "../lib/workspaces";
import { cx, ui } from "../ui/styles";

function workspaceErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.status === 404) {
    return "Workspace not found.";
  }
  if (error instanceof ApiError) {
    return error.message;
  }
  return "Could not load this Workspace.";
}

function statusBadge(status: Workspace["status"]): string {
  const tone = status === "available"
    ? "border-[#a8c3ad] bg-[#eef7f0] text-console-success"
    : status === "recovery-required"
      ? "border-[#e6aaa4] bg-[#fff1ef] text-console-danger"
      : "border-console-border bg-console-bg-panel text-console-muted";
  return cx(
    "inline-flex items-center whitespace-nowrap rounded-xs border px-2 py-0.5 font-mono text-[11px] font-medium leading-normal",
    tone,
  );
}

export function WorkspaceDetail() {
  const params = useParams();
  const scope = useScope();
  const workspaceID = createMemo(() => params["workspace_id"]?.trim() ?? "");
  const projectID = createMemo(() => scope.selectedProjectID());
  const environmentID = createMemo(() => scope.selectedEnvironmentID());
  const hasScope = createMemo(() => projectID() !== "" && environmentID() !== "");
  const workspace = createQuery(() => ({
    queryKey: ["workspace", workspaceID(), projectID(), environmentID()],
    queryFn: () => getWorkspace(workspaceID(), {
      projectID: projectID(),
      environmentID: environmentID(),
    }),
    enabled: workspaceID() !== "" && hasScope(),
    retry: false,
  }));

  return (
    <section class={ui.page}>
      <div class={ui.pageHeader}>
        <div>
          <div class={ui.pageTitle}>
            <h1 class={ui.h1}>Workspace</h1>
            <Show when={workspace.data}>
              {(current) => (
                <span class={statusBadge(current().status)}>
                  {current().status}
                </span>
              )}
            </Show>
          </div>
          <Show when={workspace.data}>
            {(current) => (
              <p class="mt-1.5 font-mono text-[12.5px] text-console-muted">
                {current().id}
              </p>
            )}
          </Show>
        </div>
      </div>

      <Show
        when={workspaceID() !== ""}
        fallback={<p class={ui.error} role="alert">Workspace ID is required.</p>}
      >
        <Show
          when={hasScope()}
          fallback={<p class={ui.emptyState}>Select a project and environment.</p>}
        >
          <Show
            when={!workspace.isPending}
            fallback={<p class={ui.muted}>Loading Workspace...</p>}
          >
            <Show
              when={workspace.data}
              fallback={
                <div class={ui.emptyState}>
                  <strong class="text-console-text">
                    {workspaceErrorMessage(workspace.error)}
                  </strong>
                  <button
                    class={ui.secondaryButton}
                    type="button"
                    onClick={() => void workspace.refetch()}
                  >
                    Retry
                  </button>
                </div>
              }
            >
              {(current) => (
                <div class="grid grid-cols-[minmax(0,1fr)_310px] items-start gap-3.5 max-[960px]:grid-cols-1">
                  <section class="border border-console-border bg-console-surface p-4">
                    <h2 class={cx(ui.h2, "mb-3")}>Secret placements</h2>
                    <Show
                      when={current().secrets.length > 0}
                      fallback={<p class={ui.emptyState}>No Secrets are attached.</p>}
                    >
                      <div class={ui.tableWrap}>
                        <table>
                          <thead>
                            <tr><th>Name</th><th>Placement</th></tr>
                          </thead>
                          <tbody>
                            <For each={current().secrets}>
                              {(secret) => (
                                <tr>
                                  <td><code>{secret.name}</code></td>
                                  <td><code>{secret.env ?? secret.file}</code></td>
                                </tr>
                              )}
                            </For>
                          </tbody>
                        </table>
                      </div>
                    </Show>
                  </section>

                  <aside class="sticky top-13.5 border border-console-border bg-console-surface px-4 py-3.5 max-[960px]:static">
                    <h3 class={cx(ui.h3, "mb-3.5")}>Workspace details</h3>
                    <dl class="m-0 grid gap-2.5 [&>div]:grid [&>div]:gap-0.75 [&_dt]:m-0 [&_dt]:font-mono [&_dt]:text-[10px] [&_dt]:font-medium [&_dt]:uppercase [&_dt]:tracking-[0.06em] [&_dt]:text-console-subtle [&_dd]:m-0 [&_dd]:[overflow-wrap:anywhere] [&_dd]:text-[12.5px] [&_dd]:text-console-text [&_dd_code]:font-mono [&_dd_code]:text-[11.5px]">
                      <div><dt>ID</dt><dd><code>{current().id}</code></dd></div>
                      <div><dt>Key</dt><dd><code>{current().key ?? "—"}</code></dd></div>
                      <div><dt>Sandbox ID</dt><dd><code>{current().sandbox_id}</code></dd></div>
                      <div><dt>Deployment</dt><dd><code>{current().deployment_id}</code></dd></div>
                      <div><dt>Last activity</dt><dd>{formatRelative(current().last_activity_at)}</dd></div>
                      <div><dt>Created</dt><dd>{formatRelative(current().created_at)}</dd></div>
                      <div><dt>Updated</dt><dd>{formatRelative(current().updated_at)}</dd></div>
                    </dl>
                  </aside>
                </div>
              )}
            </Show>
          </Show>
        </Show>
      </Show>
    </section>
  );
}
