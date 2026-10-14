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
import { DataTable, EmptyCell, LoadMore } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { Page } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { DetailLayout, KeyValue, KeyValueList, Panel } from "../ui/Panel";
import { EmptyState, QueryState } from "../ui/QueryState";
import { RelativeTime } from "../ui/RelativeTime";
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
  const noun = () => props.kind === "sessions" ? { one: "Session", other: "Sessions" } : { one: "command", other: "commands" };
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
    <QueryState
      query={usage}
      pending={`Loading ${noun().other}...`}
      error={(error) => actionErrorMessage(error, `Could not load ${noun().other}.`)}
      empty={{ when: (data) => data.pages.every(page => page.items.length === 0), title: `No ${noun().other} are using this Computer.` }}
    >
      {() => (
        <DataTable
          columns={[props.kind === "sessions" ? "Session" : "Command", "Usage"]}
          minWidth="min-w-0"
          footer={<LoadMore count={items().length} noun={noun()} hasMore={usage.hasNextPage} loading={usage.isFetchingNextPage} failed={usage.isFetchNextPageError} onLoadMore={() => void usage.fetchNextPage()} />}
        >
          <For each={items()}>{item => <tr>
            <td><Show when={props.kind === "sessions"} fallback={<IDText value={item.id} />}>
              <IDText value={item.id} mode="link" href={sessionConsolePath(item.id)} />
            </Show></td>
            <td><StatusBadge resource="computer_usage" status={item.usage_state} /></td>
          </tr>}</For>
        </DataTable>
      )}
    </QueryState>
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
    <Page>
      <PageHeader
        title="Computer"
        breadcrumb={[{ href: "/computers", label: "Computers" }]}
        actions={<CopyLinkButton />}
        badge={<Show when={computer.data}>{(current) => <StatusBadge resource="computer" status={current().status} />}</Show>}
        subtitle={<Show when={computer.data}>{(current) => <IDText value={current().id} mode="full" />}</Show>}
      />

      <Show when={hasScope()} fallback={<EmptyState title="Select a project and environment." />}>
        <QueryState query={computer} pending="Loading Computer..." error={computerErrorMessage}>
          {(current) => (
            <DetailLayout
              rail={
                <KeyValueList title="Computer details">
                  <KeyValue label="Status"><StatusBadge resource="computer" status={current().status} /></KeyValue>
                  <KeyValue label="Residency"><StatusBadge resource="computer_residency" status={current().residency} /></KeyValue>
                  <Show when={current().error}>{(error) => <KeyValue label="Unavailable reason">{error().message}</KeyValue>}</Show>
                  <KeyValue label="Key">
                    <Show when={current().key}>{(key) => <code>{key()}</code>}</Show>
                  </KeyValue>
                  <KeyValue label="Created from"><code>{current().template}</code></KeyValue>
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
                <Show when={current().secrets.length > 0} fallback={<EmptyState title="No Secrets are attached." />}>
                  <DataTable columns={["Secret", "Target", "Mode", "Origins"]} minWidth="min-w-0">
                    <For each={current().secrets}>
                      {(secret) => (
                        <tr>
                          <td><code>{secret.secret}</code></td>
                          <td><code>{secret.env?.name ?? secret.file?.path}</code></td>
                          <td>{secret.env?.mode === "protected" ? "Protected env" : secret.env ? "Raw env" : "Raw file"}</td>
                          <td><Show when={secret.env?.allowedOrigins?.length} fallback={<EmptyCell />}>{secret.env?.allowedOrigins?.join(", ")}</Show></td>
                        </tr>
                      )}
                    </For>
                  </DataTable>
                </Show>
              </Panel>
              <p class={ui.muted}>Use the CLI to run commands or delete this Computer.</p>
            </DetailLayout>
          )}
        </QueryState>
      </Show>
    </Page>
  );
}
