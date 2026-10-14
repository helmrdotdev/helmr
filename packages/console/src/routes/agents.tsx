import { useLocation, useParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show, type JSX } from "solid-js";
import { CopyLinkButton } from "../components/CopyLinkButton";
import { SectionLayout } from "../components/SectionLayout";
import { deploymentHref } from "../features/deployments/navigation";
import { AgentSlackConnection } from "../features/slack/AgentSlackConnection";
import { ApiError } from "../lib/api";
import { getCurrentDeployment } from "../lib/deployments";
import { agentHref, getAgentDefinition, getAgentStatus, listAgentDefinitions, listRetainedAgents, type AgentTab, type RetainedAgent } from "../lib/definitions";
import { errorMessage } from "../lib/error";
import { useScopeIDs } from "../lib/scope";
import { DataTable, EmptyCell, LoadMore, PrimaryLink } from "../ui/DataTable";
import { Notice } from "../ui/Notice";
import { IDText } from "../ui/IDText";
import { SearchInput, Segmented } from "../ui/Field";
import { Page, Toolbar } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { EmptyState, QueryState } from "../ui/QueryState";
import { StatusBadge } from "../ui/StatusBadge";
import { selectedLinkTab } from "../ui/linkTab";
import { LinkTabs, TabPanel, type LinkTabItem } from "../ui/Tabs";
import { SessionList } from "./sessions";
import { TriggerList } from "./triggers";

const SHOW_OPTIONS = [
  { value: "current", label: "Current Deployment" },
  { value: "all", label: "All Agents" },
] as const;

const failure = (error: unknown) => errorMessage(error, "Could not load Agents.");

type Section = "agents" | "sessions" | "triggers";
const SECTION_TABS: LinkTabItem<Section>[] = [
  { value: "agents", label: "Agents", href: "/agents" },
  { value: "sessions", label: "Sessions", href: "/sessions" },
  { value: "triggers", label: "Triggers", href: "/triggers" },
];

// The Agents section: Agents, their Sessions and triggers, one page each.
export function AgentsSection(props: { children?: JSX.Element }) {
  return (
    <SectionLayout id="agents-section" title="Agents" tabs={SECTION_TABS}>
      {props.children}
    </SectionLayout>
  );
}

type AgentRow = RetainedAgent & { template?: string | undefined };

export function AgentList() {
  const scope = useScopeIDs();
  const [all, setAll] = createSignal(false);
  const [search, setSearch] = createSignal("");
  const current = createQuery(() => ({ queryKey: ["deployments", "current", scope().projectID, scope().environmentID], queryFn: () => getCurrentDeployment(scope()), enabled: !all(), retry: false, refetchInterval: 15_000 }));
  const agents = createInfiniteQuery(() => ({
    queryKey: ["agents", "catalog", scope().projectID, scope().environmentID, all(), all() ? null : current.data?.id],
    queryFn: async ({ pageParam }): Promise<{ agents: AgentRow[]; next_cursor?: string | undefined }> => {
      if (all()) return listRetainedAgents(scope(), pageParam || undefined);
      const page = await listAgentDefinitions({ ...scope(), deploymentID: current.data!.id, cursor: pageParam || undefined, limit: 100 });
      return { agents: page.agents.map(item => ({ agent: item.agent, template: item.template, defined_in_current: true, current_deployment_id: page.deployment_id })), next_cursor: page.next_cursor };
    },
    initialPageParam: "", getNextPageParam: page => page.next_cursor,
    enabled: all() || (!!current.data && !current.isError), retry: false,
  }));
  const items = createMemo(() => agents.data?.pages.flatMap(page => page.agents) ?? []);
  const query = () => search().trim().toLowerCase();
  const shown = createMemo(() => query() ? items().filter(item => item.agent.toLowerCase().includes(query())) : items());
  const table = () => (
    <QueryState
      query={agents}
      pending="Loading Agents..."
      error={failure}
      empty={{
        when: (data) => data.pages.every(page => page.agents.length === 0),
        title: all() ? "No Agents registered in this environment." : "No Agents in the current Deployment.",
        hint: all() ? "Define Agents in code and deploy them." : "Choose All Agents to manage retained connections and history.",
      }}
    >
      {() => (
        <Show when={shown().length > 0} fallback={<EmptyState title="No loaded Agents match this filter." hint={agents.hasNextPage ? "Load more Agents to search further." : undefined} />}>
          <DataTable
            columns={all() ? ["Agent", "Status"] : ["Agent", "Status", "Template"]}
            footer={<LoadMore count={items().length} noun={{ one: "Agent", other: "Agents" }} hasMore={agents.hasNextPage} loading={agents.isFetchingNextPage} failed={agents.isFetchNextPageError} onLoadMore={() => void agents.fetchNextPage()} />}
          >
            <For each={shown()}>{item => <tr>
              <td><PrimaryLink href={agentHref(item.agent)}>{item.agent}</PrimaryLink></td>
              <td><StatusBadge resource="agent" status={item.defined_in_current ? "defined" : "not_defined"} /></td>
              <Show when={!all()}>
                <td><Show when={item.template} fallback={<EmptyCell />}>{template => <code>{template()}</code>}</Show></td>
              </Show>
            </tr>}</For>
          </DataTable>
        </Show>
      )}
    </QueryState>
  );
  return <>
    <Toolbar start={<>
      <Segmented label="Show Agents" value={all() ? "all" : "current"} options={SHOW_OPTIONS} onChange={value => setAll(value === "all")} />
      <div class="w-56 max-sm:w-full">
        <SearchInput aria-label="Filter Agents" placeholder="Filter loaded Agents…" value={search()} onInput={event => setSearch(event.currentTarget.value)} />
      </div>
    </>} />
    <Show when={all()} fallback={
      <QueryState
        query={current}
        pending="Loading current Agents..."
        error={failure}
        empty={{ when: (deployment) => !deployment, title: "No current Deployment.", hint: "Choose All Agents to manage retained connections and history." }}
      >
        {table}
      </QueryState>
    }>
      {table()}
    </Show>
  </>;
}

function agentStatusQuery(name: () => string) {
  const scope = useScopeIDs();
  return createQuery(() => ({
    queryKey: ["agents", "status", scope().projectID, scope().environmentID, name()],
    queryFn: () => getAgentStatus(name(), scope()),
    enabled: !!name() && !!scope().projectID && !!scope().environmentID,
    retry: false,
    refetchInterval: 15_000,
  }));
}

const AGENT_TABS: { value: AgentTab; label: string }[] = [
  { value: "sessions", label: "Sessions" },
  { value: "triggers", label: "Triggers" },
  { value: "slack", label: "Slack" },
];

// An Agent by its authored name: its status and Template in the current
// Deployment, then its Sessions, triggers and Slack connection as tabs, each a
// page of its own.
export function AgentDetail(props: { children?: JSX.Element }) {
  const scope = useScopeIDs();
  const params = useParams();
  const location = useLocation();
  const name = () => params["agent"] ?? "";
  const enabled = () => !!name() && !!scope().projectID && !!scope().environmentID;
  const agent = agentStatusQuery(name);
  const currentDeploymentID = () => agent.data?.defined_in_current ? agent.data.current_deployment_id ?? "" : "";
  const definition = createQuery(() => ({
    queryKey: ["agents", "definition", scope().projectID, scope().environmentID, currentDeploymentID(), name()],
    queryFn: () => getAgentDefinition(name(), { ...scope(), deploymentID: currentDeploymentID() }),
    enabled: !!currentDeploymentID(), retry: false,
  }));
  const missing = () => agent.error instanceof ApiError && agent.error.status === 404;
  const tabs = createMemo(() => AGENT_TABS.map(tab => ({ ...tab, href: agentHref(name(), tab.value) })));
  const selected = () => selectedLinkTab(tabs(), location.pathname);
  return <Page>
    <PageHeader
      title={name()}
      breadcrumb={[{ href: "/agents", label: "Agents" }]}
      badge={<Show when={agent.data}>{record => <StatusBadge resource="agent" status={record().defined_in_current ? "defined" : "not_defined"} />}</Show>}
      meta={<>
        <Show when={agent.data}>{record => (
          <Show when={record().current_deployment_id}>
            {deployment => <span>{record().defined_in_current ? "Defined in Deployment" : "Current Deployment"} <IDText value={deployment()} mode="link" href={deploymentHref(deployment())} /></span>}
          </Show>
        )}</Show>
        <Show when={currentDeploymentID() && definition.data?.template}>{template => <span>Template <span class="text-console-text">{template()}</span></span>}</Show>
      </>}
      actions={<CopyLinkButton />}
    />
    <Show when={enabled()} fallback={<EmptyState title="Select a project and environment." />}>
      <Show when={!missing()} fallback={<EmptyState title="This environment has no Agent with this name." hint="Agents appear once a Deployment in this environment defines them." />}>
        <QueryState query={agent} pending="Loading Agent..." error={failure}>
          {record => <>
            <Show when={!record().defined_in_current}>
              <Notice tone="info" class="mb-4">{record().current_deployment_id ? "This Agent is not in the current Deployment." : "This environment has no current Deployment."} New independent Sessions cannot start. Existing Sessions, trigger history and connections remain available for inspection and management.</Notice>
            </Show>
            <LinkTabs id="agent-tabs" label="Agent" items={tabs()} selected={selected()} />
            <TabPanel tabs="agent-tabs" value={selected()} selected={selected()}>{props.children}</TabPanel>
          </>}
        </QueryState>
      </Show>
    </Show>
  </Page>;
}

export function AgentSessionsTab() {
  const params = useParams();
  return <SessionList agent={params["agent"] ?? ""} />;
}

export function AgentTriggersTab() {
  const params = useParams();
  return <TriggerList agent={params["agent"] ?? ""} />;
}

export function AgentSlackTab() {
  const params = useParams();
  return <Show when={params["agent"]} keyed>{name => <AgentSlackConnection agentName={name} />}</Show>;
}
