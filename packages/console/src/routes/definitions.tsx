import { A, useParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createMemo, For, Show } from "solid-js";
import { getCurrentDeployment } from "../lib/deployments";
import { errorMessage } from "../lib/error";
import { agentHref, definitionHref, definitionName, getAgentDefinition, getTemplateDefinition, listTemplateDefinitions, templateHref, type DefinitionKind, type TemplateListItem, type AgentDefinition, type TemplateDefinition } from "../lib/definitions";
import { deploymentHref } from "../features/deployments/navigation";
import { useScopeIDs } from "../lib/scope";
import { DataTable, LoadMore, PrimaryLink } from "../ui/DataTable";
import { formatID } from "../ui/id";
import { IDText } from "../ui/IDText";
import { Notice } from "../ui/Notice";
import { Page } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { DetailLayout, KeyValue, KeyValueList, Panel } from "../ui/Panel";
import { EmptyState, QueryState } from "../ui/QueryState";
import { cx, ui } from "../ui/styles";

const failure = (error: unknown) => errorMessage(error, "Could not load resources.");

function resources(item: TemplateListItem): string {
  const disk = item.resources.diskMiB === undefined ? "Default disk" : `${item.resources.diskMiB} MiB disk`;
  return `${item.resources.milliCpu / 1000} vCPU · ${item.resources.memoryMiB} MiB RAM · ${disk}`;
}

function useCurrentDeployment(enabled: () => boolean) {
  const scope = useScopeIDs();
  return createQuery(() => ({ queryKey: ["deployments", "current", scope().projectID, scope().environmentID], queryFn: () => getCurrentDeployment(scope()), enabled: enabled() && !!scope().projectID && !!scope().environmentID, retry: false, refetchInterval: 15_000 }));
}

// The Templates of the current Deployment, in the Computers section.
export function TemplateList() {
  const scope = useScopeIDs();
  const current = useCurrentDeployment(() => true);
  // Pin every page to the observed current Deployment. Promotion changes this
  // query key, so pages from different definitions cannot be combined.
  const definitions = createInfiniteQuery(() => ({
    queryKey: ["templates", "current-definitions", scope().projectID, scope().environmentID, current.data?.id],
    queryFn: async ({ pageParam }) => {
      const options = { ...scope(), deploymentID: current.data!.id, cursor: pageParam || undefined, limit: 100 };
      const page = await listTemplateDefinitions(options); return { items: page.templates, next_cursor: page.next_cursor };
    },
    initialPageParam: "", getNextPageParam: page => page.next_cursor,
    enabled: !!current.data && !current.isError, retry: false,
  }));
  const items = createMemo(() => definitions.data?.pages.flatMap(page => page.items) ?? []);
  return (
    <QueryState
      query={current}
      pending="Loading current Deployment..."
      error={failure}
      empty={{ when: (deployment) => !deployment, title: "No current Deployment.", hint: "Deploy your code to make its Templates available here." }}
    >
      {deployment => <>
        <p class={cx(ui.muted, "mb-3")}>Templates in the current Deployment <IDText value={deployment()!.id} mode="link" href={deploymentHref(deployment()!.id)} />, for creating Computers.</p>
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
              <For each={items()}>{item => <tr>
                <td><PrimaryLink href={templateHref(item.template)}>{item.template}</PrimaryLink></td>
                <td>{resources(item)}</td>
              </tr>}</For>
            </DataTable>
          )}
        </QueryState>
      </>}
    </QueryState>
  );
}

// A definition as code declared it. `pinned` shows it in its Deployment: links
// stay in that Deployment and a notice says whether it is the current one.
// Otherwise it is the current definition, linked to the current pages.
export function DefinitionView(props: { definition: AgentDefinition | TemplateDefinition; pinned?: { currentDeploymentID: string | undefined } }) {
  const agent = () => "agent" in props.definition ? props.definition as AgentDefinition : undefined;
  const template = () => "resources" in props.definition ? props.definition as TemplateDefinition : undefined;
  const templateLink = (name: string) => props.pinned ? definitionHref("templates", name, props.definition.deployment_id) : templateHref(name);
  return (
    <div class={ui.sectionStack}>
      <Show when={props.pinned}>
        {(pinned) => (
          <Show when={pinned().currentDeploymentID !== undefined} fallback={<Notice tone="warning">Current Deployment status is unavailable.</Notice>}>
            <Notice tone="info">{pinned().currentDeploymentID === props.definition.deployment_id ? "This definition belongs to the current Deployment." : "This definition belongs to an earlier or unpromoted Deployment. Existing Sessions and Computers retain their original definitions; new independent work uses the current Deployment."}</Notice>
          </Show>
        )}
      </Show>
      <DetailLayout rail={
        <KeyValueList title={`${agent() ? "Agent" : "Template"} definition`}>
          <KeyValue label="Name"><code>{definitionName(props.definition)}</code></KeyValue>
          <KeyValue label="Deployment"><IDText value={props.definition.deployment_id} mode="link" href={deploymentHref(props.definition.deployment_id)} /></KeyValue>
          <Show when={agent()}>{item => (
            <KeyValue label="Template"><PrimaryLink href={templateLink(item().template)}>{item().template}</PrimaryLink></KeyValue>
          )}</Show>
          <Show when={template()}>{item => (
            <KeyValue label="Resources">{resources(item())}</KeyValue>
          )}</Show>
        </KeyValueList>
      }>
        <Panel title="Configuration">
          <p class={ui.muted}>Managed in code. Deploy a new version to change this definition.</p>
          <Show when={props.pinned && agent()}>{item => (
            <p><A class={ui.monoLink} href={agentHref(item().agent)}>Open Agent</A></p>
          )}</Show>
          <Show when={template()}>
            <p class={ui.muted}>Existing Computers retain the Template definition from their creation.</p>
            <p><A class={ui.monoLink} href="/computers">View Computers</A></p>
          </Show>
        </Panel>
      </DetailLayout>
    </div>
  );
}

// A Template of the current Deployment.
export function TemplateDetail() {
  const scope = useScopeIDs();
  const params = useParams();
  const name = () => params["template"] ?? "";
  const enabled = () => !!name() && !!scope().projectID && !!scope().environmentID;
  const current = useCurrentDeployment(enabled);
  const definition = createQuery(() => ({
    queryKey: ["templates", "definition", scope().projectID, scope().environmentID, current.data?.id, name()],
    queryFn: () => getTemplateDefinition(name(), { ...scope(), deploymentID: current.data!.id }),
    enabled: enabled() && !!current.data, retry: false,
  }));
  return <Page>
    <PageHeader title={name()} subtitle="Template in the current Deployment." breadcrumb={[{ href: "/computers", label: "Computers" }, { href: "/templates", label: "Templates" }]} />
    <Show when={enabled()} fallback={<EmptyState title="Select a project and environment." />}>
      <QueryState
        query={current}
        pending="Loading definition..."
        error={failure}
        empty={{ when: (deployment) => !deployment, title: "No current Deployment.", hint: "Open an existing Computer or Deployment to inspect its pinned definitions." }}
      >
        {() => (
          <QueryState query={definition} pending="Loading definition..." error={failure}>
            {item => <DefinitionView definition={item()} />}
          </QueryState>
        )}
      </QueryState>
    </Show>
  </Page>;
}

export function DeploymentAgentDefinition() { return <PinnedDefinition kind="agents" />; }
export function DeploymentTemplateDefinition() { return <PinnedDefinition kind="templates" />; }

// A definition as pinned in one Deployment, shown inside that Deployment.
function PinnedDefinition(props: { kind: DefinitionKind }) {
  const scope = useScopeIDs();
  const params = useParams();
  const name = () => params[props.kind === "agents" ? "agent" : "template"] ?? "";
  const deploymentID = () => params["deployment_id"] ?? "";
  const enabled = () => !!name() && !!deploymentID() && !!scope().projectID && !!scope().environmentID;
  const current = useCurrentDeployment(enabled);
  const definition = createQuery(() => ({
    queryKey: [props.kind, "definition", scope().projectID, scope().environmentID, deploymentID(), name()],
    queryFn: async (): Promise<AgentDefinition | TemplateDefinition> => {
      const options = { ...scope(), deploymentID: deploymentID() };
      return props.kind === "agents" ? getAgentDefinition(name(), options) : getTemplateDefinition(name(), options);
    },
    enabled: enabled(), retry: false,
  }));
  const kindLabel = () => props.kind === "agents" ? "Agent" : "Template";
  return <Page>
    <PageHeader
      title={name()}
      subtitle={`${kindLabel()} definition pinned in this Deployment.`}
      breadcrumb={[{ href: "/deployments", label: "Deployments" }, { href: deploymentHref(deploymentID()), label: formatID(deploymentID()) }]}
    />
    <Show when={enabled()} fallback={<EmptyState title="Select a project and environment." />}>
      <QueryState query={definition} pending="Loading definition..." error={failure}>
        {item => <DefinitionView definition={item()} pinned={{ currentDeploymentID: current.isSuccess ? current.data?.id ?? "" : undefined }} />}
      </QueryState>
    </Show>
  </Page>;
}
