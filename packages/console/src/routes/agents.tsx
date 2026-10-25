import { ApiError } from "../lib/api";
import { AgentDeleteDialog } from "../features/agents/AgentDeleteDialog";
import { hasPermission } from "../lib/auth";
import { useLocation, useParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show, type JSX } from "solid-js";
import { grantCoversAgent, listAccessTargets, listGrants } from "../lib/permissions";
import { AgentAccess } from "../features/access/AgentAccess";
import { useMe } from "../lib/me";
import { useCopyLink } from "../components/copyLink";
import type { ActionMenuItem } from "../ui/ActionMenu";
import { Tag } from "../ui/Tag";
import { SectionLayout } from "../components/SectionLayout";
import { deploymentHref } from "../features/deployments/navigation";
import { AgentSlackConnection } from "../features/slack/AgentSlackConnection";
import { agentHref, getAgentDefinition, getAgentStatus, listAgentDefinitions, listRetainedAgents, type AgentTab, type RetainedAgent } from "../lib/definitions";
import { loadError } from "../lib/error";
import { useScope, useScopeIDs } from "../lib/scope";
import { DataTable, EmptyCell, LoadMore, PrimaryLink, RowActions, TableCount } from "../ui/DataTable";
import { Notice } from "../ui/Notice";
import { IDText } from "../ui/IDText";
import { SearchInput, Segmented } from "../ui/Field";
import { Page, Toolbar } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { EmptyState, isDenied, isNotFound, NotFoundState, QueryState, NoScopeState } from "../ui/QueryState";
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

type Section = "agents" | "sessions" | "triggers";
const SECTION_TABS: LinkTabItem<Section>[] = [
  { value: "agents", label: "Agents", href: "/agents" },
  { value: "sessions", label: "Sessions", href: "/sessions" },
  { value: "triggers", label: "Triggers", href: "/triggers" },
];

// The Agents section: agents, their sessions and triggers, one page each.
export function AgentsSection(props: { children?: JSX.Element }) {
  return (
    <SectionLayout id="agents-section" title="Agents" tabs={SECTION_TABS}>
      {props.children}
    </SectionLayout>
  );
}

type AgentRow = RetainedAgent & { template?: string | undefined };

// The object menu for an Agent, the same in its list row and by its name.
function agentMenu(options: { agent: string; deleted: boolean; manageAccess: boolean; canDelete: boolean; canRestore: boolean; copyLink: ActionMenuItem; onChange: () => void }): ActionMenuItem[] {
  return [
    ...(options.manageAccess && !options.deleted ? [{ label: "Manage access", href: agentHref(options.agent, "access") }] : []),
    ...(options.canRestore && options.deleted ? [{ label: "Restore", onSelect: options.onChange }] : []),
    options.copyLink,
    ...(options.canDelete && !options.deleted ? [{ label: "Delete agent", tone: "danger" as const, onSelect: options.onChange }] : []),
  ];
}

const invalidateAfterChange = (cache: ReturnType<typeof useQueryClient>) => { for (const key of ["agents", "access", "me", "triggers", "slack"]) void cache.invalidateQueries({ queryKey: [key] }); };

// Who can use each agent, summarized from its role assignments.
function useAgentAudience(enabled: () => boolean) {
  const scope = useScopeIDs();
  const targets = createQuery(() => ({ queryKey: ["access", "targets", scope().projectID], queryFn: () => listAccessTargets(scope().projectID), enabled: enabled() && !!scope().projectID }));
  const grants = createQuery(() => ({ queryKey: ["access", "grants"], queryFn: listGrants, enabled: enabled() }));
  const environment = () => targets.data?.targets.find(t => t.environment === scope().environmentID);
  return (agent: string) => {
    const env = environment(); const id = env?.agents.find(a => a.key === agent)?.id;
    if (!env || !id || !grants.data) return undefined;
    const covering = grants.data.grants.filter(g => grantCoversAgent(g, env.environment_id, id));
    const people = new Set(covering.filter(g => g.subject_kind === "member").map(g => g.member_id)).size;
    const groups = new Set(covering.filter(g => g.subject_kind === "group").map(g => g.group_id)).size;
    const keys = new Set(covering.filter(g => g.subject_kind === "api_key").map(g => g.api_key_id)).size;
    const parts = [groups && `${groups} ${groups === 1 ? "group" : "groups"}`, people && `${people} ${people === 1 ? "person" : "people"}`, keys && `${keys} ${keys === 1 ? "API key" : "API keys"}`].filter(Boolean).join(" · ");
    return { shared: covering.some(g => g.subject_kind === "all_members"), parts };
  };
}

// Agents of the environment: one list for operating them and managing who can
// use them. Someone who can manage access but not read workloads sees the
// registered agents instead.
export function AgentList() {
  const scope = useScopeIDs(); const me = useMe(); const cache = useQueryClient();
  const manageAccess = () => hasPermission(me.data, "assignments.read");
  const audience = useAgentAudience(manageAccess);
  const copyLink = useCopyLink();
  const [changing, setChanging] = createSignal<{ agent: string; restore: boolean }>();
  const [catalogView, setCatalogView] = createSignal<"current" | "all" | "deleted">("current");
  const all = () => catalogView() !== "current";
  const [search, setSearch] = createSignal("");
  const agents = createInfiniteQuery(() => ({
    queryKey: ["agents", "catalog", scope().projectID, scope().environmentID, catalogView()],
    queryFn: async ({ pageParam }): Promise<{ agents: AgentRow[]; next_cursor?: string | undefined }> => {
      if (all()) return listRetainedAgents(scope(), pageParam || undefined, catalogView() === "deleted" ? "deleted" : "all");
      const page = await listAgentDefinitions({ ...scope(), cursor: pageParam || undefined, limit: 100 }).catch(error => {
        if (error instanceof ApiError && error.code === "no_current_deployment") return { agents: [], deployment_id: "", next_cursor: undefined };
        throw error;
      });
      return { agents: page.agents.map(item => ({ agent: item.agent, template: item.template, defined_in_current: true, deleted_at: null, revision: 0, current_deployment_id: page.deployment_id })), next_cursor: page.next_cursor };
    },
    initialPageParam: "", getNextPageParam: page => page.next_cursor,
    enabled: !!scope().projectID && !!scope().environmentID, retry: false, refetchInterval: 15_000,
  }));
  const items = createMemo(() => agents.data?.pages.flatMap(page => page.agents) ?? []);
  const query = () => search().trim().toLowerCase();
  const shown = createMemo(() => query() ? items().filter(item => item.agent.toLowerCase().includes(query())) : items());
  // The filter already says these agents are in the current deployment; only
  // "All" and "Deleted" mark the ones that are not.
  const columns = () => [...(all() ? ["Agent", "Status"] : ["Agent", "Template"]), ...(manageAccess() ? ["Who can use it"] : []), { label: "Actions", srOnly: true }];
  const denied = () => manageAccess() && isDenied(agents.error);
  const audienceCell = (agent: string) => {
    const who = audience(agent);
    return <td><Show when={who} fallback={<EmptyCell />}>{w => <Show when={w().shared || w().parts} fallback={<span class="text-console-muted">Nobody yet</span>}>
      <span class="inline-flex items-center gap-2"><Show when={w().shared}><Tag>all members</Tag></Show><Show when={w().parts}><span>{w().shared ? `+ ${w().parts}` : w().parts}</span></Show></span>
    </Show>}</Show></td>;
  };
  const table = () => (
    <QueryState
      query={agents}
      pending={<TableSkeleton label="Loading agents" columns={columns()} />}
      error={failure}
      empty={{
        when: (data) => data.pages.every(page => page.agents.length === 0),
        title: catalogView() === "deleted" ? "No deleted agents." : all() ? "No agents registered in this environment." : "No agents in the current deployment.",
        hint: all() ? undefined : "Show all agents to manage retained connections and history.",
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
              <Show when={manageAccess()}>{audienceCell(item.agent)}</Show>
              <RowActions label={`Actions for ${item.agent}`} items={agentMenu({
                agent: item.agent, deleted: !!item.deleted_at, manageAccess: manageAccess(),
                canDelete: hasPermission(me.data, "agents.delete", { ...scope(), agent: item.agent }),
                canRestore: hasPermission(me.data, "agents.restore", { ...scope(), agent: item.agent }),
                copyLink: copyLink.item(agentHref(item.agent)),
                onChange: () => setChanging({ agent: item.agent, restore: !!item.deleted_at }),
              })} />
            </tr>}</For>
          </DataTable>
        </Show>
      )}
    </QueryState>
  );
  return <>
    <copyLink.LiveRegion />
    <Show when={!denied()} fallback={<AccessAgentDirectory />}>
      <Toolbar start={<>
        <Segmented label="Show agents" value={catalogView()} options={SHOW_OPTIONS} onChange={setCatalogView} />
        <div class="w-56 max-sm:w-full">
          <SearchInput aria-label="Filter agents" placeholder="Filter agents…" value={search()} onInput={event => setSearch(event.currentTarget.value)} />
        </div>
      </>} />
      {table()}
    </Show>
    <Show when={changing()} keyed>{target => <AgentDeleteDialog agent={target.agent} scope={scope()} restore={target.restore} onClose={() => setChanging(undefined)} onSaved={() => { setChanging(undefined); invalidateAfterChange(cache); }} />}</Show>
  </>;
}

function AccessAgentDirectory() {
  const scope = useScopeIDs();
  const targets = createQuery(() => ({ queryKey: ["access", "targets", scope().projectID], queryFn: () => listAccessTargets(scope().projectID), enabled: !!scope().projectID }));
  const agents = () => targets.data?.targets.find(t => t.environment === scope().environmentID)?.agents ?? [];
  return <QueryState query={targets} pending={<TableSkeleton label="Loading agents" columns={["Agent", { label: "Actions", srOnly: true }]} />} error={failure} empty={{ when: () => agents().length === 0, title: "No agents registered in this environment" }}>{() => <DataTable columns={["Agent", { label: "Actions", srOnly: true }]} footer={<TableCount count={agents().length} noun={{ one: "agent", other: "agents" }} />}><For each={agents()}>{agent => <tr><td><PrimaryLink href={agentHref(agent.key, "access")}>{agent.key}</PrimaryLink></td><RowActions label={`Actions for ${agent.key}`} items={[{ label: "Manage access", href: agentHref(agent.key, "access") }]} /></tr>}</For></DataTable>}</QueryState>;
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
  const can = (permission: string) => hasPermission(me.data, permission, { ...scope(), agent: name() });
  const copyLink = useCopyLink();
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
    enabled: !!currentDeploymentID() && (hasPermission(me.data, "agents.read", {...scope(), agent: name()}) || hasPermission(me.data, "agents.use", {...scope(), agent: name()})), retry: false,
  }));
  const accessTargets=createQuery(()=>({queryKey:["access","targets",scope().projectID],queryFn:()=>listAccessTargets(scope().projectID),enabled:enabled() && hasPermission(me.data,"assignments.read")}));
  const accessIdentity=()=>accessTargets.data?.targets.find(t=>t.environment===scope().environmentID)?.agents.find(a=>a.key===name());
  const missing = () => !agent.data && !accessIdentity() && isNotFound(agent.error);
  const environment = () => scopeState.selectedEnvironment()?.name;
  const tabs = createMemo(() => [...AGENT_TABS, ...(hasPermission(me.data, "assignments.read") && !agent.data?.deleted_at ? [{ value: "access" as const, label: "Access" }] : [])].map(tab => ({ ...tab, href: agentHref(name(), tab.value) })));
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
      menu={missing() ? undefined : agentMenu({ agent: name(), deleted: !!agent.data?.deleted_at, manageAccess: hasPermission(me.data, "assignments.read"), canDelete: !!agent.data && can("agents.delete"), canRestore: !!agent.data && can("agents.restore"), copyLink: copyLink.item(), onChange: () => setChangeOpen(true) })}
      menuLabel="Agent actions"
    />
    <copyLink.LiveRegion />
    <Show when={changeMessage()}><Notice tone="success" class="mb-4" onDismiss={() => setChangeMessage("")}>{changeMessage()}</Notice></Show>
    <Show when={changeOpen()}><AgentDeleteDialog agent={name()} scope={scope()} restore={!!agent.data?.deleted_at} onClose={() => setChangeOpen(false)} onSaved={() => {
      setChangeMessage(agent.data?.deleted_at ? "Agent restored. Removed access and Slack connections remain removed." : "Agent deleted. History is retained; access and connection cleanup completed.");
      setChangeOpen(false);
      invalidateAfterChange(cache);
    }} /></Show>
    <Show when={enabled()} fallback={<NoScopeState />}>
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
