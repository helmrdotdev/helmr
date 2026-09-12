import { useParams } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createMemo, For, Show } from "solid-js";
import { deploymentHref } from "../features/deployments/navigation";
import { ApiError } from "../lib/api";
import { useScope } from "../lib/scope";
import { getWorkspace } from "../lib/workspaces";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { DetailItem, DetailList, Panel } from "../ui/Panel";
import { RelativeTime } from "../ui/RelativeTime";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";

function workspaceErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.status === 404) {
    return "Workspace not found.";
  }
  if (error instanceof ApiError) {
    return error.message;
  }
  return "Could not load this Workspace.";
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
      <PageHeader
        title="Workspace"
        back={{ href: "/workspaces", label: "Workspaces" }}
        badge={<Show when={workspace.data}>{(current) => <StatusBadge resource="workspace" status={current().status} />}</Show>}
        subtitle={<Show when={workspace.data}>{(current) => <IDText value={current().id} full />}</Show>}
      />

      <Show when={workspaceID() !== ""} fallback={<StatePanel error="Workspace ID is required." />}>
        <Show when={hasScope()} fallback={<StatePanel empty="Select a project and environment." />}>
          <Show when={!workspace.isPending} fallback={<StatePanel loading="Loading Workspace..." />}>
            <Show
              when={workspace.data}
              fallback={
                <StatePanel empty={workspaceErrorMessage(workspace.error)}>
                  <button class={ui.secondaryButton} type="button" onClick={() => void workspace.refetch()}>
                    Retry
                  </button>
                </StatePanel>
              }
            >
              {(current) => (
                <div class="grid grid-cols-[minmax(0,1fr)_310px] items-start gap-3.5 max-[960px]:grid-cols-1">
                  <Panel title="Secret placements">
                    <Show when={current().secrets.length > 0} fallback={<StatePanel empty="No Secrets are attached." />}>
                      <DataTable columns={["Name", "Placement"]} minWidth="min-w-0">
                        <For each={current().secrets}>
                          {(secret) => (
                            <tr>
                              <td><code>{secret.name}</code></td>
                              <td><code>{secret.env ?? secret.file}</code></td>
                            </tr>
                          )}
                        </For>
                      </DataTable>
                    </Show>
                  </Panel>

                  <DetailList title="Workspace details">
                    <DetailItem label="ID"><IDText value={current().id} full /></DetailItem>
                    <DetailItem label="Key">
                      <Show when={current().key} fallback={<span class="text-console-faint">—</span>}>{(key) => <code>{key()}</code>}</Show>
                    </DetailItem>
                    <DetailItem label="Sandbox ID"><code>{current().sandbox_id}</code></DetailItem>
                    <DetailItem label="Deployment">
                      <IDText value={current().deployment_id} full href={deploymentHref(current().deployment_id)} />
                    </DetailItem>
                    <DetailItem label="Last activity"><RelativeTime value={current().last_activity_at} /></DetailItem>
                    <DetailItem label="Created"><RelativeTime value={current().created_at} /></DetailItem>
                    <DetailItem label="Updated"><RelativeTime value={current().updated_at} /></DetailItem>
                  </DetailList>
                </div>
              )}
            </Show>
          </Show>
        </Show>
      </Show>
    </section>
  );
}
