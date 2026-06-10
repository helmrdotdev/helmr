import { A, useParams, useSearchParams } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, Show } from "solid-js";
import { formatRelative, StatusBadge } from "../features/runs/display";
import { ApiError } from "../lib/api";
import { formatTaskOutput, hasRunOutput, taskOutputKind, taskOutputRenderMode, taskOutputTable } from "../lib/run-output";
import {
  createWaitpointResponseToken,
  getRun,
  getRunEvents,
  getRunLogs,
  respondWaitpoint,
  type LogSnapshot,
  type PendingWaitpoint,
  type Run,
  type RunEventPage,
  type RunEventRecord,
  type WaitpointDelivery,
} from "../lib/runs";
import { useScope } from "../lib/scope";
import { cx, statusBadgeClass, ui } from "../ui/styles";

function runErrorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  return "Could not load this run.";
}

function parseCompletionValue(value: string): unknown {
  const trimmed = value.trim();
  if (trimmed === "") return undefined;
  return JSON.parse(trimmed);
}

function decodeBase64(value: string): string {
  if (!value) return "";
  const binary = atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) {
    bytes[i] = binary.charCodeAt(i);
  }
  return new TextDecoder().decode(bytes);
}

function lineCount(value: string): number {
  if (!value) return 0;
  const trimmed = value.endsWith("\n") ? value.slice(0, -1) : value;
  return trimmed.split("\n").length;
}

function objectValue(value: unknown): Record<string, unknown> | null {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return null;
  return value as Record<string, unknown>;
}

function stringValue(value: unknown): string | null {
  return typeof value === "string" ? value : null;
}

function numberValue(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

function eventKind(event: RunEventRecord): string {
  return event.message || event.kind;
}

function eventAttributes(event: RunEventRecord): Record<string, unknown> {
  return objectValue(event.attributes) ?? {};
}

function eventTime(value: string): string {
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return "—";
  return parsed.toLocaleString();
}

function formatJSON(value: unknown): string {
  return JSON.stringify(value, null, 2);
}

function taskLogEntry(event: RunEventRecord): { level: string; message: string } | null {
  if (eventKind(event) !== "log") return null;
  const raw = stringValue(eventAttributes(event)["message"]);
  if (raw === null) return null;
  try {
    const parsed = objectValue(JSON.parse(raw));
    if (parsed === null) return { level: "info", message: raw };
    return {
      level: stringValue(parsed["level"]) ?? "info",
      message: stringValue(parsed["message"]) ?? raw,
    };
  } catch {
    return { level: "info", message: raw };
  }
}

function eventLabel(event: RunEventRecord): string {
  const kind = eventKind(event);
  const attrs = eventAttributes(event);
  if (kind === "log") {
    return `task log:${taskLogEntry(event)?.level ?? "info"}`;
  }
  if (kind === "log.stdout" || kind === "log.stderr") {
    return kind === "log.stdout" ? "stdout" : "stderr";
  }
  if (kind.startsWith("emit.")) {
    return `emit:${stringValue(attrs["type"]) ?? kind.slice("emit.".length)}`;
  }
  if (kind === "waitpoint.requested") {
    return `wait:${stringValue(attrs["kind"]) ?? "request"}`;
  }
  if (kind === "waitpoint.resolved") {
    return `wait:${stringValue(attrs["resolution_kind"]) ?? "resolved"}`;
  }
  if (kind === "run.created") return "run:created";
  if (kind === "run.completed") return "run:completed";
  if (kind === "run.failed") return `run:${stringValue(attrs["failure_kind"]) ?? "failed"}`;
  if (kind === "run.cancelled") return "run:cancelled";
  if (kind === "checkpoint.ready") return "checkpoint:ready";
  return kind;
}

function eventSummary(event: RunEventRecord): string {
  const kind = eventKind(event);
  const attrs = eventAttributes(event);
  const logEntry = taskLogEntry(event);
  if (logEntry !== null) return logEntry.message;
  if (kind === "log.stdout" || kind === "log.stderr") {
    return `${numberValue(attrs["bytes"]) ?? 0} bytes, seq ${numberValue(attrs["observed_seq"]) ?? "?"}`;
  }
  if (kind === "emit.deploy.progress") {
    const content = objectValue(attrs["content"]);
    return stringValue(content?.["message"]) ?? "deploy progress";
  }
  if (kind.startsWith("emit.")) {
    return formatJSON(attrs["content"] ?? attrs);
  }
  if (kind === "run.created") {
    const workspace = objectValue(attrs["workspace"]);
    const repo = stringValue(workspace?.["repository"]);
    const ref = stringValue(workspace?.["ref"]) ?? stringValue(workspace?.["ref_name"]) ?? stringValue(workspace?.["sha"]);
    return [stringValue(attrs["task_id"]), repo, ref].filter(Boolean).join(" · ") || "Run queued";
  }
  if (kind === "run.completed") {
    const exitCode = numberValue(attrs["exit_code"]);
    return exitCode === null ? "Run completed" : `exit code ${exitCode}`;
  }
  if (kind === "run.failed") {
    const detail = objectValue(attrs["detail"]);
    return stringValue(detail?.["message"]) ?? stringValue(attrs["message"]) ?? "Run failed";
  }
  if (kind === "run.cancelled") return stringValue(attrs["reason"]) ?? "Run cancelled";
  if (kind === "waitpoint.requested") {
    return stringValue(attrs["display_text"]) ?? stringValue(objectValue(attrs["request"])?.["message"]) ?? "Waiting for input";
  }
  if (kind === "waitpoint.resolved") {
    const result = objectValue(attrs["result"]);
    return stringValue(attrs["reason"]) ?? stringValue(result?.["text"]) ?? stringValue(attrs["resolution_kind"]) ?? "Resolved";
  }
  if (kind === "checkpoint.ready") return "Checkpoint ready";
  return formatJSON(attrs);
}

function eventTone(kind: string): string {
  if (kind === "run.failed" || kind === "run.cancelled") return "bg-console-danger";
  if (kind === "run.completed") return "bg-console-success";
  if (kind === "waitpoint.requested") return "bg-console-warning";
  if (kind === "log" || kind.startsWith("emit.")) return "bg-console-info";
  return "bg-console-faint";
}

function defaultEventMode(events: RunEventRecord[]): "activity" | "all" {
  return events.some((event) => eventKind(event) !== "log.stdout" && eventKind(event) !== "log.stderr") ? "activity" : "all";
}

function deliveryStatusLabel(status: string): string {
  return status
    .split(/[_\s-]+/)
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1).toLowerCase())
    .join(" ") || "Unknown";
}

function deliveryStatusTone(status: string): "active" | "succeeded" | "revoked" | "expired" {
  if (status === "queued") return "active";
  if (status === "sent") return "succeeded";
  if (status === "failed") return "revoked";
  return "expired";
}

function DeliveryStatusBadge(props: { status: string }) {
  return (
    <span class={statusBadgeClass(deliveryStatusTone(props.status))}>
      {deliveryStatusLabel(props.status)}
    </span>
  );
}

function deliveryTime(delivery: WaitpointDelivery): string {
  return formatRelative(delivery.sent_at ?? delivery.updated_at ?? delivery.created_at);
}

function DeliveryTable(props: { deliveries: WaitpointDelivery[] }) {
  return (
    <div class={"mt-3 overflow-x-auto border border-console-border bg-white"}>
      <table class={"w-full min-w-150 border-separate border-spacing-0 [&_thead_th]:h-8 [&_thead_th]:border-b [&_thead_th]:border-console-border [&_thead_th]:bg-console-bg-panel [&_thead_th]:px-2.5 [&_thead_th]:py-0 [&_thead_th]:text-left [&_thead_th]:font-mono [&_thead_th]:text-[10px] [&_thead_th]:font-medium [&_thead_th]:uppercase [&_thead_th]:tracking-[0.06em] [&_thead_th]:text-console-subtle [&_tbody_td]:border-b [&_tbody_td]:border-console-border-soft [&_tbody_td]:px-2.5 [&_tbody_td]:py-2 [&_tbody_td]:align-top [&_tbody_td]:text-[12px] [&_tbody_tr:last-child_td]:border-b-0"}>
        <thead>
          <tr>
            <th>Channel</th>
            <th>Recipient</th>
            <th>Status</th>
            <th>Updated</th>
            <th>Error</th>
          </tr>
        </thead>
        <tbody>
          <For each={props.deliveries}>
            {(delivery) => (
              <tr>
                <td>{delivery.channel}</td>
                <td>{delivery.recipient ?? <span class={"text-console-faint"}>—</span>}</td>
                <td><DeliveryStatusBadge status={delivery.status} /></td>
                <td>{deliveryTime(delivery)}</td>
                <td>{delivery.last_error ?? <span class={"text-console-faint"}>—</span>}</td>
              </tr>
            )}
          </For>
        </tbody>
      </table>
    </div>
  );
}

function waitpointPolicyLabel(run: Run): string | null {
  return run.pending_waitpoint?.policy ?? null;
}

function waitpointDeliveries(run: Run): WaitpointDelivery[] {
  return run.pending_waitpoint?.deliveries ?? [];
}

function OutputPanel(props: { output: unknown }) {
  const kind = createMemo(() => taskOutputKind(props.output));
  const mode = createMemo(() => taskOutputRenderMode(props.output));
  const formatted = createMemo(() => formatTaskOutput(props.output));
  const table = createMemo(() => taskOutputTable(props.output));
  const [lastOutput, setLastOutput] = createSignal<string | null>(null);
  const [copyState, setCopyState] = createSignal<string | null>(null);

  createEffect(() => {
    const nextOutput = formatted();
    if (lastOutput() === nextOutput) return;
    setLastOutput(nextOutput);
    setCopyState(null);
  });

  async function copyOutput() {
    setCopyState(null);
    try {
      await navigator.clipboard.writeText(formatted());
      setCopyState("Copied");
    } catch {
      setCopyState("Copy failed");
    }
  }

  return (
    <section class={"mb-3 border border-console-border bg-console-surface p-4"}>
      <div class={"mb-3 flex flex-wrap items-center justify-between gap-3"}>
        <h2 class={ui.h2}>Output <span class={ui.muted}>({mode()} / {kind()})</span></h2>
        <div class={"flex flex-wrap items-center justify-end gap-2"}>
          <Show when={copyState()}>
            {(message) => <span class={ui.muted} role="status">{message()}</span>}
          </Show>
          <button class={ui.secondaryButton} type="button" onClick={copyOutput}>
            Copy
          </button>
        </div>
      </div>
      <Show
        when={table()}
        fallback={<pre class={"m-0 block max-h-130 overflow-auto whitespace-pre-wrap break-words border border-console-border bg-console-bg-panel px-4 py-3.25 font-mono text-[12.5px] leading-normal text-console-text"}>{formatted()}</pre>}
      >
        {(table) => <OutputTable table={table()} />}
      </Show>
    </section>
  );
}

function OutputTable(props: { table: { columns: string[]; rows: string[][] } }) {
  return (
    <div class={"max-h-130 overflow-auto border border-console-border bg-white"}>
      <table class={"w-full min-w-100 border-separate border-spacing-0 [&_thead_th]:sticky [&_thead_th]:top-0 [&_thead_th]:z-10 [&_thead_th]:h-8 [&_thead_th]:border-b [&_thead_th]:border-console-border [&_thead_th]:bg-console-bg-panel [&_thead_th]:px-2.5 [&_thead_th]:py-0 [&_thead_th]:text-left [&_thead_th]:font-mono [&_thead_th]:text-[10px] [&_thead_th]:font-medium [&_thead_th]:uppercase [&_thead_th]:tracking-[0.06em] [&_thead_th]:text-console-subtle [&_tbody_td]:border-b [&_tbody_td]:border-console-border-soft [&_tbody_td]:px-2.5 [&_tbody_td]:py-2 [&_tbody_td]:align-top [&_tbody_td]:font-mono [&_tbody_td]:text-[12px] [&_tbody_td]:leading-normal [&_tbody_td]:text-console-text [&_tbody_tr:last-child_td]:border-b-0"}>
        <thead>
          <tr>
            <For each={props.table.columns}>
              {(column) => <th>{column}</th>}
            </For>
          </tr>
        </thead>
        <tbody>
          <For each={props.table.rows}>
            {(row) => (
              <tr>
                <For each={row}>
                  {(cell) => <td><span class={"block max-w-90 whitespace-pre-wrap break-words"}>{cell}</span></td>}
                </For>
              </tr>
            )}
          </For>
        </tbody>
      </table>
    </div>
  );
}

function LogPane(props: { logs: LogSnapshot | undefined }) {
  const stdout = createMemo(() => decodeBase64(props.logs?.stdout_base64 ?? ""));
  const stderr = createMemo(() => decodeBase64(props.logs?.stderr_base64 ?? ""));
  const [view, setView] = createSignal<"stdout" | "stderr">("stdout");

  return (
    <section class={"mb-3 border border-console-border bg-console-surface p-4"}>
      <div class={"mb-3 flex items-center justify-between gap-3"}>
        <h2 class={ui.h2}>Logs</h2>
        <Show when={props.logs?.truncated}>
          <span class={ui.muted}>truncated</span>
        </Show>
      </div>
      <div class={"mb-3 flex items-center gap-0 border-b border-console-border"} role="tablist">
        <button
          type="button"
          class={cx(ui.logTab, view() === "stdout" && ui.logTabActive)}
          onClick={() => setView("stdout")}
        >
          stdout <span class={"font-mono text-[10.5px] font-medium text-console-subtle tabular-nums"}>{lineCount(stdout())}</span>
        </button>
        <button
          type="button"
          class={cx(ui.logTab, view() === "stderr" && ui.logTabActive)}
          onClick={() => setView("stderr")}
        >
          stderr <span class={"font-mono text-[10.5px] font-medium text-console-subtle tabular-nums"}>{lineCount(stderr())}</span>
        </button>
      </div>
      <pre class={"m-0 block min-h-60 max-h-130 overflow-auto whitespace-pre-wrap border border-console-border bg-console-bg-panel px-4 py-3.25 font-mono text-[12.5px] leading-normal text-console-text empty:before:text-console-faint empty:before:italic empty:before:content-['(no_output)']"}>{view() === "stdout" ? stdout() : stderr()}</pre>
    </section>
  );
}

function EventTimeline(props: {
  events: RunEventRecord[] | undefined;
  hasMore: boolean;
  loadingMore: boolean;
  moreError: string | null;
  onLoadMore: () => void;
}) {
  const [view, setView] = createSignal<"activity" | "all">("activity");
  const activityEvents = createMemo(() => {
    const events = props.events ?? [];
    return events.filter((event) => {
      const kind = eventKind(event);
      return kind !== "log.stdout" && kind !== "log.stderr";
    });
  });
  const visibleEvents = createMemo(() => {
    const events = props.events ?? [];
    const mode = view() === "activity" ? defaultEventMode(events) : "all";
    if (mode === "all") return events;
    return activityEvents();
  });

  return (
    <section class={"mb-3 border border-console-border bg-console-surface p-4"}>
      <div class={"mb-3 flex items-center justify-between gap-3"}>
        <h2 class={ui.h2}>Events</h2>
      </div>
      <div class={"mb-3 flex items-center gap-0 border-b border-console-border"} role="tablist">
        <button
          type="button"
          class={cx(ui.logTab, view() === "activity" && ui.logTabActive)}
          onClick={() => setView("activity")}
        >
          activity <span class={"font-mono text-[10.5px] font-medium text-console-subtle tabular-nums"}>{activityEvents().length}</span>
        </button>
        <button
          type="button"
          class={cx(ui.logTab, view() === "all" && ui.logTabActive)}
          onClick={() => setView("all")}
        >
          all <span class={"font-mono text-[10.5px] font-medium text-console-subtle tabular-nums"}>{props.events?.length ?? 0}</span>
        </button>
      </div>
      <Show
        when={visibleEvents().length > 0}
        fallback={<p class={ui.emptyState}>No events.</p>}
      >
        <>
          <ol class={"m-0 grid max-h-120 list-none gap-0 overflow-auto border border-console-border bg-console-bg-panel p-0"}>
            <For each={visibleEvents()}>
              {(event) => {
                const kind = eventKind(event);
                return (
                  <li class={"grid grid-cols-[88px_1fr] gap-3 border-b border-console-border-soft px-3 py-2.5 last:border-b-0 max-sm:grid-cols-1 max-sm:gap-1"}>
                    <time
                      class={"font-mono text-[10.5px] leading-5 text-console-subtle"}
                      datetime={event.at}
                      title={eventTime(event.at)}
                    >
                      {formatRelative(event.at)}
                    </time>
                    <div class={"min-w-0"}>
                      <div class={"flex min-w-0 items-center gap-2"}>
                        <span class={cx("size-1.5 shrink-0", eventTone(kind))} />
                        <span class={"min-w-0 truncate font-mono text-[11px] font-medium text-console-subtle"}>
                          {eventLabel(event)}
                        </span>
                      </div>
                      <pre class={"m-0 mt-1 whitespace-pre-wrap break-words font-mono text-[12px] leading-normal text-console-text"}>
                        {eventSummary(event)}
                      </pre>
                    </div>
                  </li>
                );
              }}
            </For>
          </ol>
          <Show when={props.moreError}>
            {(message) => <p class={ui.error} role="alert">{message()}</p>}
          </Show>
          <Show when={props.hasMore}>
            <div class={cx(ui.actionRow, "mt-2.5")}>
              <button class={ui.secondaryButton} type="button" disabled={props.loadingMore} onClick={props.onLoadMore}>
                {props.loadingMore ? "Loading…" : "Load more"}
              </button>
            </div>
          </Show>
        </>
      </Show>
    </section>
  );
}

function PendingWaitpointPanel(props: {
  runID: string;
  projectID: string;
  environmentID: string;
  wait: PendingWaitpoint;
  policy: string | null;
  deliveries: WaitpointDelivery[];
}) {
  const queryClient = useQueryClient();
  const [busy, setBusy] = createSignal(false);
  const [linkBusy, setLinkBusy] = createSignal(false);
  const [responseLink, setResponseLink] = createSignal<string | null>(null);
  const [value, setValue] = createSignal("");
  const [error, setError] = createSignal<string | null>(null);

  async function refresh() {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ["run", props.runID] }),
      queryClient.invalidateQueries({ queryKey: ["runs"] }),
      queryClient.invalidateQueries({ queryKey: ["run-events", props.runID] }),
    ]);
  }

  async function resolve() {
    setError(null);
    setBusy(true);
    try {
      await respondWaitpoint(props.wait.waitpoint_id, props.projectID, props.environmentID, parseCompletionValue(value()));
      await refresh();
    } catch (resolveError) {
      setError(runErrorMessage(resolveError));
    } finally {
      setBusy(false);
    }
  }

  async function createLink() {
    if (props.wait.kind === "delay") return;
    setError(null);
    setLinkBusy(true);
    try {
      const token = await createWaitpointResponseToken(props.wait.waitpoint_id, props.wait.kind, props.projectID, props.environmentID);
      setResponseLink(token.url);
    } catch (linkError) {
      setError(runErrorMessage(linkError));
    } finally {
      setLinkBusy(false);
    }
  }

  return (
    <section class={"mb-3 border border-[#e5c26e] bg-[#fffaf0] p-4"}>
      <div class={"mb-3 flex items-center justify-between gap-3"}>
        <h2 class={ui.h2}>Pending wait</h2>
        <span class={statusBadgeClass("waiting")}>{props.wait.kind}</span>
      </div>
      <p class="text-[12.5px] leading-normal">
        {props.wait.display_text ?? "Waiting for input."}
      </p>
      <p class="mt-1.5 text-[12.5px] text-console-muted">
        Requested {formatRelative(props.wait.requested_at)}
      </p>
      <Show when={props.policy}>
        {(policy) => (
          <p class="mt-1.5 text-[12.5px] text-console-muted">
            Policy <code class="font-mono text-[11.5px] text-console-text">{policy()}</code>
          </p>
        )}
      </Show>
      <Show when={props.deliveries.length > 0}>
        <DeliveryTable deliveries={props.deliveries} />
      </Show>
      <Show when={props.wait.kind !== "delay"}>
        <div class={cx(ui.actionRow, "mt-2.5")}>
          <button class={ui.secondaryButton} type="button" disabled={linkBusy()} onClick={createLink}>
            {linkBusy() ? "Creating…" : "Create confirmation link"}
          </button>
        </div>
      </Show>
      <Show when={responseLink()}>
        {(link) => (
          <p class="mt-2 break-all border border-console-border bg-white px-2.5 py-2 font-mono text-[12px] text-console-text">
            {link()}
          </p>
        )}
      </Show>

      <Show when={props.wait.kind === "human"}>
        <label class={cx(ui.field, "mt-3.5")}>
          <span>Value JSON (optional)</span>
          <textarea class={ui.textarea} value={value()} onInput={(event) => setValue(event.currentTarget.value)} />
        </label>
        <button
          class={ui.button}
          type="button"
          disabled={busy()}
          onClick={resolve}
        >
            {busy() ? "Responding…" : "Respond"}
        </button>
      </Show>

      <Show when={error()}>
        <p class={ui.error}>{error()}</p>
      </Show>
    </section>
  );
}

export function RunDetail() {
  const params = useParams();
  const [searchParams] = useSearchParams();
  const scope = useScope();
  const runID = createMemo(() => params["id"]?.trim() ?? "");
  const projectID = createMemo(() => searchParamValue(searchParams["project_id"]) || scope.selectedProjectID());
  const environmentID = createMemo(() => searchParamValue(searchParams["environment_id"]) || scope.selectedEnvironmentID());
  const hasRunID = createMemo(() => runID() !== "");
  const run = createQuery(() => ({
    queryKey: ["run", runID(), projectID(), environmentID()],
    queryFn: () => getRun(runID(), projectID(), environmentID()),
    enabled: hasRunID() && !!projectID() && !!environmentID(),
    retry: false,
  }));
  const logs = createQuery(() => ({
    queryKey: ["run-logs", runID(), projectID(), environmentID()],
    queryFn: () => getRunLogs(runID(), projectID(), environmentID()),
    enabled: hasRunID() && !!projectID() && !!environmentID(),
    retry: false,
  }));
  const eventPageSize = 200;
  const [extraEventPages, setExtraEventPages] = createSignal<RunEventPage[]>([]);
  const [eventsLoadingMore, setEventsLoadingMore] = createSignal(false);
  const [eventsMoreError, setEventsMoreError] = createSignal<string | null>(null);
  const events = createQuery(() => ({
    queryKey: ["run-events", runID(), projectID(), environmentID()],
    queryFn: () => getRunEvents(runID(), projectID(), environmentID(), { limit: eventPageSize }),
    enabled: hasRunID() && !!projectID() && !!environmentID(),
    retry: false,
  }));
  createEffect(() => {
    runID();
    setExtraEventPages([]);
    setEventsMoreError(null);
    setEventsLoadingMore(false);
  });
  createEffect(() => {
    events.data;
    setExtraEventPages([]);
    setEventsMoreError(null);
  });
  const eventRecords = createMemo(() => [
    ...(events.data?.events ?? []),
    ...extraEventPages().flatMap((page) => page.events),
  ]);
  const nextEventCursor = createMemo(() => {
    const pages = extraEventPages();
    if (pages.length > 0) return pages[pages.length - 1]?.next_cursor ?? null;
    return events.data?.next_cursor ?? null;
  });
  async function loadMoreEvents() {
    const cursor = nextEventCursor();
    if (cursor == null || eventsLoadingMore()) return;
    const requestedRunID = runID();
    setEventsMoreError(null);
    setEventsLoadingMore(true);
    try {
      const page = await getRunEvents(requestedRunID, projectID(), environmentID(), { cursor, limit: eventPageSize });
      if (runID() !== requestedRunID) return;
      setExtraEventPages((pages) => [...pages, page]);
    } catch (error) {
      if (runID() !== requestedRunID) return;
      setEventsMoreError(runErrorMessage(error));
    } finally {
      if (runID() === requestedRunID) setEventsLoadingMore(false);
    }
  }

  return (
    <section class={ui.page}>
      <div class={ui.pageHeader}>
        <div>
          <A href="/runs" class={ui.backLink}>Runs</A>
          <div class={ui.pageTitle}>
            <h1 class={ui.h1}>{run.data?.task_id ?? "Run"}</h1>
            <Show when={run.data}>{(current) => <StatusBadge status={current().status} />}</Show>
          </div>
          <Show when={run.data}>
            {(current) => (
              <p class="mt-1.5 max-w-180 font-mono text-[12.5px] leading-normal text-console-muted">
                {current().id}
              </p>
            )}
          </Show>
        </div>
      </div>

      <Show when={run.isError}>
        <p class={ui.error} role="alert">{runErrorMessage(run.error)}</p>
      </Show>

      <Show
        when={hasRunID()}
        fallback={<p class={ui.error} role="alert">Run ID is required.</p>}
      >
        <Show when={!run.isPending} fallback={<p class={ui.muted}>Loading run…</p>}>
          <Show when={run.data}>
            {(current) => (
              <div class={"grid grid-cols-[minmax(0,1fr)_300px] items-start gap-3.5 max-[960px]:grid-cols-1"}>
                <div class={"flex min-w-0 flex-col gap-3"}>
                  <Show when={current().pending_waitpoint}>
                    {(wait) => (
                      <PendingWaitpointPanel
                        runID={current().id}
                        projectID={current().project_id}
                        environmentID={current().environment_id}
                        wait={wait()}
                        policy={waitpointPolicyLabel(current())}
                        deliveries={waitpointDeliveries(current())}
                      />
                    )}
                  </Show>

                  <Show when={hasRunOutput(current())}>
                    <OutputPanel output={current().output} />
                  </Show>

                  <Show when={events.isError}>
                    <p class={ui.error} role="alert">{runErrorMessage(events.error)}</p>
                  </Show>
                  <Show when={!events.isPending} fallback={<p class={ui.muted}>Loading events…</p>}>
                    <EventTimeline
                      events={eventRecords()}
                      hasMore={nextEventCursor() !== null}
                      loadingMore={eventsLoadingMore()}
                      moreError={eventsMoreError()}
                      onLoadMore={loadMoreEvents}
                    />
                  </Show>

                  <Show when={logs.isError}>
                    <p class={ui.error} role="alert">{runErrorMessage(logs.error)}</p>
                  </Show>
                  <Show when={!logs.isPending} fallback={<p class={ui.muted}>Loading logs…</p>}>
                    <LogPane logs={logs.data} />
                  </Show>
                </div>

                <aside class={"sticky top-13.5 flex flex-col gap-3 max-[960px]:static"}>
                  <section class={"border border-console-border bg-console-surface px-4 py-3.5"}>
                    <h3 class={cx(ui.h3, "mb-3.5")}>Run details</h3>
                    <dl class={"m-0 grid gap-2.5 [&>div]:grid [&>div]:gap-0.75 [&_dt]:m-0 [&_dt]:font-mono [&_dt]:text-[10px] [&_dt]:font-medium [&_dt]:uppercase [&_dt]:tracking-[0.06em] [&_dt]:text-console-subtle [&_dd]:m-0 [&_dd]:[overflow-wrap:anywhere] [&_dd]:text-[12.5px] [&_dd]:text-console-text [&_dd_code]:font-mono [&_dd_code]:text-[11.5px] [&_dd_code]:text-console-text"}>
                      <div>
                        <dt>ID</dt>
                        <dd><code>{current().id}</code></dd>
                      </div>
                      <div>
                        <dt>Task</dt>
                        <dd>{current().task_id}</dd>
                      </div>
                      <div>
                        <dt>Created</dt>
                        <dd>{formatRelative(current().created_at)}</dd>
                      </div>
                      <div>
                        <dt>Updated</dt>
                        <dd>{formatRelative(current().updated_at)}</dd>
                      </div>
                      <div>
                        <dt>Exit code</dt>
                        <dd>{current().exit_code ?? "—"}</dd>
                      </div>
                      <div>
                        <dt>Waitpoint policy</dt>
                        <dd>{waitpointPolicyLabel(current()) ?? "—"}</dd>
                      </div>
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

function searchParamValue(value: string | string[] | undefined): string {
  return typeof value === "string" ? value.trim() : "";
}
