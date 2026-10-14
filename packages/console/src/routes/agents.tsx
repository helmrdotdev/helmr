import { usePermission } from "../lib/me";
import { A, useParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, on, Show } from "solid-js";
import { AgentConnectionModal } from "../features/slack/AgentConnectionModal";
import { ApiError } from "../lib/api";
import { errorMessage } from "../lib/error";
import { agentHref, definitionHref, getAgentStatus, listAgentDefinitions, listRetainedAgents, type RetainedAgent } from "../lib/definitions";
import { getCurrentDeployment } from "../lib/deployments";
import { deploymentHref } from "../features/deployments/navigation";
import { listTriggers } from "../lib/triggers";
import { useScopeIDs } from "../lib/scope";
import { CopyLinkButton } from "../components/CopyLinkButton";
import { DataTable, LoadMore, PrimaryLink } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { Segmented } from "../ui/Field";
import { Page, Toolbar } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { Panel } from "../ui/Panel";
import { EmptyState, QueryState } from "../ui/QueryState";
import { RelativeTime } from "../ui/RelativeTime";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";

const SHOW_OPTIONS = [
  { value: "current", label: "Current Agents" },
  { value: "all", label: "All Agents" },
] as const;

const failure = (error: unknown) => errorMessage(error, "Could not load Agents.");

export function Agents() {
  const scope = useScopeIDs();
  const enabled = () => !!scope().projectID && !!scope().environmentID;
  const [all, setAll] = createSignal(false);
  const current = createQuery(() => ({ queryKey: ["deployments", "current", scope().projectID, scope().environmentID], queryFn: () => getCurrentDeployment(scope()), enabled: enabled() && !all(), retry: false, refetchInterval: 15_000 }));
  const agents = createInfiniteQuery(() => ({
    queryKey: ["agents", "catalog", scope().projectID, scope().environmentID, all(), all() ? null : current.data?.id],
    queryFn: async ({ pageParam }): Promise<{ agents: RetainedAgent[]; next_cursor?: string | undefined }> => {
      if (all()) return listRetainedAgents(scope(), pageParam || undefined);
      const page = await listAgentDefinitions({ ...scope(), deploymentID: current.data!.id, cursor: pageParam || undefined, limit: 100 });
      return { agents: page.agents.map(item => ({ agent: item.agent, defined_in_current: true, current_deployment_id: page.deployment_id })), next_cursor: page.next_cursor };
    },
    initialPageParam: "", getNextPageParam: page => page.next_cursor,
    enabled: enabled() && (all() || (!!current.data && !current.isError)), retry: false,
  }));
  const items = createMemo(() => agents.data?.pages.flatMap(page => page.agents) ?? []);
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
        <DataTable
          columns={["Agent", "Current Deployment"]}
          footer={<LoadMore count={items().length} noun="Agents" hasMore={agents.hasNextPage} loading={agents.isFetchingNextPage} failed={agents.isFetchNextPageError} onLoadMore={() => void agents.fetchNextPage()} />}
        >
          <For each={items()}>{item => <tr>
            <td><PrimaryLink href={agentHref(item.agent)}>{item.agent}</PrimaryLink></td>
            <td><StatusBadge resource="agent" status={item.defined_in_current ? "defined" : "not_defined"} /></td>
          </tr>}</For>
        </DataTable>
      )}
    </QueryState>
  );
  return <Page>
    <PageHeader title="Agents" subtitle="Manage Agent connections, Sessions and triggers across Deployments." />
    <Toolbar start={<Segmented label="Show Agents" value={all() ? "all" : "current"} options={SHOW_OPTIONS} onChange={value => setAll(value === "all")} />} />
    <Show when={enabled()} fallback={<EmptyState title="Select an environment to view Agents." />}>
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
    </Show>
  </Page>;
}

export function AgentDetail() {
  const scope = useScopeIDs();
  const params = useParams();
  const name = () => params["agent"] ?? "";
  const enabled = () => !!name() && !!scope().projectID && !!scope().environmentID;
  const [slack, setSlack] = createSignal(false);
  createEffect(on(() => JSON.stringify([scope().projectID, scope().environmentID, name()]), () => setSlack(false), { defer: true }));
  const permission = usePermission();
  const agent = createQuery(() => ({ queryKey: ["agents", "status", scope().projectID, scope().environmentID, name()], queryFn: () => getAgentStatus(name(), scope()), enabled: enabled(), retry: false, refetchInterval: 15_000 }));
  const triggers = createInfiniteQuery(() => ({
    queryKey: ["triggers", scope().projectID, scope().environmentID, name()],
    queryFn: ({ pageParam }) => listTriggers(scope(), pageParam || undefined, name()),
    initialPageParam: "", getNextPageParam: page => page.next_cursor,
    enabled: enabled() && agent.isSuccess, retry: false,
  }));
  const items = createMemo(() => triggers.data?.pages.flatMap(page => page.triggers) ?? []);
  const missing = () => agent.error instanceof ApiError && agent.error.status === 404;
  return <Page>
    <PageHeader title={name()} subtitle="Agent operations across Deployments." breadcrumb={[{ href: "/agents", label: "Agents" }]} actions={<CopyLinkButton />} />
    <Show when={enabled()} fallback={<EmptyState title="Select an environment to view this Agent." />}>
      <Show when={!missing()} fallback={<EmptyState title="This environment has no Agent with this name." hint="Agents appear once a Deployment in this environment defines them." />}>
        <QueryState query={agent} pending="Loading Agent..." error={failure}>
          {record => <div class={ui.sectionStack}>
            <Panel title="Agent">
              <Show when={record().defined_in_current && record().current_deployment_id} fallback={<p class={ui.muted}>{record().current_deployment_id ? "This Agent is not defined in the current Deployment." : "This environment has no current Deployment."} New independent Sessions cannot start. Existing Sessions, trigger history and connections remain available for inspection and management.</p>}>{deployment => <p><A class={ui.monoLink} href={definitionHref("agents", name(), deployment())}>View current definition</A></p>}</Show>
              <p><A class={ui.monoLink} href={`/sessions?${new URLSearchParams({ agent: name() })}`}>View Sessions for this Agent</A></p>
            </Panel>
            <Panel title="Slack Connection">
              <p class={ui.muted}>The connection belongs to this Agent in this environment and persists across Deployments.</p>
              <Show when={permission.isOrganizationAdmin()} fallback={<p class={ui.muted}>An organization owner or admin can manage this connection.</p>}>
                <div><button type="button" class={ui.secondaryButton} onClick={() => setSlack(true)}>Manage Slack connection</button></div>
              </Show>
            </Panel>
            <Panel title="Triggers">
              <p class={ui.muted}>Trigger activations across Deployments. Change triggers in code and promote a Deployment to activate them.</p>
              <QueryState
                query={triggers}
                pending="Loading triggers..."
                error={failure}
                empty={{ when: (data) => data.pages.every(page => page.triggers.length === 0), title: "No trigger activations for this Agent." }}
              >
                {() => (
                  <DataTable
                    columns={["Trigger", "Cron", "Timezone", "Next", "Active from", "Active until", "Deployment", "ID"]}
                    minWidth="min-w-250"
                    footer={<LoadMore count={items().length} noun="triggers" hasMore={triggers.hasNextPage} loading={triggers.isFetchingNextPage} failed={triggers.isFetchNextPageError} onLoadMore={() => void triggers.fetchNextPage()} />}
                  >
                    <For each={items()}>{trigger => <tr>
                      <td>{trigger.trigger_key}</td><td><code>{trigger.cron.pattern}</code></td><td>{trigger.cron.timezone}</td>
                      <td><RelativeTime value={trigger.next_fire_at} /></td><td><RelativeTime value={trigger.active_from} /></td><td><RelativeTime value={trigger.active_until} /></td>
                      <td><IDText value={trigger.deployment_id} mode="link" href={deploymentHref(trigger.deployment_id)} /></td><td><IDText value={trigger.id} /></td>
                    </tr>}</For>
                  </DataTable>
                )}
              </QueryState>
            </Panel>
            <Show when={slack()}><AgentConnectionModal agentName={name()} projectID={scope().projectID} environmentID={scope().environmentID} onClose={() => setSlack(false)} /></Show>
          </div>}
        </QueryState>
      </Show>
    </Show>
  </Page>;
}
