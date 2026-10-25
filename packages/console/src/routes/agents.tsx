import { AgentDeleteDialog } from "../features/agents/AgentDeleteDialog";
import { hasPermission } from "../lib/auth";
import { useLocation, useParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show, type JSX } from "solid-js";
import { listAccessTargets } from "../lib/permissions";
import { AgentAccess } from "../features/access/AgentAccess";
import { useMe } from "../lib/me";
import { ui } from "../ui/styles";
import { CopyLinkButton } from "../components/CopyLinkButton";
import { SectionLayout } from "../components/SectionLayout";
import { deploymentHref } from "../features/deployments/navigation";
import { AgentSlackConnection } from "../features/slack/AgentSlackConnection";
import { getCurrentDeployment } from "../lib/deployments";
import { agentHref, getAgentDefinition, getAgentStatus, listAgentDefinitions, listRetainedAgents, type AgentTab, type RetainedAgent } from "../lib/definitions";
import { loadError } from "../lib/error";
import { useScope, useScopeIDs } from "../lib/scope";
import { DataTable, EmptyCell, LoadMore, PrimaryLink } from "../ui/DataTable";
import { Notice } from "../ui/Notice";
import { IDText } from "../ui/IDText";
import { SearchInput, Segmented } from "../ui/Field";
import { Page, Toolbar } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { EmptyState, isNotFound, NotFoundState, QueryState } from "../ui/QueryState";
import { LinesSkeleton, TableSkeleton } from "../ui/Skeleton";
import { StatusBadge } from "../ui/StatusBadge";
import { selectedLinkTab } from "../ui/linkTab";
import { LinkTabs, TabPanel, type LinkTabItem } from "../ui/Tabs";
import { SessionList } from "./sessions";
import { TriggerList } from "./triggers";

const SHOW_OPTIONS = [
  { value: "current", label: "Current deployment" },
  { value: "all", label: "All agents" },
  { value: "deleted", label: "Deleted agents" },
] as const;

const failure = (error: unknown) => loadError(error, "Could not load agents.", { forbidden: "You do not have permission to view agents." });

type Section = "agents" | "triggers";
const SECTION_TABS: LinkTabItem<Section>[] = [
  { value: "agents", label: "Agents", href: "/agents" },
  { value: "triggers", label: "Triggers", href: "/triggers" },
];

// The Agents section: Agents and their triggers, one page each.
export function AgentsSection(props: { children?: JSX.Element }) {
  return (
    <SectionLayout id="agents-section" title="Agents" tabs={SECTION_TABS}>
      {props.children}
    </SectionLayout>
  );
}

type AgentRow = RetainedAgent & { template?: string | undefined };

export function AgentList() {
 const me=useMe();
 const [view,setView]=createSignal<"workloads"|"access">();
 const selected=()=>view() ?? (hasPermission(me.data,"access.manage") ? "access" : "workloads");
 return <><Show when={hasPermission(me.data,"access.manage")}><div class="mb-4"><Segmented label="Agent view" value={selected()} options={[{value:"workloads" as const,label:"Workloads"},{value:"access" as const,label:"Manage access"}]} onChange={setView} /></div></Show><Show when={selected()==="access" && hasPermission(me.data,"access.manage")} fallback={<OperationalAgentList />}><AccessAgentDirectory /></Show></>;
}

function AccessAgentDirectory() {
 const scope=useScopeIDs();
 const targets=createQuery(()=>({queryKey:["access","targets",scope().projectID],queryFn:()=>listAccessTargets(scope().projectID),enabled:!!scope().projectID}));
 const agents=()=>targets.data?.targets.find(t=>t.environment===scope().environmentID)?.agents ?? [];
 return <QueryState query={targets} pending={<TableSkeleton label="Loading agents" columns={["Agent"]} />} error={failure} empty={{ when: () => agents().length === 0, title: "No agents registered in this environment" }}>{()=> <DataTable columns={["Agent"]} footer={<LoadMore count={agents().length} noun={{ one: "agent", other: "agents" }} hasMore={false} loading={false} onLoadMore={() => undefined} />}><For each={agents()}>{agent=><tr><td><PrimaryLink href={agentHref(agent.key, "access")}>{agent.key}</PrimaryLink></td></tr>}</For></DataTable>}</QueryState>;
}

function OperationalAgentList() {
  const scope = useScopeIDs();
  const [catalogView, setCatalogView] = createSignal<"current" | "all" | "deleted">("current");
  const all = () => catalogView() !== "current";
  const [search, setSearch] = createSignal("");
  const current = createQuery(() => ({ queryKey: ["deployments", "current", scope().projectID, scope().environmentID], queryFn: () => getCurrentDeployment(scope()), enabled: !all(), retry: false, refetchInterval: 15_000 }));
  const agents = createInfiniteQuery(() => ({
    queryKey: ["agents", "catalog", scope().projectID, scope().environmentID, catalogView(), all() ? null : current.data?.id],
    queryFn: async ({ pageParam }): Promise<{ agents: AgentRow[]; next_cursor?: string | undefined }> => {
      if (all()) return listRetainedAgents(scope(), pageParam || undefined, catalogView() === "deleted" ? "deleted" : "all");
      const page = await listAgentDefinitions({ ...scope(), deploymentID: current.data!.id, cursor: pageParam || undefined, limit: 100 });
      return { agents: page.agents.map(item => ({ agent: item.agent, template: item.template, defined_in_current: true, deleted_at: null, revision: 0, current_deployment_id: page.deployment_id })), next_cursor: page.next_cursor };
    },
    initialPageParam: "", getNextPageParam: page => page.next_cursor,
    enabled: all() || (!!current.data && !current.isError), retry: false,
  }));
  const items = createMemo(() => agents.data?.pages.flatMap(page => page.agents) ?? []);
  const query = () => search().trim().toLowerCase();
  const shown = createMemo(() => query() ? items().filter(item => item.agent.toLowerCase().includes(query())) : items());
  // The filter already says these Agents are in the current deployment; only
  // "All agents" marks the ones that are not.
  const columns = () => all() ? ["Agent", "Status"] : ["Agent", "Template"];
  const table = () => (
    <QueryState
      query={agents}
      pending={<TableSkeleton label="Loading agents" columns={columns()} />}
      error={failure}
      empty={{
        when: (data) => data.pages.every(page => page.agents.length === 0),
        title: all() ? "No agents registered in this environment." : "No agents in the current deployment.",
        hint: all() ? "Define agents in code and deploy them." : "Show all agents to manage retained connections and history.",
      }}
    >
      {() => (
        <Show when={shown().length > 0} fallback={<EmptyState title="No loaded agents match this filter." hint={agents.hasNextPage ? "Load more agents to search further." : undefined} />}>
          <DataTable
            columns={columns()}
            footer={<LoadMore count={items().length} noun={{ one: "agent", other: "agents" }} hasMore={agents.hasNextPage} loading={agents.isFetchingNextPage} failed={agents.isFetchNextPageError} onLoadMore={() => void agents.fetchNextPage()} />}
          >
            <For each={shown()}>{item => <tr>
              <td><PrimaryLink href={agentHref(item.agent)}>{item.agent}</PrimaryLink></td>
              <Show when={all()}>
                <td><Show when={item.deleted_at || !item.defined_in_current} fallback={<EmptyCell />}><StatusBadge resource="agent" status={item.deleted_at ? "deleted" : "not_defined"} /></Show></td>
              </Show>
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
      <Segmented label="Show agents" value={catalogView()} options={SHOW_OPTIONS} onChange={setCatalogView} />
      <div class="w-56 max-sm:w-full">
        <SearchInput aria-label="Filter agents" placeholder="Filter agents…" value={search()} onInput={event => setSearch(event.currentTarget.value)} />
      </div>
    </>} />
    <Show when={all()} fallback={
      <QueryState
        query={current}
        pending={<TableSkeleton label="Loading agents" columns={columns()} />}
        error={failure}
        empty={{ when: (deployment) => !deployment, title: "No current deployment.", hint: "Show all agents to manage retained connections and history." }}
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
  const cache = useQueryClient();
  const [changeOpen, setChangeOpen] = createSignal(false);
  const [changeMessage, setChangeMessage] = createSignal("");
  const me = useMe();
  const scope = useScopeIDs();
  const scopeState = useScope();
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
  const accessTargets=createQuery(()=>({queryKey:["access","targets",scope().projectID],queryFn:()=>listAccessTargets(scope().projectID),enabled:enabled() && hasPermission(me.data,"access.manage")}));
  const accessIdentity=()=>accessTargets.data?.targets.find(t=>t.environment===scope().environmentID)?.agents.find(a=>a.key===name());
  const missing = () => !agent.data && !accessIdentity() && isNotFound(agent.error);
  const environment = () => scopeState.selectedEnvironment()?.name;
  const tabs = createMemo(() => [...AGENT_TABS, ...(hasPermission(me.data, "access.manage") && !agent.data?.deleted_at ? [{ value: "access" as const, label: "Access" }] : [])].map(tab => ({ ...tab, href: agentHref(name(), tab.value) })));
  const selected = () => selectedLinkTab(tabs(), location.pathname);
  return <Page>
    <PageHeader
      title={missing() ? "Agent not found" : name()}
      breadcrumb={[{ href: "/agents", label: "Agents" }]}
      badge={<Show when={agent.data}>{record => <StatusBadge resource="agent" status={record().deleted_at ? "deleted" : record().defined_in_current ? "defined" : "not_defined"} />}</Show>}
      meta={<>
        <Show when={agent.data}>{record => (
          <Show when={record().current_deployment_id}>
            {deployment => <span>{record().defined_in_current ? "Defined in deployment" : "Current deployment"} <IDText value={deployment()} mode="link" href={deploymentHref(deployment())} /></span>}
          </Show>
        )}</Show>
        <Show when={currentDeploymentID() && definition.data?.template}>{template => <span>Template <code>{template()}</code></span>}</Show>
      </>}
      actions={<Show when={!missing()}><Show when={agent.data && hasPermission(me.data,"access.manage") && hasPermission(me.data,"agents.manage",{...scope(),agent:name()})}><button class={ui.secondaryButton} onClick={() => setChangeOpen(true)}>{agent.data?.deleted_at ? "Restore" : "Delete"}</button></Show><CopyLinkButton /></Show>}
    />
    <Show when={changeMessage()}><Notice tone="success">{changeMessage()}</Notice></Show>
    <Show when={changeOpen()}><AgentDeleteDialog agent={name()} scope={scope()} restore={!!agent.data?.deleted_at} onClose={() => setChangeOpen(false)} onSaved={() => {
      setChangeMessage(agent.data?.deleted_at ? "Agent restored. Removed access and Slack connections remain removed." : "Agent deleted. History is retained; access and connection cleanup completed.");
      setChangeOpen(false);
      for (const key of ["agents", "access", "me", "triggers", "slack"]) void cache.invalidateQueries({queryKey:[key]});
    }} /></Show>
    <Show when={enabled()} fallback={<EmptyState title="Select a project and environment." />}>
      <Show when={!missing()} fallback={<NotFoundState noun="agent" id={name()} environment={environment()} back={{ href: "/agents", label: "Go to agents" }} />}>
        <Show when={!agent.data && accessIdentity()} fallback={<QueryState query={agent} pending={<LinesSkeleton label="Loading agent" />} error={failure}>
          {record => <>
            <Show when={!record().defined_in_current}>
              <Notice tone="info" class="mb-4">{record().current_deployment_id ? "This agent is not in the current deployment." : "This environment has no current deployment."} New independent sessions cannot start. Existing sessions, trigger history and connections remain available for inspection and management.</Notice>
            </Show>
            <LinkTabs id="agent-tabs" label="Agent" items={tabs()} selected={selected()} />
            <TabPanel tabs="agent-tabs" value={selected()} selected={selected()}>{props.children}</TabPanel>
          </>}
        </QueryState>}><AgentAccess agent={name()} /></Show>
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

export function AgentAccessTab() {
  const params = useParams();
  return <Show when={params["agent"]} keyed>{name => <AgentAccess agent={name} />}</Show>;
}

export function AgentSlackTab() {
  const params = useParams();
  return <Show when={params["agent"]} keyed>{name => <AgentSlackConnection agentName={name} />}</Show>;
}
