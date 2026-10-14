import { A, useParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createMemo, For, Show } from "solid-js";
import { getCurrentDeployment } from "../lib/deployments";
import { errorMessage } from "../lib/error";
import { agentHref, definitionHref, definitionName, getAgentDefinition, getTemplateDefinition, listTemplateDefinitions, type DefinitionKind, type TemplateListItem, type AgentDefinition, type TemplateDefinition } from "../lib/definitions";
import { deploymentHref } from "../features/deployments/navigation";
import { useScopeIDs } from "../lib/scope";
import { DataTable, LoadMore, PrimaryLink } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { Notice } from "../ui/Notice";
import { Page } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { DetailLayout, KeyValue, KeyValueList, Panel } from "../ui/Panel";
import { EmptyState, QueryState } from "../ui/QueryState";
import { ui } from "../ui/styles";

const failure = (error: unknown) => errorMessage(error, "Could not load resources.");

function resources(item: TemplateListItem): string {
  const disk = item.resources.diskMiB === undefined ? "Default disk" : `${item.resources.diskMiB} MiB disk`;
  return `${item.resources.milliCpu / 1000} vCPU · ${item.resources.memoryMiB} MiB RAM · ${disk}`;
}

export function Templates() {
  const scope = useScopeIDs();

  const enabled = () => !!scope().projectID && !!scope().environmentID;
  const current = createQuery(() => ({ queryKey: ["deployments", "current", scope().projectID, scope().environmentID], queryFn: () => getCurrentDeployment(scope()), enabled: enabled(), retry: false, refetchInterval: 15_000 }));
  // Pin every page to the observed current Deployment. Promotion changes this
  // query key, so pages from different definitions cannot be combined.
  const definitions = createInfiniteQuery(() => ({
    queryKey: ["templates", "current-definitions", scope().projectID, scope().environmentID, current.data?.id],
    queryFn: async ({ pageParam }) => {
      const options = { ...scope(), deploymentID: current.data!.id, cursor: pageParam || undefined, limit: 100 };
      const page = await listTemplateDefinitions(options); return { items: page.templates, next_cursor: page.next_cursor, deployment_id: page.deployment_id };
    },
    initialPageParam: "", getNextPageParam: page => page.next_cursor,
    enabled: enabled() && !!current.data && !current.isError, retry: false,
  }));
  const items = createMemo(() => definitions.data?.pages.flatMap(page => page.items.map(item => ({ item, deploymentID: page.deployment_id }))) ?? []);
  return <Page>
    <PageHeader title="Templates" subtitle="Templates defined in the current Deployment for creating Computers." />
    <Show when={enabled()} fallback={<EmptyState title="Select an environment to view Templates." />}>
      <QueryState
        query={current}
        pending="Loading current Deployment..."
        error={failure}
        empty={{ when: (deployment) => !deployment, title: "No current Deployment.", hint: "Deploy your code to make its Templates available here." }}
      >
        {deployment => <>
          <p class={`${ui.muted} mb-3`}>Current Deployment: <IDText value={deployment()!.id} mode="link" href={deploymentHref(deployment()!.id)} /></p>
          <QueryState
            query={definitions}
            pending="Loading Templates..."
            error={failure}
            empty={{
              when: (data) => data.pages.every(page => page.items.length === 0),
              title: "No Templates in the current Deployment.",
              hint: "Add definitions in your code and deploy them. Older definitions remain accessible from their Sessions, Computers and Deployments.",
            }}
          >
            {() => (
              <DataTable
                columns={["Template", "Resources"]}
                footer={<LoadMore count={items().length} noun="Templates" hasMore={definitions.hasNextPage} loading={definitions.isFetchingNextPage} failed={definitions.isFetchNextPageError} onLoadMore={() => void definitions.fetchNextPage()} />}
              >
                <For each={items()}>{entry => <tr>
                  <td><PrimaryLink href={definitionHref("templates", entry.item.template, entry.deploymentID)}>{entry.item.template}</PrimaryLink></td>
                  <td>{resources(entry.item)}</td>
                </tr>}</For>
              </DataTable>
            )}
          </QueryState>
        </>}
      </QueryState>
    </Show>
  </Page>;
}

export function AgentDefinitionDetail() { return <DefinitionDetail kind="agents" />; }
export function TemplateDetail() { return <DefinitionDetail kind="templates" />; }
function DefinitionDetail(props: { kind: DefinitionKind }) {
  const scope = useScopeIDs();
  const params = useParams();
  const id = () => params[props.kind === "agents" ? "agent" : "template"] ?? "";
  const selected = () => params["deployment_id"] || "";
  const enabled = () => !!id() && !!scope().projectID && !!scope().environmentID;
  const current = createQuery(() => ({ queryKey: ["deployments", "current", scope().projectID, scope().environmentID], queryFn: () => getCurrentDeployment(scope()), enabled: enabled(), retry: false, refetchInterval: 15_000 }));
  const deploymentID = () => selected() || current.data?.id || "";
  const definition = createQuery(() => ({
    queryKey: [props.kind, "definition", scope().projectID, scope().environmentID, deploymentID(), id()],
    queryFn: async (): Promise<AgentDefinition | TemplateDefinition> => props.kind === "agents" ? getAgentDefinition(id(), { ...scope(), deploymentID: deploymentID() }) : getTemplateDefinition(id(), { ...scope(), deploymentID: deploymentID() }),
    enabled: enabled() && !!deploymentID(), retry: false,
  }));
  const title = () => props.kind === "agents" ? "Agent" : "Template";
  const detail = () => (
    <QueryState query={definition} pending="Loading definition..." error={failure}>
      {item => <div class={ui.sectionStack}>
        <Show when={current.isSuccess} fallback={<Notice tone="warning">Current Deployment status is unavailable.</Notice>}>
          <Notice tone="info">{current.data?.id === item().deployment_id ? "This definition belongs to the current Deployment." : "This definition belongs to an earlier or unpromoted Deployment. Existing Sessions and Computers retain their original definitions; new independent work uses the current Deployment."}</Notice>
        </Show>
        <DetailLayout rail={
          <KeyValueList title={`${title()} details`}>
            <KeyValue label="Name"><code>{definitionName(item())}</code></KeyValue>
            <KeyValue label="Deployment"><IDText value={item().deployment_id} mode="link" href={deploymentHref(item().deployment_id)} /></KeyValue>
            <Show when={"agent" in item() ? item() as AgentDefinition : undefined}>{agent => (
              <KeyValue label="Template"><PrimaryLink href={definitionHref("templates", agent().template, item().deployment_id)}>{agent().template}</PrimaryLink></KeyValue>
            )}</Show>
            <Show when={"resources" in item() ? item() as TemplateDefinition : undefined}>{template => (
              <KeyValue label="Resources">{resources(template())}</KeyValue>
            )}</Show>
          </KeyValueList>
        }>
          <Panel title="Configuration">
            <p class={ui.muted}>Managed in code. Deploy a new version to change this definition.</p>
            <Show when={"agent" in item() ? item() as AgentDefinition : undefined}>{agent => (
              <p><A class={ui.monoLink} href={agentHref(agent().agent)}>Manage Agent connections, Sessions and triggers</A></p>
            )}</Show>
            <Show when={"resources" in item()}>
              <p class={ui.muted}>Existing Computers retain the Template definition from their creation.</p>
              <p><A class={ui.monoLink} href="/computers">View Computers</A></p>
            </Show>
          </Panel>
        </DetailLayout>
      </div>}
    </QueryState>
  );
  return <Page>
    <PageHeader title={id()} subtitle={`${title()} in the selected Deployment.`} breadcrumb={[selected() ? { href: deploymentHref(selected()), label: "Deployment" } : { href: `/${props.kind}`, label: props.kind === "agents" ? "Agents" : "Templates" }]} />
    <Show when={enabled()} fallback={<EmptyState title="Select an environment to view this definition." />}>
      <Show when={!selected()} fallback={detail()}>
        <QueryState
          query={current}
          pending="Loading definition..."
          error={failure}
          empty={{ when: (deployment) => !deployment, title: "No current Deployment.", hint: "Open an existing Session or Deployment to inspect its pinned definitions." }}
        >
          {detail}
        </QueryState>
      </Show>
    </Show>
  </Page>;
}
