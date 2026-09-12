import { A, useParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { StartDefinitionModal, type StartKind } from "../features/deployments/StartDefinitionModal";
import { ApiError } from "../lib/api";
import { getMe, hasPermission } from "../lib/auth";
import { getCurrentDeployment, getDeployment, getDeploymentEvents } from "../lib/deployments";
import { listSchedules } from "../lib/schedules";
import { useScope } from "../lib/scope";
import { listActors, listSandboxes, listTasks, type DefinitionListItem } from "../lib/definitions";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { RelativeTime } from "../ui/RelativeTime";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { cx, ui } from "../ui/styles";

const TABS = ["tasks", "actors", "sandboxes", "schedules", "events"] as const;
type Tab = (typeof TABS)[number];

const TAB_LABELS: Record<Tab, string> = {
  tasks: "Tasks",
  actors: "Actors",
  sandboxes: "Sandboxes",
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
  items: DefinitionListItem[] | undefined;
  pending: boolean;
  error: unknown;
  history?: boolean;
  onStart?: ((id: string) => void) | undefined;
}) {
  const noun = () => props.label.toLowerCase();
  const columns = () => [
    props.label.replace(/e?s$/, ""),
    ...(props.history ? ["History"] : []),
    ...(props.onStart ? [{ label: "Actions", srOnly: true }] : []),
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
                  <td><strong class="font-medium text-console-text">{item.id}</strong></td>
                  <Show when={props.history}>
                    <td>
                      <A href="/runs" class="font-mono text-[11.5px] text-console-accent hover:text-console-accent-hover">Runs</A>
                    </td>
                  </Show>
                  <Show when={props.onStart}>
                    {(onStart) => (
                      <td class={ui.actionsCell}>
                        <button type="button" class={ui.button} onClick={() => onStart()(item.id)}>Start</button>
                      </td>
                    )}
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
  const scope = useScope();
  const deploymentID = createMemo(() => params["deployment_id"]?.trim() ?? "");
  const projectID = () => scope.selectedProjectID();
  const environmentID = () => scope.selectedEnvironmentID();
  const enabled = () => !!deploymentID() && !!projectID() && !!environmentID();
  const resourceScope = () => ({ projectID: projectID(), environmentID: environmentID() });
  const [tab, setTab] = createSignal<Tab>("tasks");
  const [starting, setStarting] = createSignal<{ kind: StartKind; id: string } | null>(null);
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
  }));
  const isCurrent = createMemo(() => !!current.data && current.data.id === deploymentID());
  // Start always targets the current Deployment, so the controls only appear there.
  const startTask = createMemo(() => isCurrent() && hasPermission(me.data, "runs.create")
    ? (id: string) => setStarting({ kind: "task", id })
    : undefined);
  const startActor = createMemo(() => isCurrent() && hasPermission(me.data, "actors.start")
    ? (id: string) => setStarting({ kind: "actor", id })
    : undefined);

  const definitionOptions = () => ({ ...resourceScope(), deploymentID: deploymentID(), limit: 100 });
  const tasks = createQuery(() => ({
    queryKey: ["tasks", deploymentID(), projectID(), environmentID()],
    queryFn: () => listTasks(definitionOptions()),
    enabled: enabled() && tab() === "tasks",
    retry: false,
  }));
  const actors = createQuery(() => ({
    queryKey: ["actors", deploymentID(), projectID(), environmentID()],
    queryFn: () => listActors(definitionOptions()),
    enabled: enabled() && tab() === "actors",
    retry: false,
  }));
  const sandboxes = createQuery(() => ({
    queryKey: ["sandboxes", deploymentID(), projectID(), environmentID()],
    queryFn: () => listSandboxes(definitionOptions()),
    enabled: enabled() && tab() === "sandboxes",
    retry: false,
  }));
  const schedules = createQuery(() => ({
    queryKey: ["schedules", projectID(), environmentID()],
    queryFn: () => listSchedules(resourceScope()),
    enabled: enabled() && tab() === "schedules" && isCurrent(),
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
  const scheduleItems = createMemo(() => schedules.data?.schedules ?? []);

  return (
    <section class={ui.page}>
      <PageHeader
        title={deployment.data?.version ?? "Deployment"}
        back={{ href: "/deployments", label: "Deployments" }}
        badge={<Show when={isCurrent()}><StatusBadge resource="deployment" status="current" /></Show>}
        subtitle={
          <Show when={deployment.data}>
            {(record) => (
              <span class="flex flex-wrap gap-x-4 gap-y-1">
                <span>Created <RelativeTime value={record().created_at} /></span>
                <span>Bundle <IDText value={record().bundle_digest} /></span>
                <span>ID <IDText value={record().id} full /></span>
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

        <Show when={tab() === "tasks"}>
          <DefinitionTable label="Tasks" items={tasks.data?.tasks} pending={tasks.isPending} error={tasks.error} history onStart={startTask()} />
        </Show>
        <Show when={tab() === "actors"}>
          <DefinitionTable label="Actors" items={actors.data?.actors} pending={actors.isPending} error={actors.error} onStart={startActor()} />
        </Show>
        <Show when={tab() === "sandboxes"}>
          <DefinitionTable label="Sandboxes" items={sandboxes.data?.sandboxes} pending={sandboxes.isPending} error={sandboxes.error} />
        </Show>

        <Show when={tab() === "schedules"}>
          <Show
            when={isCurrent()}
            fallback={<StatePanel empty="Schedules follow the current Deployment." hint="Promote this Deployment to see its schedules take effect." />}
          >
            <Show when={schedules.isError}>
              <StatePanel error={errorMessage(schedules.error, "Could not load schedules.")} />
            </Show>
            <Show when={!schedules.isPending} fallback={<StatePanel loading="Loading schedules..." />}>
              <Show when={scheduleItems().length > 0} fallback={<StatePanel empty="This Deployment declares no schedules." />}>
                <DataTable columns={["Task", "Status", "Last failure", "Cron", "Timezone", "Next", "Last", "Generation", "ID"]} minWidth="min-w-250">
                  <For each={scheduleItems()}>
                    {(schedule) => (
                      <tr>
                        <td><strong class="font-medium text-console-text">{schedule.task_id}</strong></td>
                        <td><StatusBadge resource="schedule" status={schedule.status} /></td>
                        <td>
                          <Show when={schedule.last_failure} fallback={<span class="text-console-faint">—</span>}>
                            {(failure) => <span class={ui.muted} title={failure().code}>{failure().message}</span>}
                          </Show>
                        </td>
                        <td><code>{schedule.cron.pattern}</code></td>
                        <td><span class={ui.muted}>{schedule.cron.timezone}</span></td>
                        <td><RelativeTime value={schedule.next_fire_at} /></td>
                        <td><RelativeTime value={schedule.last_fire_at} /></td>
                        <td>{schedule.generation}</td>
                        <td><IDText value={schedule.id} /></td>
                      </tr>
                    )}
                  </For>
                </DataTable>
              </Show>
            </Show>
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

      <Show when={starting()}>
        {(target) => (
          <StartDefinitionModal
            kind={target().kind}
            definitionID={target().id}
            projectID={projectID()}
            environmentID={environmentID()}
            onClose={() => setStarting(null)}
          />
        )}
      </Show>
    </section>
  );
}
