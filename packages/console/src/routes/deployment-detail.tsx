import { A, useParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show, type JSX } from "solid-js";
import { shortDigest } from "../features/deployments/display";
import { formatRelative } from "../features/runs/display";
import { ApiError } from "../lib/api";
import { getCurrentDeployment, getDeployment, getDeploymentEvents } from "../lib/deployments";
import { listSchedules, type Schedule } from "../lib/schedules";
import { useScope } from "../lib/scope";
import { listActors, listSandboxes, listTasks, type DefinitionListItem } from "../lib/definitions";
import { cx, statusBadgeClass, ui } from "../ui/styles";

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

function scheduleTone(schedule: Schedule): "active" | "expired" | "revoked" {
  if (schedule.status === "active") return "active";
  if (schedule.status === "errored") return "revoked";
  return "expired";
}

function dateCell(value: string | undefined): JSX.Element {
  return value ? formatRelative(value) : <span class="text-console-faint">—</span>;
}

function DefinitionTable(props: {
  label: string;
  items: DefinitionListItem[] | undefined;
  pending: boolean;
  error: unknown;
  history?: boolean;
}) {
  return (
    <Show when={!props.pending} fallback={<p class={ui.muted}>Loading {props.label.toLowerCase()}...</p>}>
      <Show when={!props.error} fallback={<p class={ui.error} role="alert">{errorMessage(props.error, `Could not load ${props.label.toLowerCase()}.`)}</p>}>
        <Show
          when={(props.items?.length ?? 0) > 0}
          fallback={<p class={ui.emptyState}>This Deployment declares no {props.label.toLowerCase()}.</p>}
        >
          <div class={ui.tableWrap}>
            <table class="min-w-120">
              <thead>
                <tr>
                  <th>{props.label.replace(/e?s$/, "")}</th>
                  <Show when={props.history}><th>History</th></Show>
                </tr>
              </thead>
              <tbody>
                <For each={props.items}>
                  {(item) => (
                    <tr>
                      <td><strong class="font-medium text-console-text">{item.id}</strong></td>
                      <Show when={props.history}>
                        <td>
                          <A href="/runs" class="font-mono text-[11.5px] text-console-accent hover:text-console-accent-hover">Runs</A>
                        </td>
                      </Show>
                    </tr>
                  )}
                </For>
              </tbody>
            </table>
          </div>
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
      <A href="/deployments" class={ui.backLink}>Deployments</A>
      <div class={ui.pageHeader}>
        <div>
          <div class={ui.pageTitle}>
            <h1 class={ui.h1}>{deployment.data?.version ?? "Deployment"}</h1>
            <Show when={isCurrent()}>
              <span class={statusBadgeClass("active")}>current</span>
            </Show>
          </div>
          <Show when={deployment.data}>
            {(record) => (
              <p class={cx(ui.pageSubtitle, "flex flex-wrap gap-x-4 gap-y-1")}>
                <span>Created <strong class="font-medium text-console-text">{formatRelative(record().created_at)}</strong></span>
                <span>Bundle <code class="font-mono text-[11.5px]" title={record().bundle_digest}>{shortDigest(record().bundle_digest)}</code></span>
                <span>ID <code class="font-mono text-[11.5px]">{record().id}</code></span>
              </p>
            )}
          </Show>
        </div>
      </div>

      <Show when={deployment.isError}>
        <p class={ui.error} role="alert">{errorMessage(deployment.error, "Could not load this Deployment.")}</p>
      </Show>
      <Show when={deployment.isPending && enabled()}>
        <p class={ui.muted}>Loading Deployment...</p>
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
          <DefinitionTable label="Tasks" items={tasks.data?.tasks} pending={tasks.isPending} error={tasks.error} history />
        </Show>
        <Show when={tab() === "actors"}>
          <DefinitionTable label="Actors" items={actors.data?.actors} pending={actors.isPending} error={actors.error} />
        </Show>
        <Show when={tab() === "sandboxes"}>
          <DefinitionTable label="Sandboxes" items={sandboxes.data?.sandboxes} pending={sandboxes.isPending} error={sandboxes.error} />
        </Show>

        <Show when={tab() === "schedules"}>
          <Show
            when={isCurrent()}
            fallback={
              <p class={ui.emptyState}>
                Schedules are reconciled from the environment's current Deployment. Promote this Deployment to see its schedules take effect.
              </p>
            }
          >
            <Show when={schedules.isError}>
              <p class={ui.error} role="alert">{errorMessage(schedules.error, "Could not load schedules.")}</p>
            </Show>
            <Show when={!schedules.isPending} fallback={<p class={ui.muted}>Loading schedules...</p>}>
              <Show when={scheduleItems().length > 0} fallback={<p class={ui.emptyState}>This Deployment declares no schedules.</p>}>
                <div class={ui.tableWrap}>
                  <table class="min-w-250">
                    <thead>
                      <tr>
                        <th>Task</th>
                        <th>Status</th>
                        <th>Cron</th>
                        <th>Timezone</th>
                        <th>Next</th>
                        <th>Last</th>
                        <th>Generation</th>
                        <th>ID</th>
                      </tr>
                    </thead>
                    <tbody>
                      <For each={scheduleItems()}>
                        {(schedule) => (
                          <tr class={ui.detailTableRow}>
                            <td><strong>{schedule.task_id}</strong></td>
                            <td>
                              <div class={ui.tableCellStack}>
                                <span class={statusBadgeClass(scheduleTone(schedule))}>{schedule.status}</span>
                                <Show when={schedule.last_failure}>
                                  {(failure) => <span class={ui.muted}>{failure().message}</span>}
                                </Show>
                              </div>
                            </td>
                            <td><code>{schedule.cron.pattern}</code></td>
                            <td><span class={ui.muted}>{schedule.cron.timezone}</span></td>
                            <td>{dateCell(schedule.next_fire_at)}</td>
                            <td>{dateCell(schedule.last_fire_at)}</td>
                            <td>{schedule.generation}</td>
                            <td><code>{schedule.id}</code></td>
                          </tr>
                        )}
                      </For>
                    </tbody>
                  </table>
                </div>
              </Show>
            </Show>
          </Show>
        </Show>

        <Show when={tab() === "events"}>
          <Show when={events.isError}>
            <p class={ui.error} role="alert">{errorMessage(events.error, "Could not load Deployment events.")}</p>
          </Show>
          <Show when={!events.isPending} fallback={<p class={ui.muted}>Loading events...</p>}>
            <Show when={eventItems().length > 0} fallback={<p class={ui.emptyState}>No events recorded for this Deployment.</p>}>
              <div class={ui.tableWrap}>
                <table class="min-w-180">
                  <thead>
                    <tr>
                      <th>Kind</th>
                      <th>Severity</th>
                      <th>Message</th>
                      <th>At</th>
                    </tr>
                  </thead>
                  <tbody>
                    <For each={eventItems()}>
                      {(event) => (
                        <tr>
                          <td><code>{event.kind}</code></td>
                          <td><span class={ui.muted}>{event.severity}</span></td>
                          <td>{event.message}</td>
                          <td><span class={ui.muted} title={event.at}>{formatRelative(event.at)}</span></td>
                        </tr>
                      )}
                    </For>
                  </tbody>
                </table>
              </div>
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
