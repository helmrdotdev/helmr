import { A, useParams, useSearchParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createMemo, For, Show } from "solid-js";
import { ApiError } from "../lib/api";
import { getCurrentDeployment } from "../lib/deployments";
import { definitionHref, scopedConsoleHref, getAgentDefinition, getTemplateDefinition, listAgentDefinitions, listTemplateDefinitions, type DefinitionKind, type DefinitionScope, type AgentDefinitionListItem, type TemplateDefinitionListItem, type AgentDefinition, type TemplateDefinition } from "../lib/definitions";
import { useRouteScope } from "../lib/scope";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { DetailItem, DetailList, Panel } from "../ui/Panel";
import { StatePanel } from "../ui/StatePanel";
import { ui } from "../ui/styles";

function value(input: string | string[] | undefined): string { return typeof input === "string" ? input : ""; }
function failure(error: unknown): string { return error instanceof ApiError ? error.message : "Could not load definitions."; }
function deploymentLink(id: string, scope: DefinitionScope): string { return scopedConsoleHref(`/deployments/${encodeURIComponent(id)}`, scope); }
function Resources(props: { item: TemplateDefinitionListItem }) {
  return <span>{props.item.resources.milliCpu / 1000} vCPU · {props.item.resources.memoryMiB} MiB RAM · {props.item.resources.diskMiB === undefined ? "Default disk" : `${props.item.resources.diskMiB} MiB disk`}</span>;
}

export function Agents() { return <Definitions kind="agents" />; }
export function Templates() { return <Definitions kind="templates" />; }
function Definitions(props: { kind: DefinitionKind }) {
  const scope = useRouteScope();
  const title = () => props.kind === "agents" ? "Agents" : "Templates";
  const enabled = () => !!scope().projectID && !!scope().environmentID;
  const current = createQuery(() => ({ queryKey: ["deployments", "current", scope().projectID, scope().environmentID], queryFn: () => getCurrentDeployment(scope()), enabled: enabled(), retry: false, refetchInterval: 15_000 }));
  // Pin every page to the observed current Deployment. Promotion changes this
  // query key, so pages from different definitions cannot be combined.
  const definitions = createInfiniteQuery(() => ({
    queryKey: [props.kind, "current-definitions", scope().projectID, scope().environmentID, current.data?.id],
    queryFn: async ({ pageParam }) => {
      const options = { ...scope(), deploymentID: current.data!.id, cursor: pageParam || undefined, limit: 100 };
      if (props.kind === "agents") { const page = await listAgentDefinitions(options); return { items: page.agent_definitions as (AgentDefinitionListItem | TemplateDefinitionListItem)[], next_cursor: page.next_cursor, deployment_id: page.deployment_id }; }
      const page = await listTemplateDefinitions(options); return { items: page.template_definitions as (AgentDefinitionListItem | TemplateDefinitionListItem)[], next_cursor: page.next_cursor, deployment_id: page.deployment_id };
    },
    initialPageParam: "", getNextPageParam: page => page.next_cursor,
    enabled: enabled() && !!current.data && !current.isError, retry: false,
  }));
  const items = createMemo(() => definitions.data?.pages.flatMap(page => page.items.map(item => ({ item, deploymentID: page.deployment_id }))) ?? []);
  return <section class={ui.page}>
    <PageHeader title={title()} subtitle={props.kind === "agents" ? "Agents defined in the current Deployment. Existing Sessions keep their pinned definitions." : "Templates defined in the current Deployment for creating Computers."} />
    <Show when={current.isError}><StatePanel error={failure(current.error)} /></Show>
    <Show when={enabled() && !current.isError} fallback={!enabled() ? <StatePanel empty="Select an environment to view definitions." /> : undefined}>
      <Show when={!current.isPending} fallback={<StatePanel loading="Loading current Deployment..." />}>
        <Show when={current.data} fallback={<StatePanel empty="No current Deployment." hint="Deploy your code to make its Agents and Templates available here." />}>
          {deployment => <>
            <p class={ui.muted}>Current Deployment: <IDText value={deployment().id} mode="link" href={deploymentLink(deployment().id, scope())} /></p>
            <Show when={definitions.isError}><StatePanel error={failure(definitions.error)} /></Show>
            <Show when={!definitions.isPending} fallback={<StatePanel loading={`Loading ${title()}...`} />}>
              <Show when={items().length} fallback={!definitions.isError ? <StatePanel empty={`No ${title()} in the current Deployment.`} hint="Add definitions in your code and deploy them. Older definitions remain accessible from their Sessions, Computers and Deployments." /> : undefined}>
                <DataTable columns={[props.kind === "agents" ? "Agent" : "Template", props.kind === "agents" ? "Template" : "Resources"]}>
                  <For each={items()}>{entry => <tr>
                    <td><A class="text-console-accent hover:text-console-accent-hover" href={definitionHref(props.kind, entry.item.id, { ...scope(), deploymentID: entry.deploymentID })}><code>{entry.item.id}</code></A></td>
                    <td>{"template_id" in entry.item ? <A class="text-console-accent hover:text-console-accent-hover" href={definitionHref("templates", entry.item.template_id, { ...scope(), deploymentID: entry.deploymentID })}><code>{entry.item.template_id}</code></A> : <Resources item={entry.item} />}</td>
                  </tr>}</For>
                </DataTable>
              </Show>
              <Show when={definitions.hasNextPage}><button type="button" class={ui.secondaryButton} disabled={definitions.isFetchingNextPage} onClick={() => void definitions.fetchNextPage()}>Load more {title()}</button></Show>
            </Show>
          </>}
        </Show>
      </Show>
    </Show>
  </section>;
}

export function AgentDetail() { return <DefinitionDetail kind="agents" />; }
export function TemplateDetail() { return <DefinitionDetail kind="templates" />; }
function DefinitionDetail(props: { kind: DefinitionKind }) {
  const scope = useRouteScope();
  const params = useParams();
  const [search] = useSearchParams();
  const id = () => params["definition_id"] ?? "";
  const selected = () => value(search["deployment_id"]);
  const enabled = () => !!id() && !!scope().projectID && !!scope().environmentID;
  const current = createQuery(() => ({ queryKey: ["deployments", "current", scope().projectID, scope().environmentID], queryFn: () => getCurrentDeployment(scope()), enabled: enabled(), retry: false, refetchInterval: 15_000 }));
  const deploymentID = () => selected() || current.data?.id || "";
  const definition = createQuery(() => ({
    queryKey: [props.kind, "definition", scope().projectID, scope().environmentID, deploymentID(), id()],
    queryFn: async (): Promise<AgentDefinition | TemplateDefinition> => props.kind === "agents" ? getAgentDefinition(id(), { ...scope(), deploymentID: deploymentID() }) : getTemplateDefinition(id(), { ...scope(), deploymentID: deploymentID() }),
    enabled: enabled() && !!deploymentID(), retry: false,
  }));
  const title = () => props.kind === "agents" ? "Agent" : "Template";
  return <section class={ui.page}>
    <PageHeader title={id()} subtitle={`${title()} definition in the selected Deployment.`} back={{ href: scopedConsoleHref(`/${props.kind}`, scope()), label: props.kind === "agents" ? "Agents" : "Templates" }} />
    <Show when={current.isError}><StatePanel error={failure(current.error)} /></Show>
    <Show when={definition.isError}><StatePanel error={failure(definition.error)} /></Show>
    <Show when={enabled() && (selected() || !current.isPending)} fallback={enabled() ? <StatePanel loading="Loading definition..." /> : <StatePanel empty="Select an environment to view this definition." />}>
      <Show when={deploymentID()} fallback={!current.isError ? <StatePanel empty="No current Deployment." hint="Open an existing Session or Deployment to inspect its pinned definitions." /> : undefined}>
        <Show when={!definition.isPending} fallback={<StatePanel loading="Loading definition..." />}>
          <Show when={definition.data}>{item => <>
            <Show when={current.isSuccess} fallback={<p class={ui.muted}>Current Deployment status is unavailable.</p>}>
              <p class={ui.muted}>{current.data?.id === item().deployment_id ? "This definition belongs to the current Deployment." : "This definition belongs to an earlier or unpromoted Deployment. Existing Sessions and Computers retain their original definitions; new independent work uses the current Deployment."}</p>
            </Show>
            <div class="grid grid-cols-[minmax(0,1fr)_300px] items-start gap-3.5 max-[960px]:grid-cols-1">
              <Panel title="Definition">
                <p class={ui.muted}>Managed in code. Deploy a new version to change this definition.</p>
                <Show when={"template_id" in item() ? item() as AgentDefinition : undefined}>{agent => <>
                  <p>Template: <A class="text-console-accent hover:text-console-accent-hover" href={definitionHref("templates", agent().template_id, { ...scope(), deploymentID: item().deployment_id })}>{agent().template_id}</A></p>
                  <p><A class="text-console-accent hover:text-console-accent-hover" href={`${scopedConsoleHref("/sessions", scope())}&agent_id=${encodeURIComponent(agent().agent_id)}`}>View Sessions for this Agent across Deployments</A></p>
                  <p><A class="text-console-accent hover:text-console-accent-hover" href={deploymentLink(item().deployment_id, scope())}>View Deployment, schedules and Slack connections</A></p>
                </>}</Show>
                <Show when={"resources" in item() ? item() as TemplateDefinition : undefined}>{template => <>
                  <p><Resources item={template()} /></p>
                  <p>Existing Computers retain the Template definition from their creation.</p>
                  <A class="text-console-accent hover:text-console-accent-hover" href={scopedConsoleHref("/computers", scope())}>View Computers</A>
                </>}</Show>
              </Panel>
              <DetailList title={`${title()} details`}>
                <DetailItem label="Name"><code>{item().id}</code></DetailItem>
                <DetailItem label="Deployment"><IDText value={item().deployment_id} mode="link" href={deploymentLink(item().deployment_id, scope())} /></DetailItem>
                <Show when={"agent_id" in item() ? (item() as AgentDefinition).agent_id : undefined}>{agentID => <DetailItem label="Agent ID"><IDText value={agentID()} /></DetailItem>}</Show>
              </DetailList>
            </div>
          </>}</Show>
        </Show>
      </Show>
    </Show>
  </section>;
}
