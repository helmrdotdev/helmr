import { A, useParams, useSearchParams } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, Show } from "solid-js";
import { formatRelative } from "../features/runs/display";
import { runHref } from "../features/runs/navigation";
import { SessionStatusBadge } from "../features/sessions/display";
import { ApiError } from "../lib/api";
import { formatTaskOutput, hasRunOutput, taskOutputKind } from "../lib/run-output";
import {
  cancelSession,
  closeSession,
  getSession,
  listSessionRuns,
  listSessionStreamRecords,
  listSessionStreams,
  type StreamRecord,
  type Session,
  type SessionRun,
  type SessionStream,
} from "../lib/sessions";
import { useScope } from "../lib/scope";
import { cx, ui } from "../ui/styles";

type TimelineStream = {
  stream: SessionStream;
  records: StreamRecord[];
};

function sessionErrorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  return "Could not load this session.";
}

function searchParamValue(value: string | string[] | undefined): string {
  return typeof value === "string" ? value.trim() : "";
}

function formatJSON(value: unknown): string {
  return JSON.stringify(value ?? null, null, 2);
}

function shortID(id: string | undefined): string {
  return id ? id.slice(0, 8) : "—";
}

function isOpen(session: Session): boolean {
  return session.status === "open";
}

function canClose(session: Session): boolean {
  return isOpen(session) && !session.current_run_id;
}

function SessionResult(props: { session: Session }) {
  const result = createMemo(() => props.session.result ?? props.session.error ?? props.session.terminal_reason);
  return (
    <Show when={result() !== undefined}>
      <section class={"border border-console-border bg-console-surface p-4"}>
        <div class={"mb-3 flex items-center justify-between gap-3"}>
          <h2 class={ui.h2}>Result</h2>
          <span class={ui.muted}>{taskOutputKind(result())}</span>
        </div>
        <pre class={"m-0 max-h-100 overflow-auto whitespace-pre-wrap break-words border border-console-border bg-console-bg-panel px-4 py-3 font-mono text-[12px] leading-normal text-console-text"}>
          {hasRunOutput({ output: result() }) ? formatTaskOutput(result()) : formatJSON(result())}
        </pre>
      </section>
    </Show>
  );
}

function SessionRuns(props: { sessionID: string; runs: SessionRun[]; projectID: string; environmentID: string }) {
  return (
    <section class={"border border-console-border bg-console-surface p-4"}>
      <div class={"mb-3 flex items-center justify-between gap-3"}>
        <h2 class={ui.h2}>Run history</h2>
        <span class={ui.muted}>{props.runs.length} turns</span>
      </div>
      <Show
        when={props.runs.length > 0}
        fallback={<p class={ui.emptyState}>No runs have been recorded for this session.</p>}
      >
        <div class={ui.tableWrap}>
          <table class={"min-w-190"}>
            <thead>
              <tr>
                <th>Turn</th>
                <th>Run</th>
                <th>Status</th>
                <th>Execution</th>
                <th>Started</th>
                <th>Ended</th>
              </tr>
            </thead>
            <tbody>
              <For each={props.runs}>
                {(run) => (
                  <tr>
                    <td><code>{run.turn_index}</code></td>
                    <td>
                      <A class={"font-mono text-[11.5px] text-console-accent hover:text-console-accent-hover"} href={runHref(run.run_id, props.sessionID, props.projectID, props.environmentID)}>
                        {shortID(run.run_id)}
                      </A>
                    </td>
                    <td>{run.status}</td>
                    <td>{run.execution_status}</td>
                    <td><span class={ui.muted}>{formatRelative(run.created_at)}</span></td>
                    <td><span class={ui.muted}>{formatRelative(run.ended_at)}</span></td>
                  </tr>
                )}
              </For>
            </tbody>
          </table>
        </div>
      </Show>
    </section>
  );
}

function StreamTimeline(props: { streams: TimelineStream[] }) {
  const rows = createMemo(() => props.streams
    .flatMap((item) => item.records.map((record) => ({ stream: item.stream, record })))
    .sort((left, right) => left.record.created_at.localeCompare(right.record.created_at)));
  return (
    <section class={"border border-console-border bg-console-surface p-4"}>
      <div class={"mb-3 flex items-center justify-between gap-3"}>
        <h2 class={ui.h2}>Stream timeline</h2>
        <span class={ui.muted}>{rows().length} records</span>
      </div>
      <Show
        when={rows().length > 0}
        fallback={<p class={ui.emptyState}>No stream records yet.</p>}
      >
        <ol class={"m-0 grid max-h-130 list-none gap-0 overflow-auto border border-console-border bg-console-bg-panel p-0"}>
          <For each={rows()}>
            {(item) => (
              <li class={"grid grid-cols-[96px_170px_minmax(0,1fr)] gap-3 border-b border-console-border-soft px-3 py-2.5 last:border-b-0 max-[760px]:grid-cols-1 max-[760px]:gap-1.5"}>
                <time class={"font-mono text-[10.5px] leading-5 text-console-subtle"} datetime={item.record.created_at}>
                  {formatRelative(item.record.created_at)}
                </time>
                <div class={"min-w-0"}>
                  <div class={"font-mono text-[11px] font-medium text-console-text"}>
                    {item.stream.direction}:{item.stream.name}
                  </div>
                  <div class={"font-mono text-[10.5px] text-console-subtle"}>
                    seq {item.record.sequence}
                    <Show when={item.record.correlation_id}> · {item.record.correlation_id}</Show>
                  </div>
                </div>
                <pre class={"m-0 whitespace-pre-wrap break-words font-mono text-[12px] leading-normal text-console-text"}>
                  {formatJSON(item.record.data)}
                </pre>
              </li>
            )}
          </For>
        </ol>
      </Show>
    </section>
  );
}

function DetailsAside(props: { session: Session; currentRunHref: string | null }) {
  return (
    <aside class={"sticky top-13.5 flex flex-col gap-3 max-[960px]:static"}>
      <section class={"border border-console-border bg-console-surface px-4 py-3.5"}>
        <h3 class={cx(ui.h3, "mb-3.5")}>Session details</h3>
        <dl class={"m-0 grid gap-2.5 [&>div]:grid [&>div]:gap-0.75 [&_dt]:m-0 [&_dt]:font-mono [&_dt]:text-[10px] [&_dt]:font-medium [&_dt]:uppercase [&_dt]:tracking-[0.06em] [&_dt]:text-console-subtle [&_dd]:m-0 [&_dd]:[overflow-wrap:anywhere] [&_dd]:text-[12.5px] [&_dd]:text-console-text [&_dd_code]:font-mono [&_dd_code]:text-[11.5px] [&_dd_code]:text-console-text"}>
          <div>
            <dt>ID</dt>
            <dd><code>{props.session.id}</code></dd>
          </div>
          <div>
            <dt>External ID</dt>
            <dd>{props.session.external_id || "—"}</dd>
          </div>
          <div>
            <dt>Current run</dt>
            <dd>
              <Show when={props.currentRunHref} fallback="—">
                {(href) => <A class={"font-mono text-console-accent hover:text-console-accent-hover"} href={href()}>{shortID(props.session.current_run_id)}</A>}
              </Show>
            </dd>
          </div>
          <div>
            <dt>Active deployment</dt>
            <dd><code>{props.session.active_deployment_id}</code></dd>
          </div>
          <div>
            <dt>Created</dt>
            <dd>{formatRelative(props.session.created_at)}</dd>
          </div>
          <div>
            <dt>Updated</dt>
            <dd>{formatRelative(props.session.updated_at)}</dd>
          </div>
          <div>
            <dt>Expires</dt>
            <dd>{formatRelative(props.session.expires_at)}</dd>
          </div>
        </dl>
      </section>
    </aside>
  );
}

export function SessionDetail() {
  const params = useParams();
  const [searchParams] = useSearchParams();
  const scope = useScope();
  const queryClient = useQueryClient();
  const sessionID = createMemo(() => params["id"]?.trim() ?? "");
  const projectID = createMemo(() => searchParamValue(searchParams["project_id"]) || scope.selectedProjectID());
  const environmentID = createMemo(() => searchParamValue(searchParams["environment_id"]) || scope.selectedEnvironmentID());
  const hasSessionID = createMemo(() => sessionID() !== "");
  const [action, setAction] = createSignal<"close" | "cancel" | null>(null);
  const [actionError, setActionError] = createSignal<string | null>(null);
  const scopeIDs = () => ({ projectID: projectID(), environmentID: environmentID() });

  const session = createQuery(() => ({
    queryKey: ["session", sessionID(), projectID(), environmentID()],
    queryFn: () => getSession(sessionID(), scopeIDs()),
    enabled: hasSessionID() && !!projectID() && !!environmentID(),
    retry: false,
  }));
  const runs = createQuery(() => ({
    queryKey: ["session-runs", sessionID(), projectID(), environmentID()],
    queryFn: () => listSessionRuns(sessionID(), scopeIDs()),
    enabled: hasSessionID() && !!projectID() && !!environmentID(),
    retry: false,
  }));
  const timeline = createQuery(() => ({
    queryKey: ["session-streams", sessionID(), projectID(), environmentID()],
    queryFn: async (): Promise<TimelineStream[]> => {
      const streams = await listSessionStreams(sessionID(), scopeIDs());
      return Promise.all(streams.streams.map(async (stream) => ({
        stream,
        records: (await listSessionStreamRecords(sessionID(), scopeIDs(), stream, { limit: 100 })).records,
      })));
    },
    enabled: hasSessionID() && !!projectID() && !!environmentID(),
    retry: false,
  }));
  createEffect(() => {
    sessionID();
    setActionError(null);
    setAction(null);
  });

  const currentRunHref = createMemo(() => {
    const id = session.data?.current_run_id;
    if (!id || !projectID() || !environmentID()) return null;
    return runHref(id, sessionID(), projectID(), environmentID());
  });

  async function runSessionAction(kind: "close" | "cancel") {
    if (!session.data || action()) return;
    const prompt = kind === "close" ? "Close this session?" : "Cancel this session and its current run?";
    if (!window.confirm(prompt)) return;
    setAction(kind);
    setActionError(null);
    try {
      if (kind === "close") {
        await closeSession(session.data.id, scopeIDs());
      } else {
        await cancelSession(session.data.id, scopeIDs());
      }
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["session"] }),
        queryClient.invalidateQueries({ queryKey: ["session-runs"] }),
        queryClient.invalidateQueries({ queryKey: ["sessions"] }),
        queryClient.invalidateQueries({ queryKey: ["runs"] }),
      ]);
    } catch (error) {
      setActionError(sessionErrorMessage(error));
    } finally {
      setAction(null);
    }
  }

  return (
    <section class={ui.page}>
      <div class={ui.pageHeader}>
        <div>
          <A href="/sessions" class={ui.backLink}>Sessions</A>
          <div class={ui.pageTitle}>
            <h1 class={ui.h1}>{session.data?.task_id ?? "Session"}</h1>
            <Show when={session.data}>{(current) => <SessionStatusBadge status={current().status} />}</Show>
          </div>
          <Show when={session.data}>
            {(current) => (
              <p class={"mt-1.5 max-w-180 font-mono text-[12.5px] leading-normal text-console-muted"}>
                {current().id}
              </p>
            )}
          </Show>
        </div>
        <Show when={session.data && isOpen(session.data)}>
          <div class={"flex flex-wrap items-center gap-1.5"}>
            <Show when={session.data ? canClose(session.data) : false}>
              <button class={ui.secondaryButton} type="button" disabled={!!action()} onClick={() => void runSessionAction("close")}>
                {action() === "close" ? "Closing..." : "Close"}
              </button>
            </Show>
            <button class={ui.dangerOutlineButton} type="button" disabled={!!action()} onClick={() => void runSessionAction("cancel")}>
              {action() === "cancel" ? "Cancelling..." : "Cancel"}
            </button>
          </div>
        </Show>
      </div>

      <Show when={session.isError}>
        <p class={ui.error} role="alert">{sessionErrorMessage(session.error)}</p>
      </Show>
      <Show when={actionError()}>
        {(message) => <p class={ui.error} role="alert">{message()}</p>}
      </Show>

      <Show
        when={hasSessionID()}
        fallback={<p class={ui.error} role="alert">Session ID is required.</p>}
      >
        <Show when={!session.isPending} fallback={<p class={ui.muted}>Loading session...</p>}>
          <Show when={session.data}>
            {(current) => (
              <div class={"grid grid-cols-[minmax(0,1fr)_310px] items-start gap-3.5 max-[960px]:grid-cols-1"}>
                <div class={"flex min-w-0 flex-col gap-3"}>
                  <SessionResult session={current()} />
                  <Show when={runs.isError}>
                    <p class={ui.error} role="alert">{sessionErrorMessage(runs.error)}</p>
                  </Show>
                  <SessionRuns sessionID={current().id} runs={runs.data?.runs ?? []} projectID={projectID()} environmentID={environmentID()} />
                  <Show when={timeline.isError}>
                    <p class={ui.error} role="alert">{sessionErrorMessage(timeline.error)}</p>
                  </Show>
                  <Show when={!timeline.isPending} fallback={<p class={ui.muted}>Loading stream records...</p>}>
                    <StreamTimeline streams={timeline.data ?? []} />
                  </Show>
                </div>
                <DetailsAside session={current()} currentRunHref={currentRunHref()} />
              </div>
            )}
          </Show>
        </Show>
      </Show>
    </section>
  );
}
