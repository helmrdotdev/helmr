import { useParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, on, Show, type JSX } from "solid-js";
import { loadError } from "../lib/error";
import { getCurrentDeployment, getDeployment, getDeploymentEvents } from "../lib/deployments";
import { useScope, useScopeIDs } from "../lib/scope";
import { useCopyLink } from "../components/copyLink";
import { canPromoteDeployment, PromoteDeploymentModal } from "../features/deployments/PromoteDeploymentModal";
import { hasPermission } from "../lib/auth";
import { useMe } from "../lib/me";
import { agentHref, listAgentDefinitions, listTemplateDefinitions, templateResources } from "../lib/definitions";
import { DataTable, LoadMore, PrimaryLink, type Noun } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { shortVersion } from "../features/deployments/navigation";
import { Page } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { isNotFound, NotFoundState, QueryState, NoScopeState } from "../ui/QueryState";
import { LinesSkeleton, TableSkeleton } from "../ui/Skeleton";
import { RelativeTime } from "../ui/RelativeTime";
import { StatusBadge } from "../ui/StatusBadge";
import { TabPanel, Tabs, type TabItem } from "../ui/Tabs";

const EVENT_COLUMNS = ["Message", "At"] as const;

type Tab = "agents" | "templates" | "events";

const TABS: TabItem<Tab>[] = [
  { value: "agents", label: "Agents" },
  { value: "templates", label: "Templates" },
  { value: "events", label: "Events" },
];

const DEPLOYMENT_ERROR_MESSAGES = {
  forbidden: "You do not have permission to view this deployment.",
  deployment_not_materialized: "Definitions for this deployment are not materialized yet.",
};
const deploymentError = (error: unknown, fallback: string) => loadError(error, fallback, DEPLOYMENT_ERROR_MESSAGES);

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
  const scopeState = useScope();
  const environment = () => scopeState.selectedEnvironment()?.name;
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
  const notFound = () => !deployment.data && isNotFound(deployment.error);
  const isCurrent = createMemo(() => !!current.data && current.data.id === deploymentID());
  const me = useMe();
  const copyLink = useCopyLink();
  const [promoting, setPromoting] = createSignal(false);
  const canPromote = () => canPromoteDeployment({ permitted: hasPermission(me.data, "deployments.promote", resourceScope()), currentLoaded: current.isSuccess, isCurrent: isCurrent() });
  const menu = () => [...(deployment.data && canPromote() ? [{ label: "Promote", onSelect: () => setPromoting(true) }] : []), copyLink.item()];
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
        title={notFound() ? "Deployment not found" : deployment.data ? shortVersion(deployment.data.version) : "Deployment"}
        breadcrumb={[{ href: "/deployments", label: "Deployments" }]}
        current={deployment.data ? shortVersion(deployment.data.version) : "Deployment"}
        menu={notFound() ? undefined : menu()}
        menuLabel="Deployment actions"
        badge={<Show when={current.isSuccess && !notFound()}><StatusBadge resource="deployment" status={isCurrent() ? "current" : "not_current"} /></Show>}
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

      <copyLink.LiveRegion />
      <Show when={promoting() && deployment.data}>{record => <PromoteDeploymentModal deployment={record()} projectID={projectID()} environmentID={environmentID()} environment={scopeState.selectedEnvironment()} onClose={() => setPromoting(false)} />}</Show>
      <Show when={enabled()} fallback={<NoScopeState />}>
      <QueryState query={deployment} pending={<LinesSkeleton label="Loading deployment" />} error={(error) => deploymentError(error, "Could not load this deployment.")}
        notFound={<NotFoundState noun="deployment" id={deploymentID()} environment={environment()} back={{ href: "/deployments", label: "Go to deployments" }} />}
      >
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
                          <td>{event.message} <span class="ml-1.5 font-mono text-chrome text-console-faint">{event.kind}</span></td>
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
