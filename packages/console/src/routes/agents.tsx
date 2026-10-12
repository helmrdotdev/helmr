import { A, useParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, on, Show } from "solid-js";
import { AgentConnectionModal } from "../features/slack/AgentConnectionModal";
import { ApiError } from "../lib/api";
import { getMe } from "../lib/auth";
import { agentHref, definitionHref, getAgentStatus, listAgentDefinitions, listRetainedAgents, scopedConsoleHref } from "../lib/definitions";
import { getCurrentDeployment } from "../lib/deployments";
import { listTriggers } from "../lib/triggers";
import { useRouteScope } from "../lib/scope";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { Panel } from "../ui/Panel";
import { RelativeTime } from "../ui/RelativeTime";
import { StatePanel } from "../ui/StatePanel";
import { ui } from "../ui/styles";

function failure(error: unknown): string { return error instanceof ApiError ? error.message : "Could not load Agents."; }

export function Agents() {
  const scope = useRouteScope();
  const enabled = () => !!scope().projectID && !!scope().environmentID;
  const [all, setAll] = createSignal(false);
  const current = createQuery(() => ({ queryKey: ["deployments", "current", scope().projectID, scope().environmentID], queryFn: () => getCurrentDeployment(scope()), enabled: enabled() && !all(), retry: false, refetchInterval: 15_000 }));
  const agents = createInfiniteQuery(() => ({
    queryKey: ["agents", "catalog", scope().projectID, scope().environmentID, all(), all() ? null : current.data?.id],
    queryFn: async ({ pageParam }) => {
      if (all()) return listRetainedAgents(scope(), pageParam || undefined);
      const page = await listAgentDefinitions({ ...scope(), deploymentID: current.data!.id, cursor: pageParam || undefined, limit: 100 });
      return { agents: page.agents.map(item => ({ agent: item.agent, defined_in_current: true, current_deployment_id: page.deployment_id })), next_cursor: page.next_cursor };
    },
    initialPageParam: "", getNextPageParam: page => page.next_cursor,
    enabled: enabled() && (all() || (!!current.data && !current.isError)), retry: false,
  }));
  const items = createMemo(() => agents.data?.pages.flatMap(page => page.agents) ?? []);
  return <section class={ui.page}>
    <PageHeader title="Agents" subtitle="Manage Agent connections, Sessions and triggers across Deployments." />
    <label class={ui.muted}>Show <select aria-label="Show Agents" class={ui.input} value={all() ? "all" : "current"} onChange={event => setAll(event.currentTarget.value === "all")}><option value="current">Current Agents</option><option value="all">All Agents</option></select></label>
    <Show when={enabled()} fallback={<StatePanel empty="Select an environment to view Agents." />}>
      <Show when={!all() && current.isPending}><StatePanel loading="Loading current Agents..." /></Show>
      <Show when={!all() && current.isError}><StatePanel error={failure(current.error)} /></Show>
      <Show when={!all() && current.isSuccess && !current.data}><StatePanel empty="No current Deployment." hint="Choose All Agents to manage retained connections and history." /></Show>
      <Show when={all() || !!current.data}>
        <Show when={agents.isError}><StatePanel error={failure(agents.error)} /></Show>
        <Show when={!agents.isPending} fallback={<StatePanel loading="Loading Agents..." />}>
          <Show when={items().length} fallback={!agents.isError ? <StatePanel empty={all() ? "No Agents registered in this environment." : "No Agents in the current Deployment."} hint={all() ? "Define Agents in code and deploy them." : "Choose All Agents to manage retained connections and history."} /> : undefined}>
            <DataTable columns={["Agent", "Current definition"]}><For each={items()}>{item => <tr>
              <td><A class="text-console-accent hover:text-console-accent-hover" href={agentHref(item.agent, scope())}><code>{item.agent}</code></A></td>
              <td>{item.defined_in_current ? "In current Deployment" : "Not in current Deployment"}</td>
            </tr>}</For></DataTable>
          </Show>
          <Show when={agents.hasNextPage}><button class={ui.secondaryButton} disabled={agents.isFetchingNextPage} onClick={() => void agents.fetchNextPage()}>Load more Agents</button></Show>
        </Show>
      </Show>
    </Show>
  </section>;
}

export function AgentDetail() {
  const scope = useRouteScope();
  const params = useParams();
  const name = () => params["agent"] ?? "";
  const enabled = () => !!name() && !!scope().projectID && !!scope().environmentID;
  const [slack, setSlack] = createSignal(false);
  createEffect(on(() => JSON.stringify([scope().projectID, scope().environmentID, name()]), () => setSlack(false), { defer: true }));
  const me = createQuery(() => ({ queryKey: ["me"], queryFn: getMe, retry: false, staleTime: 60_000 }));
  const agent = createQuery(() => ({ queryKey: ["agents", "status", scope().projectID, scope().environmentID, name()], queryFn: () => getAgentStatus(name(), scope()), enabled: enabled(), retry: false, refetchInterval: 15_000 }));
  const triggers = createInfiniteQuery(() => ({
    queryKey: ["triggers", scope().projectID, scope().environmentID, name()],
    queryFn: ({ pageParam }) => listTriggers(scope(), pageParam || undefined, name()),
    initialPageParam: "", getNextPageParam: page => page.next_cursor,
    enabled: enabled() && agent.isSuccess, retry: false,
  }));
  const items = createMemo(() => triggers.data?.pages.flatMap(page => page.triggers) ?? []);
  return <section class={ui.page}>
    <PageHeader title={name()} subtitle="Agent operations across Deployments." back={{ href: scopedConsoleHref("/agents", scope()), label: "Agents" }} />
    <Show when={agent.isError}><StatePanel error={failure(agent.error)} /></Show>
    <Show when={enabled()} fallback={<StatePanel empty="Select an environment to view this Agent." />}>
      <Show when={!agent.isPending} fallback={<StatePanel loading="Loading Agent..." />}>
        <Show when={agent.data}>{record => <>
          <Panel title="Agent">
            <Show when={record().defined_in_current && record().current_deployment_id} fallback={<p class={ui.muted}>{record().current_deployment_id ? "This Agent is not defined in the current Deployment." : "This environment has no current Deployment."} New independent Sessions cannot start. Existing Sessions, trigger history and connections remain available for inspection and management.</p>}>{deployment => <p><A class="text-console-accent hover:text-console-accent-hover" href={definitionHref("agents", name(), { ...scope(), deploymentID: deployment() })}>View current definition</A></p>}</Show>
            <p><A class="text-console-accent hover:text-console-accent-hover" href={`${scopedConsoleHref("/sessions", scope())}&agent=${encodeURIComponent(name())}`}>View Sessions for this Agent</A></p>
          </Panel>
          <Panel title="Slack Connection">
            <p class={ui.muted}>The connection belongs to this Agent in this environment and persists across Deployments.</p>
            <Show when={me.data?.role === "owner" || me.data?.role === "admin"} fallback={<p class={ui.muted}>An organization owner or admin can manage this connection.</p>}><button class={ui.secondaryButton} onClick={() => setSlack(true)}>Manage Slack connection</button></Show>
          </Panel>
          <Panel title="Triggers">
            <p class={ui.muted}>Trigger activations across Deployments. Change triggers in code and promote a Deployment to activate them.</p>
            <Show when={triggers.isError}><StatePanel error={failure(triggers.error)} /></Show>
            <Show when={!triggers.isPending} fallback={<StatePanel loading="Loading triggers..." />}>
              <Show when={items().length} fallback={!triggers.isError ? <StatePanel empty="No trigger activations for this Agent." /> : undefined}>
                <DataTable columns={["Trigger", "Cron", "Timezone", "Next", "Active from", "Active until", "Deployment", "ID"]} minWidth="min-w-250">
                  <For each={items()}>{trigger => <tr>
                    <td>{trigger.trigger_key}</td><td><code>{trigger.cron.pattern}</code></td><td>{trigger.cron.timezone}</td>
                    <td><RelativeTime value={trigger.next_fire_at} /></td><td><RelativeTime value={trigger.active_from} /></td><td><RelativeTime value={trigger.active_until} /></td>
                    <td><IDText value={trigger.deployment_id} mode="link" href={scopedConsoleHref(`/deployments/${encodeURIComponent(trigger.deployment_id)}`, scope())} /></td><td><IDText value={trigger.id} /></td>
                  </tr>}</For>
                </DataTable>
              </Show>
              <Show when={triggers.hasNextPage}><button class={ui.secondaryButton} disabled={triggers.isFetchingNextPage} onClick={() => void triggers.fetchNextPage()}>Load more triggers</button></Show>
            </Show>
          </Panel>
          <Show when={slack()}><AgentConnectionModal agentName={name()} projectID={scope().projectID} environmentID={scope().environmentID} onClose={() => setSlack(false)} /></Show>
        </>}</Show>
      </Show>
    </Show>
  </section>;
}
