import { A, useParams, useSearchParams } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, Show } from "solid-js";
import { deploymentHref } from "../features/deployments/navigation";
import { ApiError } from "../lib/api";
import { getMe, hasPermission } from "../lib/auth";
import { runSessionConsolePath } from "../lib/sessions";
import {
  cancelRun,
  getRun,
  getRunEvents,
  getRunLogs,
  isTerminalRunStatus,
  type RunEventPage,
  type RunLogPage,
  type RunLogRecord,
} from "../lib/runs";
import { useScope } from "../lib/scope";
import { ConfirmModal } from "../ui/ConfirmModal";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { DetailItem, DetailList, Panel } from "../ui/Panel";
import { RelativeTime } from "../ui/RelativeTime";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { cx, ui } from "../ui/styles";

const pageSize = 200;
const pollInterval = 5_000;

function runErrorMessage(error: unknown, fallback = "Could not load this Run."): string {
  if (error instanceof ApiError && error.code === "forbidden") return "You do not have permission to do this.";
  if (error instanceof ApiError) return error.message;
  return fallback;
}

function decodeBase64(value: string): string {
  if (!value) return "";
  const binary = atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) {
    bytes[index] = binary.charCodeAt(index);
  }
  return new TextDecoder().decode(bytes);
}

function logText(record: RunLogRecord): string {
  if (record.kind === "structured") {
    const attributes = record.attributes === undefined ? "" : ` ${JSON.stringify(record.attributes)}`;
    return `${record.level ?? "info"} ${record.message ?? ""}${attributes}`;
  }
  return decodeBase64(record.content_base64 ?? "");
}

function searchParamValue(value: string | string[] | undefined): string {
  return typeof value === "string" ? value.trim() : "";
}

function JSONPanel(props: { title: string; value: unknown }) {
  return (
    <Panel title={props.title}>
      <pre class="m-0 max-h-130 overflow-auto whitespace-pre-wrap break-words border border-console-border bg-console-bg-panel px-4 py-3 font-mono text-[12px] leading-normal text-console-text">
        {JSON.stringify(props.value, null, 2)}
      </pre>
    </Panel>
  );
}

export function RunDetail() {
  const params = useParams();
  const [searchParams] = useSearchParams();
  const scope = useScope();
  const queryClient = useQueryClient();
  const runID = createMemo(() => params["run_id"]?.trim() ?? "");
  const projectID = createMemo(() => searchParamValue(searchParams["project_id"]) || scope.selectedProjectID());
  const environmentID = createMemo(() => searchParamValue(searchParams["environment_id"]) || scope.selectedEnvironmentID());
  const enabled = createMemo(() => !!runID() && !!projectID() && !!environmentID());

  const me = createQuery(() => ({ queryKey: ["me"], queryFn: getMe, retry: false, staleTime: 60_000 }));
  const can = (permission: string) => hasPermission(me.data, permission);

  const run = createQuery(() => ({
    queryKey: ["run", runID(), projectID(), environmentID()],
    queryFn: () => getRun(runID(), projectID(), environmentID()),
    enabled: enabled(),
    retry: false,
    refetchInterval: (query) => (query.state.data && !isTerminalRunStatus(query.state.data.status) ? pollInterval : false),
  }));
  const live = () => !!run.data && !isTerminalRunStatus(run.data.status);
  const sessionPath = createMemo(() => (run.data ? runSessionConsolePath(run.data, projectID(), environmentID()) : undefined));
  const initialLogs = createQuery(() => ({
    queryKey: ["run-logs", runID(), projectID(), environmentID()],
    queryFn: () => getRunLogs(runID(), projectID(), environmentID(), { limit: pageSize }),
    enabled: enabled(),
    retry: false,
    refetchInterval: live() ? pollInterval : false,
  }));
  const initialEvents = createQuery(() => ({
    queryKey: ["run-events", runID(), projectID(), environmentID()],
    queryFn: () => getRunEvents(runID(), projectID(), environmentID(), { limit: pageSize }),
    enabled: enabled(),
    retry: false,
    refetchInterval: live() ? pollInterval : false,
  }));
  const [cancelling, setCancelling] = createSignal(false);
  const [logPages, setLogPages] = createSignal<RunLogPage[]>([]);
  const [eventPages, setEventPages] = createSignal<RunEventPage[]>([]);
  const [loadingLogs, setLoadingLogs] = createSignal(false);
  const [loadingEvents, setLoadingEvents] = createSignal(false);
  const [pageError, setPageError] = createSignal<string | null>(null);

  createEffect(() => {
    runID();
    setLogPages([]);
    setEventPages([]);
    setPageError(null);
  });

  const logs = createMemo(() => [
    ...(initialLogs.data?.logs ?? []),
    ...logPages().flatMap((page) => page.logs),
  ]);
  const events = createMemo(() => [
    ...(initialEvents.data?.events ?? []),
    ...eventPages().flatMap((page) => page.events),
  ]);
  const nextLogCursor = createMemo(() => {
    const pages = logPages();
    return pages.length > 0
      ? pages[pages.length - 1]?.next_cursor
      : initialLogs.data?.next_cursor;
  });
  const nextEventCursor = createMemo(() => {
    const pages = eventPages();
    return pages.length > 0
      ? pages[pages.length - 1]?.next_cursor
      : initialEvents.data?.next_cursor;
  });

  async function loadMoreLogs() {
    const cursor = nextLogCursor();
    if (!cursor || loadingLogs()) return;
    setLoadingLogs(true);
    setPageError(null);
    try {
      const page = await getRunLogs(runID(), projectID(), environmentID(), { cursor, limit: pageSize });
      setLogPages((pages) => [...pages, page]);
    } catch (error) {
      setPageError(runErrorMessage(error));
    } finally {
      setLoadingLogs(false);
    }
  }

  async function loadMoreEvents() {
    const cursor = nextEventCursor();
    if (!cursor || loadingEvents()) return;
    setLoadingEvents(true);
    setPageError(null);
    try {
      const page = await getRunEvents(runID(), projectID(), environmentID(), { cursor, limit: pageSize });
      setEventPages((pages) => [...pages, page]);
    } catch (error) {
      setPageError(runErrorMessage(error));
    } finally {
      setLoadingEvents(false);
    }
  }

  return (
    <section class={ui.page}>
      <PageHeader
        title={run.data?.entrypoint.id ?? "Run"}
        back={{ href: "/runs", label: "Runs" }}
        badge={<Show when={run.data}>{(current) => <StatusBadge resource="run" status={current().status} />}</Show>}
        subtitle={<Show when={run.data}>{(current) => <IDText value={current().id} full />}</Show>}
        actions={
          <Show when={can("runs.manage") && live() && run.data?.status !== "cancel_requested"}>
            <button type="button" class={ui.dangerOutlineButton} onClick={() => setCancelling(true)}>Cancel Run</button>
          </Show>
        }
      />

      <Show when={run.isError}>
        <StatePanel error={runErrorMessage(run.error)} />
      </Show>
      <Show when={enabled()} fallback={<StatePanel error="Run ID and environment scope are required." />}>
        <Show when={!run.isPending} fallback={<StatePanel loading="Loading Run..." />}>
          <Show when={run.data}>
            {(current) => (
              <div class="grid grid-cols-[minmax(0,1fr)_300px] items-start gap-3.5 max-[960px]:grid-cols-1">
                <div class="flex min-w-0 flex-col gap-3">
                  <Show when={sessionPath()}>
                    {(path) => (
                      <div class="flex flex-wrap items-center gap-2 border border-[#9bb9e8] bg-[#eef4ff] px-3 py-2 text-[12.5px] text-console-text">
                        <span>
                          Actor Run of Session <IDText value={current().session_id ?? ""} href={path()} />
                        </span>
                        <span class={ui.muted}>· cause {current().cause.type} · attempt {current().current_attempt_number}</span>
                        <A class={cx(ui.secondaryButton, "ml-auto")} href={path()}>Open Session</A>
                      </div>
                    )}
                  </Show>
                  <Show when={current().output !== undefined}>
                    <JSONPanel title="Output" value={current().output} />
                  </Show>
                  <Show when={current().failure}>
                    {(failure) => <JSONPanel title="Run failure" value={failure()} />}
                  </Show>

                  <Panel title="Events">
                    <Show when={!initialEvents.isPending} fallback={<StatePanel loading="Loading events..." />}>
                      <Show when={events().length > 0} fallback={<StatePanel empty="No events." />}>
                        <ol class="m-0 list-none border border-console-border p-0">
                          <For each={events()}>
                            {(event) => (
                              <li class="grid grid-cols-[110px_1fr] gap-3 border-b border-console-border-soft px-3 py-2.5 last:border-b-0">
                                <span class="font-mono text-[10.5px]"><RelativeTime value={event.at} /></span>
                                <div class="min-w-0">
                                  <div class="font-mono text-[11px] text-console-subtle">
                                    {event.severity} · {event.source} · {event.kind}
                                  </div>
                                  <div class="mt-1 whitespace-pre-wrap break-words text-[12px] text-console-text">
                                    {event.message}
                                  </div>
                                </div>
                              </li>
                            )}
                          </For>
                        </ol>
                      </Show>
                      <Show when={nextEventCursor()}>
                        <button class={ui.secondaryButton} disabled={loadingEvents()} onClick={loadMoreEvents}>
                          {loadingEvents() ? "Loading..." : "Load more events"}
                        </button>
                      </Show>
                    </Show>
                  </Panel>

                  <Panel title="Logs">
                    <Show when={!initialLogs.isPending} fallback={<StatePanel loading="Loading logs..." />}>
                      <Show when={logs().length > 0} fallback={<StatePanel empty="No logs." />}>
                        <ol class="m-0 list-none border border-console-border p-0">
                          <For each={logs()}>
                            {(record) => (
                              <li class="grid grid-cols-[110px_1fr] gap-3 border-b border-console-border-soft px-3 py-2.5 last:border-b-0">
                                <span class="font-mono text-[10.5px]"><RelativeTime value={record.at} /></span>
                                <pre class="m-0 whitespace-pre-wrap break-words font-mono text-[12px] text-console-text">
                                  <span class="text-console-subtle">{record.kind} · attempt {record.attempt_number}</span>{"\n"}
                                  {logText(record)}
                                </pre>
                              </li>
                            )}
                          </For>
                        </ol>
                      </Show>
                      <Show when={nextLogCursor()}>
                        <button class={ui.secondaryButton} disabled={loadingLogs()} onClick={loadMoreLogs}>
                          {loadingLogs() ? "Loading..." : "Load more logs"}
                        </button>
                      </Show>
                    </Show>
                  </Panel>
                  <Show when={pageError()}>
                    {(message) => <StatePanel error={message()} />}
                  </Show>
                </div>

                <DetailList title="Run details">
                  <DetailItem label="ID"><IDText value={current().id} full /></DetailItem>
                  <DetailItem label="Entrypoint">{current().entrypoint.kind} · {current().entrypoint.id}</DetailItem>
                  <Show when={sessionPath()}>
                    {(path) => (
                      <DetailItem label="Session">
                        <IDText value={current().session_id ?? ""} full href={path()} />
                      </DetailItem>
                    )}
                  </Show>
                  <DetailItem label="Workspace">
                    <IDText value={current().workspace_id} full href={`/workspaces/${current().workspace_id}`} />
                  </DetailItem>
                  <DetailItem label="Deployment">
                    <A class="text-console-accent" href={deploymentHref(current().deployment.id)}>{current().deployment.version}</A>
                  </DetailItem>
                  <DetailItem label="Attempt">{current().current_attempt_number}</DetailItem>
                  <DetailItem label="Created"><RelativeTime value={current().created_at} /></DetailItem>
                  <DetailItem label="Started"><RelativeTime value={current().started_at} /></DetailItem>
                  <DetailItem label="Terminal"><RelativeTime value={current().terminal_at} /></DetailItem>
                  <DetailItem label="Cause">{current().cause.type}</DetailItem>
                </DetailList>
              </div>
            )}
          </Show>
        </Show>
      </Show>

      <Show when={cancelling() && run.data}>
        {(current) => (
          <ConfirmModal
            title="Cancel Run"
            confirmLabel="Cancel Run"
            busyLabel="Cancelling..."
            tone="danger"
            onClose={() => setCancelling(false)}
            onConfirm={async () => {
              await cancelRun(current().id, projectID(), environmentID());
              await queryClient.invalidateQueries({ queryKey: ["run", current().id] });
              await queryClient.invalidateQueries({ queryKey: ["runs"] });
            }}
            errorMessage={(error) => runErrorMessage(error, "Could not cancel this Run.")}
          >
            Cancellation is requested for <strong>{current().entrypoint.id}</strong> (<IDText value={current().id} />). A running attempt stops at its next checkpoint.
          </ConfirmModal>
        )}
      </Show>
    </section>
  );
}
