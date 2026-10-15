import { CopyLinkButton } from "../components/CopyLinkButton";
import { useParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, on, Show } from "solid-js";
import { ComputerStatus } from "../features/computers/ComputerStatus";
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
import { DetailLayout, KeyValue, KeyValueList } from "../ui/Panel";
import { TabPanel, Tabs, type TabItem } from "../ui/Tabs";
import { EmptyState, QueryState } from "../ui/QueryState";
import { PageSkeleton, TableSkeleton } from "../ui/Skeleton";
import { RelativeTime } from "../ui/RelativeTime";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";

function computerErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.status === 404) return "Computer not found.";
  return errorMessage(error, "Could not load this computer.", { forbidden: "You do not have permission to view this computer." });
}

const actionErrorMessage = (error: unknown, fallback: string) =>
  errorMessage(error, fallback, { forbidden: "You do not have permission to do this." });

// The sessions or commands using this Computer, paged, as the table of its tab.
function ComputerUsage(props: { computerID: string; scope: ComputerScope; kind: "sessions" | "commands" }) {
  const noun = () => props.kind === "sessions" ? { one: "session", other: "sessions" } : { one: "command", other: "commands" };
  const columns = () => [props.kind === "sessions" ? "Session" : "Command", "Usage"];
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
  return (
    <QueryState
      query={usage}
      pending={<TableSkeleton label={`Loading ${noun().other}`} columns={columns()} minWidth="min-w-0" />}
      error={(error) => actionErrorMessage(error, `Could not load ${noun().other}.`)}
      empty={{ when: (data) => data.pages.every(page => page.items.length === 0), title: `No ${noun().other} are using this computer.` }}
    >
      {() => (
        <DataTable
          columns={columns()}
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
  );
}

type Tab = "sessions" | "commands" | "secrets";

// The Computer's tables, one tab each; the secret placements are on the
// Computer record, so their tab shows the count.
const tabs = (secrets: number): TabItem<Tab>[] => [
  { value: "sessions", label: "Sessions" },
  { value: "commands", label: "Commands" },
  { value: "secrets", label: "Secret placements", count: secrets },
];

export function ComputerDetail() {
  const params = useParams();
  const scope = useScopeIDs();
  const computerID = createMemo(() => params["computer_id"]?.trim() ?? "");
  const projectID = createMemo(() => scope().projectID);
  const environmentID = createMemo(() => scope().environmentID);
  const hasScope = createMemo(() => projectID() !== "" && environmentID() !== "");
  const resourceScope = (): ComputerScope => ({ projectID: projectID(), environmentID: environmentID() });
  const [tab, setTab] = createSignal<Tab>("sessions");
  createEffect(on(computerID, () => setTab("sessions"), { defer: true }));
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
        badge={<Show when={computer.data}>{(current) => <ComputerStatus computer={current()} />}</Show>}
        subtitle={<Show when={computer.data}>{(current) => <IDText value={current().id} mode="full" />}</Show>}
      />

      <Show when={hasScope()} fallback={<EmptyState title="Select a project and environment." />}>
        <QueryState query={computer} pending={<PageSkeleton label="Loading computer" sections={["Sessions"]} rail="Computer details" />} error={computerErrorMessage}>
          {(current) => (
            <DetailLayout
              rail={
                <KeyValueList title="Computer details">
                  <KeyValue label="Status"><ComputerStatus computer={current()} /></KeyValue>
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
              <div>
                <Tabs id="computer-sections" label="Computer sections" items={tabs(current().secrets.length)} selected={tab()} onSelect={setTab} />
                <TabPanel tabs="computer-sections" value="sessions" selected={tab()}>
                  <ComputerUsage computerID={current().id} scope={resourceScope()} kind="sessions" />
                </TabPanel>
                <TabPanel tabs="computer-sections" value="commands" selected={tab()}>
                  <ComputerUsage computerID={current().id} scope={resourceScope()} kind="commands" />
                </TabPanel>
                <TabPanel tabs="computer-sections" value="secrets" selected={tab()}>
                  <Show when={current().secrets.length > 0} fallback={<EmptyState title="No secrets are attached." />}>
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
                </TabPanel>
              </div>
              <p class={ui.muted}>Use the CLI to run commands or delete this computer.</p>
            </DetailLayout>
          )}
        </QueryState>
      </Show>
    </Page>
  );
}
