import { A, useParams, useSearchParams } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, Show } from "solid-js";
import { formatRelative, StatusBadge } from "../features/runs/display";
import { ApiError } from "../lib/api";
import { actorRunConsolePath } from "../lib/actors";
import {
  getRun,
  getRunEvents,
  getRunLogs,
  type RunEventPage,
  type RunLogPage,
  type RunLogRecord,
} from "../lib/runs";
import { useScope } from "../lib/scope";
import { cx, ui } from "../ui/styles";

const pageSize = 200;

function runErrorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  return "Could not load this Run.";
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

function timeLabel(value: string | undefined): string {
  if (!value) return "—";
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? "—" : parsed.toLocaleString();
}

function searchParamValue(value: string | string[] | undefined): string {
  return typeof value === "string" ? value.trim() : "";
}

function JSONPanel(props: { title: string; value: unknown }) {
  return (
    <section class="border border-console-border bg-console-surface p-4">
      <h2 class={cx(ui.h2, "mb-3")}>{props.title}</h2>
      <pre class="m-0 max-h-130 overflow-auto whitespace-pre-wrap break-words border border-console-border bg-console-bg-panel px-4 py-3 font-mono text-[12px] leading-normal text-console-text">
        {JSON.stringify(props.value, null, 2)}
      </pre>
    </section>
  );
}

export function RunDetail() {
  const params = useParams();
  const [searchParams] = useSearchParams();
  const scope = useScope();
  const runID = createMemo(() => params["run_id"]?.trim() ?? "");
  const projectID = createMemo(() => searchParamValue(searchParams["project_id"]) || scope.selectedProjectID());
  const environmentID = createMemo(() => searchParamValue(searchParams["environment_id"]) || scope.selectedEnvironmentID());
  const enabled = createMemo(() => !!runID() && !!projectID() && !!environmentID());

  const run = createQuery(() => ({
    queryKey: ["run", runID(), projectID(), environmentID()],
    queryFn: () => getRun(runID(), projectID(), environmentID()),
    enabled: enabled(),
    retry: false,
  }));
  const initialLogs = createQuery(() => ({
    queryKey: ["run-logs", runID(), projectID(), environmentID()],
    queryFn: () => getRunLogs(runID(), projectID(), environmentID(), { limit: pageSize }),
    enabled: enabled(),
    retry: false,
  }));
  const initialEvents = createQuery(() => ({
    queryKey: ["run-events", runID(), projectID(), environmentID()],
    queryFn: () => getRunEvents(runID(), projectID(), environmentID(), { limit: pageSize }),
    enabled: enabled(),
    retry: false,
  }));
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
      <div class={ui.pageHeader}>
        <div>
          <A href="/runs" class={ui.backLink}>Runs</A>
          <div class={ui.pageTitle}>
            <h1 class={ui.h1}>{run.data?.entrypoint.id ?? "Run"}</h1>
            <Show when={run.data}>{(current) => <StatusBadge status={current().status} />}</Show>
          </div>
          <Show when={run.data}>
            {(current) => <p class="mt-1.5 font-mono text-[12px] text-console-muted">{current().id}</p>}
          </Show>
        </div>
      </div>

      <Show when={run.isError}>
        <p class={ui.error} role="alert">{runErrorMessage(run.error)}</p>
      </Show>
      <Show when={enabled()} fallback={<p class={ui.error}>Run ID and environment scope are required.</p>}>
        <Show when={!run.isPending} fallback={<p class={ui.muted}>Loading Run...</p>}>
          <Show when={run.data}>
            {(current) => (
              <div class="grid grid-cols-[minmax(0,1fr)_300px] items-start gap-3.5 max-[960px]:grid-cols-1">
                <div class="flex min-w-0 flex-col gap-3">
                  <Show when={current().output !== undefined}>
                    <JSONPanel title="Output" value={current().output} />
                  </Show>
                  <Show when={current().error}>
                    {(error) => <JSONPanel title="Run error" value={error()} />}
                  </Show>

                  <section class="border border-console-border bg-console-surface p-4">
                    <h2 class={cx(ui.h2, "mb-3")}>Events</h2>
                    <Show when={!initialEvents.isPending} fallback={<p class={ui.muted}>Loading events...</p>}>
                      <Show when={events().length > 0} fallback={<p class={ui.emptyState}>No events.</p>}>
                        <ol class="m-0 list-none border border-console-border p-0">
                          <For each={events()}>
                            {(event) => (
                              <li class="grid grid-cols-[110px_1fr] gap-3 border-b border-console-border-soft px-3 py-2.5 last:border-b-0">
                                <time class="font-mono text-[10.5px] text-console-subtle" datetime={event.at}>
                                  {formatRelative(event.at)}
                                </time>
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
                  </section>

                  <section class="border border-console-border bg-console-surface p-4">
                    <h2 class={cx(ui.h2, "mb-3")}>Logs</h2>
                    <Show when={!initialLogs.isPending} fallback={<p class={ui.muted}>Loading logs...</p>}>
                      <Show when={logs().length > 0} fallback={<p class={ui.emptyState}>No logs.</p>}>
                        <ol class="m-0 list-none border border-console-border p-0">
                          <For each={logs()}>
                            {(record) => (
                              <li class="grid grid-cols-[110px_1fr] gap-3 border-b border-console-border-soft px-3 py-2.5 last:border-b-0">
                                <time class="font-mono text-[10.5px] text-console-subtle" datetime={record.at}>
                                  {formatRelative(record.at)}
                                </time>
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
                  </section>
                  <Show when={pageError()}>
                    {(message) => <p class={ui.error} role="alert">{message()}</p>}
                  </Show>
                </div>

                <aside class="sticky top-13.5 flex flex-col gap-3 max-[960px]:static">
                  <section class="border border-console-border bg-console-surface px-4 py-3.5">
                    <h3 class={cx(ui.h3, "mb-3.5")}>Run details</h3>
                    <dl class="m-0 grid gap-2.5 [&>div]:grid [&>div]:gap-0.75 [&_dt]:font-mono [&_dt]:text-[10px] [&_dt]:uppercase [&_dt]:text-console-subtle [&_dd]:m-0 [&_dd]:break-words [&_dd]:text-[12px]">
                      <div><dt>ID</dt><dd><code>{current().id}</code></dd></div>
                      <div><dt>Entrypoint</dt><dd>{current().entrypoint.kind} · {current().entrypoint.id}</dd></div>
                      <Show when={actorRunConsolePath(current(), projectID(), environmentID())}>
                        {(actorPath) => <div>
                          <dt>Actor</dt>
                          <dd>
                            <A
                              class="text-console-accent"
                              href={actorPath()}
                            >
                              {current().actor_id}
                            </A>
                          </dd>
                        </div>}
                      </Show>
                      <div>
                        <dt>Workspace</dt>
                        <dd><A class="text-console-accent" href={`/workspaces/${current().workspace_id}`}>{current().workspace_id}</A></dd>
                      </div>
                      <div><dt>Deployment</dt><dd>{current().deployment.id} · {current().deployment.version}</dd></div>
                      <div><dt>Attempt</dt><dd>{current().current_attempt_number}</dd></div>
                      <div><dt>Created</dt><dd title={timeLabel(current().created_at)}>{formatRelative(current().created_at)}</dd></div>
                      <div><dt>Started</dt><dd>{formatRelative(current().started_at)}</dd></div>
                      <div><dt>Terminal</dt><dd>{formatRelative(current().terminal_at)}</dd></div>
                      <div><dt>Cause</dt><dd>{current().cause.type}</dd></div>
                    </dl>
                  </section>
                </aside>
              </div>
            )}
          </Show>
        </Show>
      </Show>
    </section>
  );
}
