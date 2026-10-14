import { definitionHref } from "../lib/definitions";
import { CopyLinkButton } from "../components/CopyLinkButton";
import { useParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createMemo, For, Show } from "solid-js";
import { deploymentHref } from "../features/deployments/navigation";
import { ApiError } from "../lib/api";
import { errorMessage } from "../lib/error";
import { useScopeIDs } from "../lib/scope";
import { sessionConsolePath } from "../lib/sessions";
import {
  getComputer,
  listComputerSessions,
  listComputerCommands,
  type ComputerScope,
} from "../lib/computers";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { DetailLayout, KeyValue, KeyValueList, Panel } from "../ui/Panel";
import { RelativeTime } from "../ui/RelativeTime";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";

function computerErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.status === 404) return "Computer not found.";
  return errorMessage(error, "Could not load this Computer.", { forbidden: "You do not have permission to view this Computer." });
}

const actionErrorMessage = (error: unknown, fallback: string) =>
  errorMessage(error, fallback, { forbidden: "You do not have permission to do this." });

function ComputerUsagePanel(props: { computerID: string; scope: ComputerScope; kind: "sessions" | "commands" }) {
  const title = () => props.kind === "sessions" ? "Sessions" : "Commands";
  const usage = createInfiniteQuery(() => ({
    queryKey: ["computer", props.computerID, props.kind, props.scope.projectID, props.scope.environmentID],
    queryFn: async ({ pageParam }) => {
      const options = { cursor: pageParam || undefined };
      if (props.kind === "sessions") {
        const page = await listComputerSessions(props.computerID, props.scope, options);
        return { items: page.sessions, next_cursor: page.next_cursor };
      }
      const page = await listComputerCommands(props.computerID, props.scope, options);
      return { items: page.commands, next_cursor: page.next_cursor };
    },
    initialPageParam: "",
    getNextPageParam: page => page.next_cursor,
    refetchInterval: 5_000,
    retry: false,
  }));
  const items = createMemo(() => usage.data?.pages.flatMap(page => page.items) ?? []);
  return <Panel title={title()}>
    <p class={ui.muted}>Currently using this Computer, including work awaiting cleanup.</p>
    <Show when={usage.isError}><StatePanel error={actionErrorMessage(usage.error, `Could not load ${props.kind}.`)} /></Show>
    <Show when={usage.isPending}><StatePanel loading={`Loading ${props.kind}...`} /></Show>
    <Show when={usage.data}>
      <Show when={items().length > 0} fallback={<StatePanel empty={`No ${props.kind} are using this Computer.`} />}>
        <DataTable columns={["ID", "Usage"]} minWidth="min-w-0">
          <For each={items()}>{item => <tr>
            <td><Show when={props.kind === "sessions"} fallback={<IDText value={item.id} />}>
              <IDText value={item.id} mode="link" href={sessionConsolePath(item.id)} />
            </Show></td>
            <td>{item.usage_state}</td>
          </tr>}</For>
        </DataTable>
      </Show>
      <Show when={usage.hasNextPage}><button class={ui.secondaryButton} disabled={usage.isFetchingNextPage} onClick={() => void usage.fetchNextPage()}>Load more {props.kind}</button></Show>
    </Show>
  </Panel>;
}

export function ComputerDetail() {
  const params = useParams();
  const scope = useScopeIDs();
  const computerID = createMemo(() => params["computer_id"]?.trim() ?? "");
  const projectID = createMemo(() => scope().projectID);
  const environmentID = createMemo(() => scope().environmentID);
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
        breadcrumb={[{ href: "/computers", label: "Computers" }]}
        actions={<CopyLinkButton />}
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
                <DetailLayout
                  rail={
                  <KeyValueList title="Computer details">
                    <KeyValue label="Status"><StatusBadge resource="computer" status={current().status} /></KeyValue>
                    <KeyValue label="Residency">{current().residency}</KeyValue>
                    <Show when={current().error}>{(error) => <KeyValue label="Unavailable reason">{error().message}</KeyValue>}</Show>
                    <KeyValue label="Key">
                      <Show when={current().key} fallback={<span class="text-console-faint">—</span>}>{(key) => <code>{key()}</code>}</Show>
                    </KeyValue>
                    <KeyValue label="Created from"><a class="text-console-accent hover:text-console-accent-hover" href={definitionHref("templates", current().template, current().deployment_id)}>{current().template}</a></KeyValue>
                    <KeyValue label="Deployment">
                      <IDText value={current().deployment_id} mode="link" href={deploymentHref(current().deployment_id)} />
                    </KeyValue>
                    <KeyValue label="Last activity"><RelativeTime value={current().last_activity_at} /></KeyValue>
                    <KeyValue label="Created"><RelativeTime value={current().created_at} /></KeyValue>
                    <KeyValue label="Updated"><RelativeTime value={current().updated_at} /></KeyValue>
                  </KeyValueList>
                  }
                >
                    <ComputerUsagePanel computerID={current().id} scope={resourceScope()} kind="sessions" />
                    <ComputerUsagePanel computerID={current().id} scope={resourceScope()} kind="commands" />
                    <Panel title="Secret placements">
                      <Show when={current().secrets.length > 0} fallback={<StatePanel empty="No Secrets are attached." />}>
                        <DataTable columns={["Secret", "Target", "Mode", "Origins"]} minWidth="min-w-0">
                          <For each={current().secrets}>
                            {(secret) => (
                              <tr>
                                <td><code>{secret.secret}</code></td>
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
                </DetailLayout>
              )}
            </Show>
          </Show>
        </Show>
      </Show>

    </section>
  );
}
