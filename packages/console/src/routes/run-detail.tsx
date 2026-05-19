import { A, useParams } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, Match, Show, Switch } from "solid-js";
import { formatRelative, StatusBadge } from "../features/runs/display";
import { ApiError } from "../lib/api";
import {
  approveWaitpoint,
  createWaitpointResponseToken,
  denyWaitpoint,
  getRun,
  getRunLogs,
  replyToWaitpoint,
  type LogSnapshot,
  type PendingWait,
} from "../lib/runs";
import { cx, statusBadgeClass, ui } from "../ui/styles";

function runErrorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  return "Could not load this run.";
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

function PendingWaitPanel(props: { runID: string; wait: PendingWait }) {
  const queryClient = useQueryClient();
  const [busy, setBusy] = createSignal<"approve" | "deny" | "reply" | null>(null);
  const [linkBusy, setLinkBusy] = createSignal(false);
  const [responseLink, setResponseLink] = createSignal<string | null>(null);
  const [reason, setReason] = createSignal("");
  const [reply, setReply] = createSignal("");
  const [error, setError] = createSignal<string | null>(null);

  async function refresh() {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ["run", props.runID] }),
      queryClient.invalidateQueries({ queryKey: ["runs"] }),
    ]);
  }

  async function resolve(action: "approve" | "deny" | "reply") {
    setError(null);
    setBusy(action);
    try {
      if (action === "approve") await approveWaitpoint(props.runID, props.wait.waitpoint_id, reason().trim());
      if (action === "deny") await denyWaitpoint(props.runID, props.wait.waitpoint_id, reason().trim());
      if (action === "reply") await replyToWaitpoint(props.runID, props.wait.waitpoint_id, reply());
      await refresh();
    } catch (resolveError) {
      setError(runErrorMessage(resolveError));
    } finally {
      setBusy(null);
    }
  }

  async function createLink() {
    setError(null);
    setLinkBusy(true);
    try {
      const token = await createWaitpointResponseToken(props.runID, props.wait.waitpoint_id, props.wait.kind);
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
      <p class="text-[12.5px] leading-normal">{props.wait.message ?? props.wait.prompt ?? "Waiting for input."}</p>
      <p class="mt-1.5 text-[12.5px] text-console-muted">
        Requested {formatRelative(props.wait.requested_at)}
      </p>
      <div class={cx(ui.actionRow, "mt-2.5")}>
        <button class={ui.secondaryButton} type="button" disabled={linkBusy()} onClick={createLink}>
          {linkBusy() ? "Creating…" : "Create confirmation link"}
        </button>
      </div>
      <Show when={responseLink()}>
        {(link) => (
          <p class="mt-2 break-all border border-console-border bg-white px-2.5 py-2 font-mono text-[12px] text-console-text">
            {link()}
          </p>
        )}
      </Show>

      <Switch>
        <Match when={props.wait.kind === "approval"}>
          <label class={cx(ui.field, "mt-3.5")}>
            <span>Reason (optional)</span>
            <input class={ui.input} value={reason()} onInput={(event) => setReason(event.currentTarget.value)} />
          </label>
          <div class={cx(ui.actionRow, "mt-2.5")}>
            <button class={ui.button} type="button" disabled={busy() !== null} onClick={() => resolve("approve")}>
              {busy() === "approve" ? "Approving…" : "Approve"}
            </button>
            <button
              type="button"
              class={ui.dangerButton}
              disabled={busy() !== null}
              onClick={() => resolve("deny")}
            >
              {busy() === "deny" ? "Denying…" : "Deny"}
            </button>
          </div>
        </Match>
        <Match when={props.wait.kind === "message"}>
          <label class={cx(ui.field, "mt-3.5")}>
            <span>Message</span>
            <textarea class={ui.textarea} value={reply()} onInput={(event) => setReply(event.currentTarget.value)} />
          </label>
          <button
            class={ui.button}
            type="button"
            disabled={busy() !== null || reply().trim() === ""}
            onClick={() => resolve("reply")}
          >
            {busy() === "reply" ? "Sending…" : "Send"}
          </button>
        </Match>
      </Switch>

      <Show when={error()}>
        <p class={ui.error}>{error()}</p>
      </Show>
    </section>
  );
}

export function RunDetail() {
  const params = useParams();
  const runID = createMemo(() => params["id"] ?? "");
  const run = createQuery(() => ({
    queryKey: ["run", runID()],
    queryFn: () => getRun(runID()),
    enabled: runID() !== "",
    retry: false,
  }));
  const logs = createQuery(() => ({
    queryKey: ["run-logs", runID()],
    queryFn: () => getRunLogs(runID()),
    enabled: runID() !== "",
    retry: false,
  }));

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

      <Show when={!run.isPending} fallback={<p class={ui.muted}>Loading run…</p>}>
        <Show when={run.data}>
          {(current) => (
            <div class={"grid grid-cols-[minmax(0,1fr)_300px] items-start gap-3.5 max-[960px]:grid-cols-1"}>
              <div class={"flex min-w-0 flex-col gap-3"}>
                <Show when={current().pending_wait}>
                  {(wait) => <PendingWaitPanel runID={current().id} wait={wait()} />}
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
                  </dl>
                </section>
              </aside>
            </div>
          )}
        </Show>
      </Show>
    </section>
  );
}
