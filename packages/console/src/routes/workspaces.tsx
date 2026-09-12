import { A } from "@solidjs/router";
import { createInfiniteQuery } from "@tanstack/solid-query";
import { createMemo, For, Show } from "solid-js";
import { formatRelative } from "../features/runs/display";
import { WorkspaceStatusBadge } from "../features/workspaces/display";
import { ApiError } from "../lib/api";
import { useScope } from "../lib/scope";
import { listWorkspaces, type WorkspaceListItem } from "../lib/workspaces";
import { ui } from "../ui/styles";

function workspacesErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.code === "forbidden") {
    return "You do not have permission to view Workspaces.";
  }
  if (error instanceof ApiError) return error.message;
  return "Could not load Workspaces.";
}

function workspaceHref(id: string): string {
  return `/workspaces/${encodeURIComponent(id)}`;
}

function WorkspaceRow(props: { workspace: WorkspaceListItem }) {
  return (
    <tr>
      <td>
        <A href={workspaceHref(props.workspace.id)} class="font-mono text-[11.5px] font-medium text-console-text hover:text-console-accent">
          {props.workspace.id.slice(0, 12)}
        </A>
      </td>
      <td>
        <Show when={props.workspace.key} fallback={<span class="text-console-faint">—</span>}>
          {(key) => <code>{key()}</code>}
        </Show>
      </td>
      <td><strong class="font-medium text-console-text">{props.workspace.sandbox_id}</strong></td>
      <td><WorkspaceStatusBadge status={props.workspace.status} /></td>
      <td><span class={ui.muted}>{formatRelative(props.workspace.updated_at)}</span></td>
    </tr>
  );
}

export function Workspaces() {
  const scope = useScope();
  const projectID = () => scope.selectedProjectID();
  const environmentID = () => scope.selectedEnvironmentID();
  const workspaces = createInfiniteQuery(() => ({
    queryKey: ["workspaces", "list", projectID(), environmentID()],
    queryFn: ({ pageParam }) => listWorkspaces(
      { projectID: projectID(), environmentID: environmentID() },
      { cursor: pageParam || undefined, limit: 100 },
    ),
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor,
    enabled: !!projectID() && !!environmentID(),
    retry: false,
  }));
  const items = createMemo(() => workspaces.data?.pages.flatMap((page) => page.workspaces) ?? []);

  return (
    <section class={ui.page}>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Workspaces</h1>
          <p class={ui.pageSubtitle}>
            Durable filesystems created from Sandbox definitions in the selected environment.
          </p>
        </div>
      </div>

      <Show when={workspaces.isError}>
        <p class={ui.error} role="alert">{workspacesErrorMessage(workspaces.error)}</p>
      </Show>
      <Show when={!workspaces.isPending} fallback={<p class={ui.muted}>Loading Workspaces...</p>}>
        <Show
          when={items().length > 0}
          fallback={
            <div class={ui.emptyState}>
              <strong>No Workspaces yet.</strong>
              <span>Runs and Sessions create Workspaces from their Sandbox.</span>
            </div>
          }
        >
          <div class={ui.tableWrap}>
            <table class="min-w-160">
              <thead>
                <tr>
                  <th>Workspace</th>
                  <th>Key</th>
                  <th>Sandbox</th>
                  <th>State</th>
                  <th>Updated</th>
                </tr>
              </thead>
              <tbody>
                <For each={items()}>
                  {(workspace) => <WorkspaceRow workspace={workspace} />}
                </For>
              </tbody>
            </table>
          </div>
          <Show when={workspaces.hasNextPage}>
            <div class={ui.actionRow}>
              <button
                type="button"
                class={ui.secondaryButton}
                disabled={workspaces.isFetchingNextPage}
                onClick={() => void workspaces.fetchNextPage()}
              >
                {workspaces.isFetchingNextPage ? "Loading..." : "Load more"}
              </button>
            </div>
          </Show>
        </Show>
      </Show>
    </section>
  );
}
