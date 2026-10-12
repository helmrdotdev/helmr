import { useParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, on, Show } from "solid-js";
import { canPromoteDeployment, PromoteDeploymentModal } from "../features/deployments/PromoteDeploymentModal";
import { AgentConnectionModal } from "../features/slack/AgentConnectionModal";
import { ApiError } from "../lib/api";
import { getMe, hasPermission } from "../lib/auth";
import { getCurrentDeployment, getDeployment, getDeploymentEvents } from "../lib/deployments";
import { listSchedules } from "../lib/schedules";
import { useRouteScope, useScope } from "../lib/scope";
import { definitionHref, listAgentDefinitions, listTemplateDefinitions, type DefinitionListItem } from "../lib/definitions";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { RelativeTime } from "../ui/RelativeTime";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { cx, ui } from "../ui/styles";

const TABS = ["agent_definitions", "template_definitions", "schedules", "events"] as const;
type Tab = (typeof TABS)[number];

const TAB_LABELS: Record<Tab, string> = {
  agent_definitions: "Agent definitions",
  template_definitions: "Template definitions",
  schedules: "Schedules",
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
  onSlack?: ((id: string) => void) | undefined;
}) {
  const noun = () => props.label.toLowerCase();
  const columns = () => [
    props.label.replace(/e?s$/, ""),
    ...((props.onSlack) ? [{ label: "Actions", srOnly: true }] : []),
  ];
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
                  <td><a class="text-console-accent hover:text-console-accent-hover" href={props.href(item.id)}>{item.id}</a></td>
                  <Show when={props.onSlack}>
                    <td class={ui.actionsCell}>
                      <Show when={props.onSlack}>{(edit) => <button type="button" class={ui.secondaryButton} onClick={() => edit()(item.id)}>Slack connection</button>}</Show>
                    </td>
                  </Show>
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
  const globalScope = useScope();
  const deploymentID = createMemo(() => params["deployment_id"]?.trim() ?? "");
  const projectID = () => scope().projectID;
  const environmentID = () => scope().environmentID;
  const enabled = () => !!deploymentID() && !!projectID() && !!environmentID();
  const resourceScope = () => ({ projectID: projectID(), environmentID: environmentID() });
  const [tab, setTab] = createSignal<Tab>("agent_definitions");
  const [slackAgent, setSlackAgent] = createSignal<string>();
  const [promoting, setPromoting] = createSignal(false);
  const selection = createMemo(() => JSON.stringify([projectID(), environmentID(), deploymentID()]));
  createEffect(on(selection, () => {
    setSlackAgent(undefined);
    setPromoting(false);
    setTab("agent_definitions");
  }, { defer: true }));
  const me = createQuery(() => ({ queryKey: ["me"], queryFn: getMe, retry: false, staleTime: 60_000 }));

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
  const canPromote = createMemo(() => !!deployment.data && canPromoteDeployment({
    permitted: hasPermission(me.data, "deployments.write"),
    currentLoaded: current.isSuccess,
    isCurrent: isCurrent(),
  }));
  const definitionOptions = () => ({ ...resourceScope(), deploymentID: deploymentID(), limit: 100 });
  const agents = createInfiniteQuery(() => ({
    queryKey: ["agent_definitions", deploymentID(), projectID(), environmentID()],
    queryFn: ({ pageParam }) => listAgentDefinitions({ ...definitionOptions(), cursor: pageParam || undefined }),
    initialPageParam: "",
    getNextPageParam: page => page.next_cursor,
    enabled: enabled() && tab() === "agent_definitions",
    retry: false,
  }));
  const templateDefinitions = createInfiniteQuery(() => ({
    queryKey: ["template_definitions", deploymentID(), projectID(), environmentID()],
    queryFn: ({ pageParam }) => listTemplateDefinitions({ ...definitionOptions(), cursor: pageParam || undefined }),
    initialPageParam: "",
    getNextPageParam: page => page.next_cursor,
    enabled: enabled() && tab() === "template_definitions",
    retry: false,
  }));
  const schedules = createInfiniteQuery(() => ({
    queryKey: ["schedules", projectID(), environmentID()],
    queryFn: ({ pageParam }) => listSchedules(resourceScope(), pageParam || undefined),
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor ?? undefined,
    enabled: enabled() && tab() === "schedules",
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
  const scheduleItems = createMemo(() => schedules.data?.pages.flatMap((page) => page.schedules).filter((schedule) => schedule.deployment_id === deploymentID()) ?? []);

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
        actions={
          <Show when={canPromote()}>
            <button type="button" class={ui.secondaryButton} onClick={() => setPromoting(true)}>Promote</button>
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

        <Show when={tab() === "agent_definitions"}>
          <p class={`${ui.muted} mb-3`}>Definitions in this Deployment. New independent Sessions use only the current Deployment; existing Sessions and owned children keep their pinned definitions. Inspecting another Deployment does not promote it.</p>
          <DefinitionTable href={id => definitionHref("agents", id, { ...resourceScope(), deploymentID: deploymentID() })} label="Agent definitions" items={agents.data?.pages.flatMap(page => page.agent_definitions)} pending={agents.isPending} error={agents.error} onSlack={me.data?.role === "owner" || me.data?.role === "admin" ? setSlackAgent : undefined} />
          <Show when={agents.hasNextPage}>
            <button type="button" class={ui.secondaryButton} disabled={agents.isFetchingNextPage} onClick={() => void agents.fetchNextPage()}>Load more Agent definitions</button>
          </Show>
        </Show>
        <Show when={tab() === "template_definitions"}>
          <p class={`${ui.muted} mb-3`}>Computer creation settings in this Deployment. New Computers use the current Deployment. Existing Computers keep their original Template definition.</p>
          <DefinitionTable href={id => definitionHref("templates", id, { ...resourceScope(), deploymentID: deploymentID() })} label="Template definitions" items={templateDefinitions.data?.pages.flatMap(page => page.template_definitions)} pending={templateDefinitions.isPending} error={templateDefinitions.error} />
          <Show when={templateDefinitions.hasNextPage}>
            <button type="button" class={ui.secondaryButton} disabled={templateDefinitions.isFetchingNextPage} onClick={() => void templateDefinitions.fetchNextPage()}>Load more Template definitions</button>
          </Show>
        </Show>

        <Show when={tab() === "schedules"}>
            <Show when={schedules.isError}>
              <StatePanel error={errorMessage(schedules.error, "Could not load schedules.")} />
            </Show>
            <Show when={!schedules.isPending} fallback={<StatePanel loading="Loading schedules..." />}>
              <Show when={scheduleItems().length > 0} fallback={<StatePanel empty="No schedule activations for this Deployment in the loaded pages." />}>
                <DataTable columns={["Agent", "Trigger", "Cron", "Timezone", "Next", "Active from", "Active until", "ID"]} minWidth="min-w-250">
                  <For each={scheduleItems()}>
                    {(schedule) => (
                      <tr>
                        <td><IDText value={schedule.agent_id} /></td>
                        <td>{schedule.trigger_key}</td>
                        <td><code>{schedule.cron.pattern}</code></td>
                        <td><span class={ui.muted}>{schedule.cron.timezone}</span></td>
                        <td><RelativeTime value={schedule.next_fire_at} /></td>
                        <td><RelativeTime value={schedule.active_from} /></td>
                        <td><RelativeTime value={schedule.active_until} /></td>
                        <td><IDText value={schedule.id} /></td>
                      </tr>
                    )}
                  </For>
                </DataTable>
              </Show>
            </Show>
          <Show when={schedules.hasNextPage}>
            <button type="button" class={ui.secondaryButton} disabled={schedules.isFetchingNextPage} onClick={() => void schedules.fetchNextPage()}>Load more schedules</button>
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

      <Show when={promoting() && deployment.data}>
        {(record) => (
          <PromoteDeploymentModal
            deployment={record()}
            projectID={projectID()}
            environmentID={environmentID()}
            environmentName={globalScope.selectedEnvironment()?.name}
            onClose={() => setPromoting(false)}
          />
        )}
      </Show>

      <Show when={slackAgent()} keyed>{(agentName) => <AgentConnectionModal agentName={agentName} projectID={projectID()} environmentID={environmentID()} onClose={() => setSlackAgent(undefined)} />}</Show>

    </section>
  );
}
