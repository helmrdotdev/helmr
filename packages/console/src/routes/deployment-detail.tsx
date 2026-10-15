import { useParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, on, Show, type JSX } from "solid-js";
import { errorMessage } from "../lib/error";
import { getCurrentDeployment, getDeployment, getDeploymentEvents } from "../lib/deployments";
import { useScopeIDs } from "../lib/scope";
import { CopyLinkButton } from "../components/CopyLinkButton";
import { agentHref, listAgentDefinitions, listTemplateDefinitions, templateResources } from "../lib/definitions";
import { DataTable, LoadMore, PrimaryLink, type Noun } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { formatID } from "../ui/id";
import { Page } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { EmptyState, QueryState } from "../ui/QueryState";
import { LinesSkeleton, TableSkeleton } from "../ui/Skeleton";
import { RelativeTime } from "../ui/RelativeTime";
import { StatusBadge } from "../ui/StatusBadge";
import { TabPanel, Tabs, type TabItem } from "../ui/Tabs";

const EVENT_COLUMNS = ["Kind", "Message", "At"] as const;

type Tab = "agents" | "templates" | "events";

// A version names its Deployment by date and ID ("20260101.<uuid>"); the short
// form keeps the date and the ID's short form (`formatID`).
function shortVersion(version: string): string {
  const separator = version.indexOf(".");
  return separator > 0 ? `${version.slice(0, separator)}.${formatID(version.slice(separator + 1))}` : formatID(version);
}

const TABS: TabItem<Tab>[] = [
  { value: "agents", label: "Agents" },
  { value: "templates", label: "Templates" },
  { value: "events", label: "Events" },
];

const DEPLOYMENT_ERROR_MESSAGES = {
  forbidden: "You do not have permission to view this deployment.",
  deployment_not_materialized: "Definitions for this deployment are not materialized yet.",
};
const deploymentError = (error: unknown, fallback: string) => errorMessage(error, fallback, DEPLOYMENT_ERROR_MESSAGES);

type DefinitionPages<T> = {
  isPending: boolean;
  isError: boolean;
  error: unknown;
  data: { pages: { items: T[] }[] } | undefined;
  refetch: () => unknown;
  hasNextPage: boolean;
  isFetchingNextPage: boolean;
  isFetchNextPageError: boolean;
  fetchNextPage: () => unknown;
};

// The Agents or Templates this Deployment declares, with every field of their
// definitions as columns.
function DefinitionTable<T>(props: { noun: Noun; columns: string[]; query: DefinitionPages<T>; row: (item: T) => JSX.Element }) {
  const items = createMemo(() => props.query.data?.pages.flatMap(page => page.items) ?? []);
  return (
    <QueryState
      query={props.query}
      pending={<TableSkeleton label={`Loading ${props.noun.other}`} columns={props.columns} />}
      error={(error) => deploymentError(error, `Could not load ${props.noun.other}.`)}
      empty={{ when: (data) => data.pages.every(page => page.items.length === 0), title: `This deployment declares no ${props.noun.other}.` }}
    >
      {() => (
        <DataTable
          columns={props.columns}
          footer={<LoadMore count={items().length} noun={props.noun} hasMore={props.query.hasNextPage} loading={props.query.isFetchingNextPage} failed={props.query.isFetchNextPageError} onLoadMore={() => void props.query.fetchNextPage()} />}
        >
          <For each={items()}>{(item) => <tr>{props.row(item)}</tr>}</For>
        </DataTable>
      )}
    </QueryState>
  );
}

export function DeploymentDetail() {
  const params = useParams();
  const scope = useScopeIDs();
  const deploymentID = createMemo(() => params["deployment_id"]?.trim() ?? "");
  const projectID = () => scope().projectID;
  const environmentID = () => scope().environmentID;
  const enabled = () => !!deploymentID() && !!projectID() && !!environmentID();
  const resourceScope = () => ({ projectID: projectID(), environmentID: environmentID() });
  const [tab, setTab] = createSignal<Tab>("agents");
  const selection = createMemo(() => JSON.stringify([projectID(), environmentID(), deploymentID()]));
  createEffect(on(selection, () => {
    setTab("agents");
  }, { defer: true }));

  const deployment = createQuery(() => ({
    queryKey: ["deployments", "detail", deploymentID(), projectID(), environmentID()],
    queryFn: () => getDeployment(deploymentID(), resourceScope()),
    enabled: enabled(),
    retry: false,
  }));
  const current = createQuery(() => ({
    queryKey: ["deployments", "current", projectID(), environmentID()],
    queryFn: () => getCurrentDeployment(resourceScope()),
    enabled: enabled(),
    retry: false,
    refetchInterval: 15_000,
    refetchOnWindowFocus: true,
  }));
  const isCurrent = createMemo(() => !!current.data && current.data.id === deploymentID());
  const definitionOptions = () => ({ ...resourceScope(), deploymentID: deploymentID(), limit: 100 });
  const agents = createInfiniteQuery(() => ({
    queryKey: ["agents", deploymentID(), projectID(), environmentID()],
    queryFn: async ({ pageParam }) => {
      const page = await listAgentDefinitions({ ...definitionOptions(), cursor: pageParam || undefined });
      return { items: page.agents, next_cursor: page.next_cursor };
    },
    initialPageParam: "",
    getNextPageParam: page => page.next_cursor,
    enabled: enabled() && tab() === "agents",
    retry: false,
  }));
  const templates = createInfiniteQuery(() => ({
    queryKey: ["templates", deploymentID(), projectID(), environmentID()],
    queryFn: async ({ pageParam }) => {
      const page = await listTemplateDefinitions({ ...definitionOptions(), cursor: pageParam || undefined });
      return { items: page.templates, next_cursor: page.next_cursor };
    },
    initialPageParam: "",
    getNextPageParam: page => page.next_cursor,
    enabled: enabled() && tab() === "templates",
    retry: false,
  }));
  const events = createInfiniteQuery(() => ({
    queryKey: ["deployments", deploymentID(), "events", projectID(), environmentID()],
    queryFn: ({ pageParam }) => getDeploymentEvents(deploymentID(), resourceScope(), { cursor: pageParam || undefined }),
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor ?? undefined,
    enabled: enabled() && tab() === "events",
    retry: false,
  }));
  const eventItems = createMemo(() => events.data?.pages.flatMap((page) => page.events) ?? []);

  return (
    <Page>
      <PageHeader
        title={deployment.data?.version ?? "Deployment"}
        breadcrumb={[{ href: "/deployments", label: "Deployments" }]}
        current={deployment.data ? shortVersion(deployment.data.version) : "Deployment"}
        actions={<CopyLinkButton />}
        badge={<Show when={current.isSuccess}><StatusBadge resource="deployment" status={isCurrent() ? "current" : "not_current"} /></Show>}
        meta={
          <Show when={deployment.data}>
            {(record) => (
              <>
                <span>ID <IDText value={record().id} mode="copy" /></span>
                <span>Bundle <IDText value={record().bundle_digest} /></span>
                <span>Created <RelativeTime value={record().created_at} /></span>
              </>
            )}
          </Show>
        }
      />

      <Show when={enabled()} fallback={<EmptyState title="Select a project and environment." />}>
      <QueryState query={deployment} pending={<LinesSkeleton label="Loading deployment" />} error={(error) => deploymentError(error, "Could not load this deployment.")}>
        {() => (
          <>
            <Tabs id="deployment-sections" label="Deployment sections" items={TABS} selected={tab()} onSelect={setTab} />

            <TabPanel tabs="deployment-sections" value="agents" selected={tab()}>
              <DefinitionTable noun={{ one: "agent", other: "agents" }} columns={["Agent", "Template"]} query={agents} row={item => <>
                <td><PrimaryLink href={agentHref(item.agent)}>{item.agent}</PrimaryLink></td>
                <td><code>{item.template}</code></td>
              </>} />
            </TabPanel>
            <TabPanel tabs="deployment-sections" value="templates" selected={tab()}>
              <DefinitionTable noun={{ one: "template", other: "templates" }} columns={["Template", "Resources"]} query={templates} row={item => <>
                <td><code>{item.template}</code></td>
                <td>{templateResources(item)}</td>
              </>} />
            </TabPanel>
            <TabPanel tabs="deployment-sections" value="events" selected={tab()}>
              <QueryState
                query={events}
                pending={<TableSkeleton label="Loading events" columns={EVENT_COLUMNS} minWidth="min-w-180" />}
                error={(error) => deploymentError(error, "Could not load deployment events.")}
                empty={{ when: (data) => data.pages.every(page => page.events.length === 0), title: "No events recorded for this deployment." }}
              >
                {() => (
                  <DataTable
                    columns={EVENT_COLUMNS}
                    minWidth="min-w-180"
                    footer={<LoadMore count={eventItems().length} noun={{ one: "event", other: "events" }} hasMore={events.hasNextPage} loading={events.isFetchingNextPage} failed={events.isFetchNextPageError} onLoadMore={() => void events.fetchNextPage()} />}
                  >
                    <For each={eventItems()}>
                      {(event) => (
                        <tr>
                          <td><code>{event.kind}</code></td>
                          <td>{event.message}</td>
                          <td><RelativeTime value={event.at} /></td>
                        </tr>
                      )}
                    </For>
                  </DataTable>
                )}
              </QueryState>
            </TabPanel>
          </>
        )}
      </QueryState>
      </Show>
    </Page>
  );
}
