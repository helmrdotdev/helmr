import { useParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, on, Show } from "solid-js";
import { errorMessage } from "../lib/error";
import { getCurrentDeployment, getDeployment, getDeploymentEvents } from "../lib/deployments";
import { useScopeIDs } from "../lib/scope";
import { CopyLinkButton } from "../components/CopyLinkButton";
import { definitionHref, definitionName, listAgentDefinitions, listTemplateDefinitions, type DefinitionListItem } from "../lib/definitions";
import { DataTable, LoadMore, PrimaryLink } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { Page } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { EmptyState, QueryState } from "../ui/QueryState";
import { RelativeTime } from "../ui/RelativeTime";
import { StatusBadge } from "../ui/StatusBadge";
import { cx, ui } from "../ui/styles";
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

type DefinitionPages = {
  isPending: boolean;
  isError: boolean;
  error: unknown;
  data: { pages: { items: DefinitionListItem[] }[] } | undefined;
  refetch: () => unknown;
  hasNextPage: boolean;
  isFetchingNextPage: boolean;
  isFetchNextPageError: boolean;
  fetchNextPage: () => unknown;
};

function DefinitionTable(props: { label: "Agents" | "Templates"; href: (id: string) => string; query: DefinitionPages }) {
  const noun = () => props.label.toLowerCase();
  const items = createMemo(() => props.query.data?.pages.flatMap(page => page.items) ?? []);
  return (
    <QueryState
      query={props.query}
      pending={`Loading ${noun()}...`}
      error={(error) => deploymentError(error, `Could not load ${noun()}.`)}
      empty={{ when: (data) => data.pages.every(page => page.items.length === 0), title: `This Deployment declares no ${noun()}.` }}
    >
      {() => (
        <DataTable
          columns={[props.label === "Agents" ? "Agent" : "Template"]}
          footer={<LoadMore count={items().length} noun={props.label} hasMore={props.query.hasNextPage} loading={props.query.isFetchingNextPage} failed={props.query.isFetchNextPageError} onLoadMore={() => void props.query.fetchNextPage()} />}
        >
          <For each={items()}>
            {(item) => (
              <tr>
                <td><PrimaryLink href={props.href(definitionName(item))}>{definitionName(item)}</PrimaryLink></td>
              </tr>
            )}
          </For>
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
      return { items: page.agents as DefinitionListItem[], next_cursor: page.next_cursor };
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
      return { items: page.templates as DefinitionListItem[], next_cursor: page.next_cursor };
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
              <p class={cx(ui.muted, "mb-3")}>Definitions in this Deployment. New independent Sessions use only the current Deployment; existing Sessions and owned children keep their pinned definitions. Inspecting another Deployment does not promote it.</p>
              <DefinitionTable label="Agents" href={id => definitionHref("agents", id, deploymentID())} query={agents} />
            </TabPanel>
            <TabPanel tabs="deployment-sections" value="templates" selected={tab()}>
              <p class={cx(ui.muted, "mb-3")}>Computer creation settings in this Deployment. New Computers use the current Deployment. Existing Computers keep their original Template definition.</p>
              <DefinitionTable label="Templates" href={id => definitionHref("templates", id, deploymentID())} query={templates} />
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
                    footer={<LoadMore count={eventItems().length} noun="events" hasMore={events.hasNextPage} loading={events.isFetchingNextPage} failed={events.isFetchNextPageError} onLoadMore={() => void events.fetchNextPage()} />}
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
