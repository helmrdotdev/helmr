import { createInfiniteQuery, createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { CreateWorkspaceModal } from "../features/workspaces/CreateWorkspaceModal";
import { workspaceHref } from "../features/workspaces/navigation";
import { ApiError } from "../lib/api";
import { getMe, hasPermission } from "../lib/auth";
import { useScope } from "../lib/scope";
import { listWorkspaces, type WorkspaceListItem } from "../lib/workspaces";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { RelativeTime } from "../ui/RelativeTime";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";

function workspacesErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.code === "forbidden") {
    return "You do not have permission to view Workspaces.";
  }
  if (error instanceof ApiError) return error.message;
  return "Could not load Workspaces.";
}

function WorkspaceRow(props: { workspace: WorkspaceListItem }) {
  return (
    <tr>
      <td><IDText value={props.workspace.id} href={workspaceHref(props.workspace.id)} /></td>
      <td>
        <Show when={props.workspace.key} fallback={<span class="text-console-faint">—</span>}>
          {(key) => <code>{key()}</code>}
        </Show>
      </td>
      <td><span class={ui.muted}>{props.workspace.sandbox_id}</span></td>
      <td><StatusBadge resource="workspace" status={props.workspace.status} /></td>
      <td><RelativeTime value={props.workspace.last_activity_at} /></td>
      <td><RelativeTime value={props.workspace.created_at} /></td>
    </tr>
  );
}

export function Workspaces() {
  const scope = useScope();
  const queryClient = useQueryClient();
  const projectID = () => scope.selectedProjectID();
  const environmentID = () => scope.selectedEnvironmentID();
  const me = createQuery(() => ({ queryKey: ["me"], queryFn: getMe, retry: false, staleTime: 60_000 }));
  const [creating, setCreating] = createSignal(false);
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
      <PageHeader
        title="Workspaces"
        subtitle="Durable filesystems created from Sandbox definitions in the selected environment."
        actions={
          <Show when={hasPermission(me.data, "workspaces.create")}>
            <button type="button" class={ui.button} disabled={!projectID() || !environmentID()} onClick={() => setCreating(true)}>
              Create Workspace
            </button>
          </Show>
        }
      />

      <Show when={workspaces.isError}>
        <StatePanel error={workspacesErrorMessage(workspaces.error)} />
      </Show>
      <Show when={!workspaces.isPending} fallback={<StatePanel loading="Loading Workspaces..." />}>
        <Show
          when={items().length > 0}
          fallback={<StatePanel empty="No Workspaces yet." hint="Runs and Sessions create Workspaces from their Sandbox, or create one here." />}
        >
          <DataTable columns={["Workspace", "Key", "Sandbox", "State", "Last activity", "Created"]} minWidth="min-w-200">
            <For each={items()}>
              {(workspace) => <WorkspaceRow workspace={workspace} />}
            </For>
          </DataTable>
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

      <Show when={creating()}>
        <CreateWorkspaceModal
          projectID={projectID()}
          environmentID={environmentID()}
          onClose={() => setCreating(false)}
          onCreated={async () => {
            await queryClient.invalidateQueries({ queryKey: ["workspaces"] });
          }}
        />
      </Show>
    </section>
  );
}
