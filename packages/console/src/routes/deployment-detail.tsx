import { useParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, on, Show } from "solid-js";
import { ApiError } from "../lib/api";
import { getCurrentDeployment, getDeployment, getDeploymentEvents } from "../lib/deployments";
import { useRouteScope } from "../lib/scope";
import { definitionHref, definitionName, listAgentDefinitions, listTemplateDefinitions, type DefinitionListItem } from "../lib/definitions";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { RelativeTime } from "../ui/RelativeTime";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { cx, ui } from "../ui/styles";

const TABS = ["agents", "templates", "events"] as const;
type Tab = (typeof TABS)[number];

const TAB_LABELS: Record<Tab, string> = {
  agents: "Agents",
  templates: "Templates",
  events: "Events",
};

function errorMessage(error: unknown, fallback: string): string {
  if (error instanceof ApiError && error.code === "forbidden") return "You do not have permission to view this Deployment.";
  if (error instanceof ApiError && error.code === "deployment_not_materialized") {
    return "Definitions for this Deployment are not materialized yet.";
  }
  if (error instanceof ApiError) return error.message;
  return fallback;
}

function DefinitionTable(props: {
  label: string;
  href: (id: string) => string;
  items: DefinitionListItem[] | undefined;
  pending: boolean;
  error: unknown;
}) {
  const noun = () => props.label.toLowerCase();
  const columns = () => [props.label.replace(/e?s$/, "")];
  return (
    <Show when={!props.pending} fallback={<StatePanel loading={`Loading ${noun()}...`} />}>
      <Show when={!props.error} fallback={<StatePanel error={errorMessage(props.error, `Could not load ${noun()}.`)} />}>
        <Show
          when={(props.items?.length ?? 0) > 0}
          fallback={<StatePanel empty={`This Deployment declares no ${noun()}.`} />}
        >
          <DataTable columns={columns()}>
            <For each={props.items}>
              {(item) => (
                <tr>
                  <td><a class="text-console-accent hover:text-console-accent-hover" href={props.href(definitionName(item))}>{definitionName(item)}</a></td>
                </tr>
              )}
            </For>
          </DataTable>
        </Show>
      </Show>
    </Show>
  );
}

export function DeploymentDetail() {
  const params = useParams();
  const scope = useRouteScope();
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
    queryFn: ({ pageParam }) => listAgentDefinitions({ ...definitionOptions(), cursor: pageParam || undefined }),
    initialPageParam: "",
    getNextPageParam: page => page.next_cursor,
    enabled: enabled() && tab() === "agents",
    retry: false,
  }));
  const templates = createInfiniteQuery(() => ({
    queryKey: ["templates", deploymentID(), projectID(), environmentID()],
    queryFn: ({ pageParam }) => listTemplateDefinitions({ ...definitionOptions(), cursor: pageParam || undefined }),
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
    <section class={ui.page}>
      <PageHeader
        title={deployment.data?.version ?? "Deployment"}
        back={{ href: "/deployments", label: "Deployments" }}
        badge={<Show when={current.isSuccess} fallback={<span>Checking current Deployment…</span>}><Show when={isCurrent()} fallback={<span>Non-current</span>}><StatusBadge resource="deployment" status="current" /></Show></Show>}
        subtitle={
          <Show when={deployment.data}>
            {(record) => (
              <span class="grid gap-1">
                <IDText value={record().id} mode="full" />
                <span class="flex flex-wrap gap-x-4 gap-y-1">
                  <span>Bundle <IDText value={record().bundle_digest} /></span>
                  <span>Created <RelativeTime value={record().created_at} /></span>
                </span>
              </span>
            )}
          </Show>
        }
      />

      <Show when={deployment.isError}>
        <StatePanel error={errorMessage(deployment.error, "Could not load this Deployment.")} />
      </Show>
      <Show when={deployment.isPending && enabled()}>
        <StatePanel loading="Loading Deployment..." />
      </Show>

      <Show when={deployment.isSuccess}>
        <div class="mb-4 flex border-b border-console-border" aria-label="Deployment sections">
          <For each={TABS}>
            {(candidate) => (
              <button
                type="button"
                aria-pressed={tab() === candidate}
                class={cx(ui.logTab, tab() === candidate && ui.logTabActive)}
                onClick={() => setTab(candidate)}
              >
                {TAB_LABELS[candidate]}
              </button>
            )}
          </For>
        </div>

        <Show when={tab() === "agents"}>
          <p class={`${ui.muted} mb-3`}>Definitions in this Deployment. New independent Sessions use only the current Deployment; existing Sessions and owned children keep their pinned definitions. Inspecting another Deployment does not promote it.</p>
          <DefinitionTable href={id => definitionHref("agents", id, { ...resourceScope(), deploymentID: deploymentID() })} label="Agents" items={agents.data?.pages.flatMap(page => page.agents)} pending={agents.isPending} error={agents.error} />
          <Show when={agents.hasNextPage}>
            <button type="button" class={ui.secondaryButton} disabled={agents.isFetchingNextPage} onClick={() => void agents.fetchNextPage()}>Load more Agents</button>
          </Show>
        </Show>
        <Show when={tab() === "templates"}>
          <p class={`${ui.muted} mb-3`}>Computer creation settings in this Deployment. New Computers use the current Deployment. Existing Computers keep their original Template definition.</p>
          <DefinitionTable href={id => definitionHref("templates", id, { ...resourceScope(), deploymentID: deploymentID() })} label="Templates" items={templates.data?.pages.flatMap(page => page.templates)} pending={templates.isPending} error={templates.error} />
          <Show when={templates.hasNextPage}>
            <button type="button" class={ui.secondaryButton} disabled={templates.isFetchingNextPage} onClick={() => void templates.fetchNextPage()}>Load more Templates</button>
          </Show>
        </Show>

        <Show when={tab() === "events"}>
          <Show when={events.isError}>
            <StatePanel error={errorMessage(events.error, "Could not load Deployment events.")} />
          </Show>
          <Show when={!events.isPending} fallback={<StatePanel loading="Loading events..." />}>
            <Show when={eventItems().length > 0} fallback={<StatePanel empty="No events recorded for this Deployment." />}>
              <DataTable columns={["Kind", "Severity", "Message", "At"]} minWidth="min-w-180">
                <For each={eventItems()}>
                  {(event) => (
                    <tr>
                      <td><code>{event.kind}</code></td>
                      <td><span class={ui.muted}>{event.severity}</span></td>
                      <td>{event.message}</td>
                      <td><RelativeTime value={event.at} /></td>
                    </tr>
                  )}
                </For>
              </DataTable>
              <Show when={events.hasNextPage}>
                <div class={ui.actionRow}>
                  <button
                    type="button"
                    class={ui.secondaryButton}
                    disabled={events.isFetchingNextPage}
                    onClick={() => void events.fetchNextPage()}
                  >
                    {events.isFetchingNextPage ? "Loading..." : "Load more"}
                  </button>
                </div>
              </Show>
            </Show>
          </Show>
        </Show>
      </Show>

    </section>
  );
}
