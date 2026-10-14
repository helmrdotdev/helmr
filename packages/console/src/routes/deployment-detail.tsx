import { useParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, on, Show, type JSX } from "solid-js";
import { errorMessage } from "../lib/error";
import { getCurrentDeployment, getDeployment, getDeploymentEvents } from "../lib/deployments";
import { useScopeIDs } from "../lib/scope";
import { CopyLinkButton } from "../components/CopyLinkButton";
import { listAgentDefinitions, listTemplateDefinitions, templateResources } from "../lib/definitions";
import { DataTable, LoadMore, type Noun } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { Page } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { EmptyState, QueryState } from "../ui/QueryState";
import { RelativeTime } from "../ui/RelativeTime";
import { StatusBadge } from "../ui/StatusBadge";
import { TabPanel, Tabs, type TabItem } from "../ui/Tabs";

type Tab = "agents" | "templates" | "events";

const TABS: TabItem<Tab>[] = [
  { value: "agents", label: "Agents" },
  { value: "templates", label: "Templates" },
  { value: "events", label: "Events" },
];

const DEPLOYMENT_ERROR_MESSAGES = {
  forbidden: "You do not have permission to view this Deployment.",
  deployment_not_materialized: "Definitions for this Deployment are not materialized yet.",
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
      pending={`Loading ${props.noun.other}...`}
      error={(error) => deploymentError(error, `Could not load ${props.noun.other}.`)}
      empty={{ when: (data) => data.pages.every(page => page.items.length === 0), title: `This Deployment declares no ${props.noun.other}.` }}
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
        actions={<CopyLinkButton />}
        badge={<Show when={current.isSuccess}><StatusBadge resource="deployment" status={isCurrent() ? "current" : "not_current"} /></Show>}
        subtitle={<Show when={deployment.data}>{(record) => <IDText value={record().id} mode="full" />}</Show>}
        meta={
          <Show when={deployment.data}>
            {(record) => (
              <>
                <span>Bundle <IDText value={record().bundle_digest} /></span>
                <span>Created <RelativeTime value={record().created_at} /></span>
              </>
            )}
          </Show>
        }
      />

      <Show when={enabled()} fallback={<EmptyState title="Select a project and environment." />}>
      <QueryState query={deployment} pending="Loading Deployment..." error={(error) => deploymentError(error, "Could not load this Deployment.")}>
        {() => (
          <>
            <Tabs id="deployment-sections" label="Deployment sections" items={TABS} selected={tab()} onSelect={setTab} />

            <TabPanel tabs="deployment-sections" value="agents" selected={tab()}>
              <DefinitionTable noun={{ one: "Agent", other: "Agents" }} columns={["Agent", "Template"]} query={agents} row={item => <>
                <td><code>{item.agent}</code></td>
                <td><code>{item.template}</code></td>
              </>} />
            </TabPanel>
            <TabPanel tabs="deployment-sections" value="templates" selected={tab()}>
              <DefinitionTable noun={{ one: "Template", other: "Templates" }} columns={["Template", "Resources"]} query={templates} row={item => <>
                <td><code>{item.template}</code></td>
                <td>{templateResources(item)}</td>
              </>} />
            </TabPanel>
            <TabPanel tabs="deployment-sections" value="events" selected={tab()}>
              <QueryState
                query={events}
                pending="Loading events..."
                error={(error) => deploymentError(error, "Could not load Deployment events.")}
                empty={{ when: (data) => data.pages.every(page => page.events.length === 0), title: "No events recorded for this Deployment." }}
              >
                {() => (
                  <DataTable
                    columns={["Kind", "Message", "At"]}
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
