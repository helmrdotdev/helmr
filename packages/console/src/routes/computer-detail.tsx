import { useParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createMemo, For, Show } from "solid-js";
import { deploymentHref } from "../features/deployments/navigation";
import { ApiError } from "../lib/api";
import { useScope } from "../lib/scope";
import { sessionConsolePath } from "../lib/sessions";
import {
  getComputer,
  listComputerMembers,
  type ComputerScope,
} from "../lib/computers";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { DetailItem, DetailList, Panel } from "../ui/Panel";
import { RelativeTime } from "../ui/RelativeTime";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";

function computerErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.status === 404) return "Computer not found.";
  if (error instanceof ApiError && error.code === "forbidden") return "You do not have permission to view this Computer.";
  if (error instanceof ApiError) return error.message;
  return "Could not load this Computer.";
}

function actionErrorMessage(error: unknown, fallback: string): string {
  if (error instanceof ApiError && error.code === "forbidden") return "You do not have permission to do this.";
  if (error instanceof ApiError) return error.message;
  return fallback;
}

function ComputerMembersPanel(props: { computerID: string; scope: ComputerScope }) {
  const members = createInfiniteQuery(() => ({
    queryKey: ["computer-members", props.computerID, props.scope.projectID, props.scope.environmentID],
    queryFn: ({ pageParam }) => listComputerMembers(props.computerID, props.scope, { cursor: pageParam || undefined }),
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor,
    refetchInterval: 5_000,
    retry: false,
  }));
  const items = createMemo(() => members.data?.pages.flatMap((page) => page.members) ?? []);
  return (
    <Panel title="Members">
      <Show when={members.isError}>
        <StatePanel error={actionErrorMessage(members.error, "Could not load Computer members.")} />
      </Show>
      <Show when={members.isPending}><StatePanel loading="Loading members..." /></Show>
      <Show when={members.data}>
        <Show when={items().length > 0} fallback={<StatePanel empty="No active members." />}>
          <DataTable columns={["Kind", "ID", "Status"]} minWidth="min-w-0">
            <For each={items()}>{(member) => (
              <tr>
                <td>{member.kind}</td>
                <td>
                  <Show when={member.kind === "session"} fallback={<IDText value={member.id} />}>
                    <IDText value={member.id} mode="link" href={sessionConsolePath(member.id, props.scope.projectID, props.scope.environmentID)} />
                  </Show>
                </td>
                <td>{member.state}</td>
              </tr>
            )}</For>
          </DataTable>
        </Show>
        <Show when={members.hasNextPage}>
          <button class={ui.secondaryButton} disabled={members.isFetchingNextPage} onClick={() => void members.fetchNextPage()}>
            {members.isFetchingNextPage ? "Loading..." : "Load more"}
          </button>
        </Show>
      </Show>
    </Panel>
  );
}

export function ComputerDetail() {
  const params = useParams();
  const scope = useScope();
  const computerID = createMemo(() => params["computer_id"]?.trim() ?? "");
  const projectID = createMemo(() => scope.selectedProjectID());
  const environmentID = createMemo(() => scope.selectedEnvironmentID());
  const hasScope = createMemo(() => projectID() !== "" && environmentID() !== "");
  const resourceScope = (): ComputerScope => ({ projectID: projectID(), environmentID: environmentID() });
  const computer = createQuery(() => ({
    queryKey: ["computer", computerID(), projectID(), environmentID()],
    queryFn: () => getComputer(computerID(), resourceScope()),
    enabled: computerID() !== "" && hasScope(),
    retry: false,
  }));

  return (
    <section class={ui.page}>
      <PageHeader
        title="Computer"
        back={{ href: "/computers", label: "Computers" }}
        badge={<Show when={computer.data}>{(current) => <StatusBadge resource="computer" status={current().status} />}</Show>}
        subtitle={<Show when={computer.data}>{(current) => <IDText value={current().id} mode="full" />}</Show>}
      />

      <Show when={computerID() !== ""} fallback={<StatePanel error="Computer ID is required." />}>
        <Show when={hasScope()} fallback={<StatePanel empty="Select a project and environment." />}>
          <Show when={!computer.isPending} fallback={<StatePanel loading="Loading Computer..." />}>
            <Show
              when={computer.data}
              fallback={
                <StatePanel empty={computerErrorMessage(computer.error)}>
                  <button class={ui.secondaryButton} type="button" onClick={() => void computer.refetch()}>
                    Retry
                  </button>
                </StatePanel>
              }
            >
              {(current) => (
                <div class="grid grid-cols-[minmax(0,1fr)_310px] items-start gap-3.5 max-[960px]:grid-cols-1">
                  <div class="grid gap-3.5">
                    <ComputerMembersPanel computerID={current().id} scope={resourceScope()} />
                    <Panel title="Secret placements">
                      <Show when={current().secrets.length > 0} fallback={<StatePanel empty="No Secrets are attached." />}>
                        <DataTable columns={["Secret", "Target", "Mode", "Origins"]} minWidth="min-w-0">
                          <For each={current().secrets}>
                            {(secret) => (
                              <tr>
                                <td><code>{secret.secretId}</code></td>
                                <td><code>{secret.env?.name ?? secret.file?.path}</code></td>
                                <td>{secret.env?.mode === "protected" ? "Protected env" : secret.env ? "Raw env" : "Raw file"}</td>
                                <td>{secret.env?.allowedOrigins?.join(", ") ?? "—"}</td>
                              </tr>
                            )}
                          </For>
                        </DataTable>
                      </Show>
                    </Panel>

                    <p class={ui.muted}>Use the CLI to run commands or delete this Computer.</p>
                  </div>

                  <DetailList title="Computer details">
                    <DetailItem label="Status"><StatusBadge resource="computer" status={current().status} /></DetailItem>
                    <DetailItem label="Residency">{current().residency}</DetailItem>
                    <Show when={current().error}>{(error) => <DetailItem label="Unavailable reason">{error().message}</DetailItem>}</Show>
                    <DetailItem label="Key">
                      <Show when={current().key} fallback={<span class="text-console-faint">—</span>}>{(key) => <code>{key()}</code>}</Show>
                    </DetailItem>
                    <DetailItem label="Computer definition"><code>{current().definition_key}</code></DetailItem>
                    <DetailItem label="Deployment">
                      <IDText value={current().deployment_id} mode="link" href={deploymentHref(current().deployment_id)} />
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
