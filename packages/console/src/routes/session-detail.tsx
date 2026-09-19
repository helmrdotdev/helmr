import { useParams, useSearchParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery, useQueryClient } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, on, Show } from "solid-js";
import { deploymentHref } from "../features/deployments/navigation";
import { runHref } from "../features/runs/navigation";
import { ApiError } from "../lib/api";
import { getMe, hasPermission } from "../lib/auth";
import { listRuns } from "../lib/runs";
import { useScope } from "../lib/scope";
import {
  closeSession, getSession, getSessionEvents, getSessionTurn,
  sendSession, sendTurnMessage, interruptTurn, resumeSession,
  type SessionEvent, type SessionAddress, type SessionReceipt,
} from "../lib/sessions";
import { ConfirmModal } from "../ui/ConfirmModal";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { DetailItem, DetailList, Panel } from "../ui/Panel";
import { RelativeTime } from "../ui/RelativeTime";
import { SectionHeader } from "../ui/SectionHeader";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";

const pageSize = 100;
const pollInterval = 5_000;

function searchParamValue(value: string | string[] | undefined): string {
  return typeof value === "string" ? value.trim() : "";
}

function sessionErrorMessage(error: unknown, fallback = "Could not load this Session."): string {
  if (error instanceof ApiError && error.code === "forbidden") return "You do not have permission to do this.";
  if (error instanceof ApiError) {
    if (error.message === "turn_not_active") return "This Turn is no longer active. The message was not sent to another Turn.";
    if (error.message === "hold_mismatch") return "The pause has changed. Review the current Session before resuming.";
    return error.message;
  }
  return fallback;
}

function ConversationRecord(props: { entry: SessionEvent; projectID: string; environmentID: string; onTurn: (id: string) => void }) {
  return <li class="border-b border-console-border-soft px-3 py-2.5 last:border-b-0">
    <div class="flex flex-wrap items-center gap-2 font-mono text-[10.5px] text-console-subtle">
      <span>{props.entry.kind}</span><span>#{props.entry.sequence}</span>
      <Show when={props.entry.turn_id}>{(id) => <button class="text-console-info underline" onClick={() => props.onTurn(id())}>Turn {id()}</button>}</Show>
      <Show when={props.entry.provenance}>{(provenance) => <IDText value={provenance().run_id} mode="link" href={runHref(provenance().run_id, props.projectID, props.environmentID)} />}</Show>
      <span class="ml-auto"><RelativeTime value={props.entry.created_at} /></span>
    </div>
    <pre class="my-2 whitespace-pre-wrap break-words font-mono text-[12px] text-console-text">{JSON.stringify(props.entry.data, null, 2)}</pre>
  </li>;
}

export function SessionDetail() {
  const params = useParams();
  const [search] = useSearchParams();
  const scope = useScope();
  const identity = () => JSON.stringify([params["session_id"], searchParamValue(search["project_id"]) || scope.selectedProjectID(), searchParamValue(search["environment_id"]) || scope.selectedEnvironmentID()]);
  return <Show when={identity()} keyed>{(_identity) => <SessionDetailContent />}</Show>;
}

function SessionDetailContent() {
  const params = useParams();
  const [searchParams] = useSearchParams();
  const scope = useScope();
  const queryClient = useQueryClient();
  const sessionID = createMemo(() => params["session_id"]?.trim() ?? "");
  const projectID = createMemo(() => searchParamValue(searchParams["project_id"]) || scope.selectedProjectID());
  const environmentID = createMemo(() => searchParamValue(searchParams["environment_id"]) || scope.selectedEnvironmentID());
  const enabled = createMemo(() => !!sessionID() && !!projectID() && !!environmentID());
  const address = (): SessionAddress => ({
    sessionID: sessionID(),
    projectID: projectID(),
    environmentID: environmentID(),
  });

  const me = createQuery(() => ({ queryKey: ["me"], queryFn: getMe, retry: false, staleTime: 60_000 }));
  const can = (permission: string) => hasPermission(me.data, permission);

  const session = createQuery(() => ({
    queryKey: ["session", sessionID(), projectID(), environmentID()],
    queryFn: () => getSession(address()),
    enabled: enabled(),
    retry: false,
    refetchInterval: (query) => (["open", "closing"].includes(query.state.data?.status ?? "") ? pollInterval : false),
  }));
  const open = () => session.data?.status === "open";
  const polling = () => session.data?.status === "open" || session.data?.status === "closing";
  const events = createInfiniteQuery(() => ({
    queryKey: ["session-events", sessionID(), projectID(), environmentID()],
    queryFn: ({ pageParam }) => getSessionEvents(address(), { after: pageParam, limit: pageSize }),
    initialPageParam: 0,
    getNextPageParam: (page) => page.has_more ? page.next_after : undefined,
    enabled: enabled(), retry: false, refetchInterval: polling() ? pollInterval : false,
  }));
  const [selectedTurn, setSelectedTurn] = createSignal<string>();
  const [lastActiveTurn, setLastActiveTurn] = createSignal<string>();
  createEffect(on(() => session.data?.active_turn_id, (id) => { if (id) setLastActiveTurn(id); }));
  const turnID = () => selectedTurn() ?? session.data?.active_turn_id ?? lastActiveTurn();
  const turn = createQuery(() => ({
    queryKey: ["session-turn", sessionID(), projectID(), environmentID(), turnID()],
    queryFn: () => getSessionTurn(address(), turnID()!),
    enabled: enabled() && !!turnID(), retry: false,
    refetchInterval: polling() ? pollInterval : false,
  }));
  const history = createInfiniteQuery(() => ({
    queryKey: ["runs", "session", sessionID(), projectID(), environmentID()],
    queryFn: ({ pageParam }) => listRuns({
      projectID: projectID(),
      environmentID: environmentID(),
      sessionID: sessionID(),
      cursor: pageParam || undefined,
      limit: 100,
    }),
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor,
    enabled: enabled(),
    retry: false,
    refetchInterval: polling() ? pollInterval : false,
  }));

  // A terminal Session can arrive after the last independent event/Turn poll.
  // Read those projections once more after observing the terminal commit.
  createEffect(on(() => session.data?.status, (status, previous) => {
    if (previous && (status === "closed" || status === "failed")) {
      void events.refetch();
      void history.refetch();
      if (turnID()) void turn.refetch();
    }
  }));
  const entries = createMemo(() => events.data?.pages.flatMap((page) => page.records) ?? []);
  const hasMoreRecords = () => events.hasNextPage;
  const fetchingMoreRecords = () => events.isFetchingNextPage;
  const loadMoreRecords = () => events.fetchNextPage();
  const runs = createMemo(() => history.data?.pages.flatMap((page) => page.runs) ?? []);
  const optionalRunHref = (runID: string | null | undefined) => (runID ? runHref(runID, projectID(), environmentID()) : undefined);

  const [closing, setClosing] = createSignal(false);
  const [receipt, setReceipt] = createSignal<SessionReceipt>();
  const [mode, setMode] = createSignal<"send" | "enqueue" | "message">("send");
  const [messageTarget, setMessageTarget] = createSignal<string>();
  const [control, setControl] = createSignal<{ kind: "interrupt" | "resume"; target: string; key: string }>();
  const refresh = () => Promise.all([session.refetch(), events.refetch(), ...(turnID() ? [turn.refetch()] : [])]);
  const [draft, setDraft] = createSignal("{}");
  const [sending, setSending] = createSignal(false);
  const [sendError, setSendError] = createSignal<string | null>(null);
  // One idempotency key per draft: a retry of the same text after a timeout
  // reuses it, and it is regenerated only when the text changes or the send
  // succeeds.
  const [draftKey, setDraftKey] = createSignal<string | null>(null);
  const [closeKey, setCloseKey] = createSignal<string | null>(null);

  const submitInput = async (event: Event) => {
    event.preventDefault();
    let parsed: unknown;
    try {
      parsed = JSON.parse(draft());
    } catch {
      setSendError("Input must be valid JSON.");
      return;
    }
    const key = draftKey() ?? crypto.randomUUID();
    setDraftKey(key);
    setSendError(null);
    setSending(true);
    try {
      const input = { data: parsed, idempotency_key: key };
      const target = messageTarget();
      if (mode() === "message" && !target) throw new Error("Select an exact Turn before sending a message.");
      const result = mode() === "message"
        ? await sendTurnMessage(address(), target!, input)
        : await sendSession(address(), input, mode() as "send" | "enqueue");
      setReceipt(result);
      setDraft("{}");
      setDraftKey(null);
      await refresh();
    } catch (error) {
      setSendError(sessionErrorMessage(error, "Could not send this input."));
    } finally {
      setSending(false);
    }
  };

  return (
    <section class={ui.page}>
      <PageHeader
        title={session.data?.actor_id ?? "Session"}
        back={{ href: "/sessions", label: "Sessions" }}
        badge={<Show when={session.data}>{(current) => <StatusBadge resource="session" status={current().status} />}</Show>}
        subtitle={<IDText value={sessionID()} mode="full" />}
        actions={
          <Show when={can("sessions.close") && open()}>
            <button type="button" class={ui.dangerOutlineButton} onClick={() => setClosing(true)}>Close Session</button>
          </Show>
        }
      />

      <Show when={session.isError}>
        <StatePanel error={sessionErrorMessage(session.error)} />
      </Show>
      <Show when={enabled()} fallback={<StatePanel error="Session ID and environment scope are required." />}>
        <Show when={!session.isPending} fallback={<StatePanel loading="Loading Session..." />}>
          <Show when={session.data}>
            {(current) => (
              <div class="grid grid-cols-[minmax(0,1fr)_300px] items-start gap-3.5 max-[960px]:grid-cols-1">
                <div class="flex min-w-0 flex-col gap-6">
                  <Show when={receipt()}>{(value) => <Panel title="Operation receipt"><p class={ui.muted}>Accepted operations are not necessarily finished. Follow the Turn and timeline for the outcome.</p><pre class="whitespace-pre-wrap break-words">{JSON.stringify(value(), null, 2)}</pre></Panel>}</Show>
                  <Panel title="Turn">
                    <Show when={turnID()} fallback={<p class={ui.muted}>No active Turn. Select a Turn from the timeline to inspect its outcome.</p>}>
                      <p class="break-all">{turnID()}</p>
                      <Show when={selectedTurn()}><button class={ui.secondaryButton} onClick={() => setSelectedTurn(undefined)}>Follow active Turn</button></Show>
                      <Show when={turn.isError}><StatePanel error={sessionErrorMessage(turn.error, "Could not load this Turn.")} /></Show>
                      <Show when={turn.data}>{(value) => <>
                        <p>Turn status: {value().status}</p>
                        <p>Messages: {value().accepts_messages ? "Ready" : "Not accepting"}</p>
                        <Show when={value().interrupt_requested && value().status === "running"}><p>Interruption requested; waiting for execution to stop.</p></Show>
                        <Show when={value().result !== undefined}><pre class="whitespace-pre-wrap break-words">Result: {JSON.stringify(value().result, null, 2)}</pre></Show>
                        <Show when={value().error !== undefined}><pre class="whitespace-pre-wrap break-words">Turn error: {JSON.stringify(value().error, null, 2)}</pre></Show>
                        <Show when={can("sessions.interrupt") && value().id === session.data?.active_turn_id && !value().interrupt_requested}>
                          <button class={ui.dangerOutlineButton} onClick={() => setControl({ kind: "interrupt", target: value().id, key: crypto.randomUUID() })}>Interrupt Turn</button>
                        </Show>
                      </>}</Show>
                    </Show>
                  </Panel>
                  <Panel title="Event timeline">
                    <Show when={events.isError}><StatePanel error={sessionErrorMessage(events.error, "Could not load the event timeline.")} /></Show>
                    <Show when={(events.data?.pages[0]?.retained_after ?? 0) > 0}><p class={ui.muted}>Earlier events are no longer retained. Showing events after #{events.data?.pages[0]?.retained_after}.</p></Show>
                    <Show when={!events.isPending} fallback={<StatePanel loading="Loading timeline..." />}>
                      <Show
                        when={entries().length > 0}
                        fallback={<StatePanel empty="No events yet." hint="Work, messages, output and lifecycle events share one durable sequence." />}
                      >
                        <ol class="m-0 list-none border border-console-border p-0">
                          <For each={entries()}>
                            {(entry) => <ConversationRecord entry={entry} projectID={projectID()} environmentID={environmentID()} onTurn={(id) => { setSelectedTurn(id); }} />}
                          </For>
                        </ol>
                      </Show>
                      <Show when={hasMoreRecords()}>
                        <div class={ui.actionRow}>
                          <button
                            type="button"
                            class={ui.secondaryButton}
                            disabled={fetchingMoreRecords()}
                            onClick={() => void loadMoreRecords()}
                          >
                            {fetchingMoreRecords() ? "Loading..." : "Load more"}
                          </button>
                        </div>
                      </Show>
                    </Show>

                    <Show when={can("sessions.send")}>
                      <form class="mt-4 border-t border-console-border pt-4" onSubmit={submitInput}>
                        <label class={ui.field}>
                          <span>Application data (JSON)</span>
                          <select class={ui.input} aria-label="Send mode" value={mode()} disabled={sending()} onChange={(event) => {
                            const next = event.currentTarget.value as "send" | "enqueue" | "message";
                            setMode(next); setMessageTarget(next === "message" ? turnID() : undefined); setDraftKey(null);
                          }}>
                            <option value="send">Send to Session</option><option value="enqueue">Queue new Turn</option>
                            <option value="message" disabled={!turnID()}>Message selected Turn</option>
                          </select>
                          <Show when={mode() === "message"}><span>Target Turn: {messageTarget()}</span></Show>
                          <textarea
                            class={`${ui.textarea} font-mono`}
                            rows={4}
                            value={draft()}
                            disabled={sending() || (mode() === "message" ? !polling() || !messageTarget() : !open())}
                            onInput={(event) => {
                              setDraft(event.currentTarget.value);
                              setDraftKey(null);
                            }}
                            spellcheck={false}
                          />
                        </label>
                        <Show when={sendError()}>
                          <p class={ui.fieldError} role="alert">{sendError()}</p>
                        </Show>
                        <div class={ui.actionRow}>
                          <Show when={!open()}>
                            <span class={ui.muted}>New work requires an open Session. Existing Turn interaction may continue while closing.</span>
                          </Show>
                          <button type="submit" class={ui.button} disabled={sending() || (mode() === "message" ? !polling() || !messageTarget() : !open())}>
                            {sending() ? "Sending..." : mode() === "enqueue" ? "Queue Turn" : "Send"}
                          </button>
                        </div>
                      </form>
                    </Show>
                  </Panel>

                  <section class="min-w-0">
                    <SectionHeader title="Execution history" count={runs().length} subtitle="Runs that served this Session, newest first." />
                    <Show when={history.isError}>
                      <StatePanel error={sessionErrorMessage(history.error, "Could not load the Session's Runs.")} />
                    </Show>
                    <Show when={!history.isPending} fallback={<StatePanel loading="Loading Runs..." />}>
                      <Show when={runs().length > 0} fallback={<StatePanel empty="No Runs yet." />}>
                        <DataTable columns={["Run", "Status", "Attempt", "Created", "Terminal"]}>
                          <For each={runs()}>
                            {(run) => (
                              <tr>
                                <td><IDText value={run.id} mode="link" href={runHref(run.id, projectID(), environmentID())} /></td>
                                <td><StatusBadge resource="run" status={run.status} /></td>
                                <td>{run.current_attempt_number}</td>
                                <td><RelativeTime value={run.created_at} /></td>
                                <td><RelativeTime value={run.terminal_at} /></td>
                              </tr>
                            )}
                          </For>
                        </DataTable>
                        <Show when={history.hasNextPage}>
                          <div class={ui.actionRow}>
                            <button
                              type="button"
                              class={ui.secondaryButton}
                              disabled={history.isFetchingNextPage}
                              onClick={() => void history.fetchNextPage()}
                            >
                              {history.isFetchingNextPage ? "Loading..." : "Load more"}
                            </button>
                          </div>
                        </Show>
                      </Show>
                    </Show>
                  </section>
                </div>

                <DetailList title="Session details">
                  <DetailItem label="Actor ID"><code>{current().actor_id}</code></DetailItem>
                  <DetailItem label="Key">
                    <Show when={current().key} fallback={<span class="text-console-faint">—</span>}>
                      {(key) => <code>{key()}</code>}
                    </Show>
                  </DetailItem>
                  <DetailItem label="Status"><StatusBadge resource="session" status={current().status} /></DetailItem>
                  <DetailItem label="Dispatch">{current().dispatch.state}</DetailItem>
                  <Show when={current().dispatch.hold_id}>{(hold) => <>
                    <DetailItem label="Waiting">{current().dispatch.reason === "recovery_required" ? "Recovery required" : current().dispatch.reason === "interrupt_requested" ? "Stopping" : "Paused"}</DetailItem>
                    <DetailItem label="Hold"><code class="break-all">{hold()}</code></DetailItem>
                    <Show when={current().dispatch.reason === "recovery_required"} fallback={
                      <Show when={can("sessions.resume") && current().dispatch.reason !== "interrupt_requested"}><button class={ui.button} onClick={() => setControl({ kind: "resume", target: hold(), key: crypto.randomUUID() })}>Resume queued work</button></Show>
                    }><p class={ui.muted}>An owner or admin must reconcile external effects and the Workspace version using actor recover before queued work can resume.</p></Show>
                  </>}</Show>
                  <DetailItem label="Deployment">
                    <IDText value={current().deployment_id} mode="link" href={deploymentHref(current().deployment_id)} />
                  </DetailItem>
                  <DetailItem label="Workspace">
                    <IDText
                      value={current().workspace_id ?? ""}
                      mode="link"
                      href={current().workspace_id ? `/workspaces/${current().workspace_id}` : undefined}
                    />
                  </DetailItem>
                  <DetailItem label="Current Run">
                    <IDText value={current().current_run_id ?? ""} mode="link" href={optionalRunHref(current().current_run_id)} />
                  </DetailItem>
                  <Show when={current().failure}>
                    {(failure) => (
                      <>
                        <DetailItem label="Failure">
                          <span class="text-console-danger">{failure().code}</span>
                          <Show when={failure().message}>{(message) => <span class="block text-console-muted">{message()}</span>}</Show>
                        </DetailItem>
                        <DetailItem label="Failing Run">
                          <IDText value={failure().details.run_id ?? ""} mode="link" href={optionalRunHref(failure().details.run_id)} />
                        </DetailItem>
                      </>
                    )}
                  </Show>
                  <DetailItem label="Created"><RelativeTime value={current().created_at} /></DetailItem>
                  <DetailItem label="Updated"><RelativeTime value={current().updated_at} /></DetailItem>
                </DetailList>
              </div>
            )}
          </Show>
        </Show>
      </Show>

      <Show when={control()}>{(action) => <ConfirmModal
        title={action().kind === "interrupt" ? "Interrupt Turn" : "Resume queued work"}
        confirmLabel={action().kind === "interrupt" ? "Interrupt" : "Resume"}
        onClose={() => setControl(undefined)}
        errorMessage={(error) => sessionErrorMessage(error, "Operation failed.")}
        onConfirm={async () => {
          const exact = action();
          setReceipt(exact.kind === "interrupt"
            ? await interruptTurn(address(), exact.target, { idempotency_key: exact.key })
            : await resumeSession(address(), { hold_id: exact.target, idempotency_key: exact.key }));
          await refresh();
        }}>
        <p>{action().kind === "interrupt" ? "Stop this Turn and retain queued work. The receipt acknowledges the request, not completed interruption." : "Resume queued work after this pause. The interrupted Turn is not restarted."}</p>
        <code class="break-all">{action().target}</code>
      </ConfirmModal>}</Show>

      <Show when={closing()}>
        <ConfirmModal
          title="Close Session"
          confirmLabel="Close Session"
          busyLabel="Closing..."
          tone="danger"
          onClose={() => setClosing(false)}
          onConfirm={async () => {
            const key = closeKey() ?? crypto.randomUUID();
            setCloseKey(key);
            setReceipt(await closeSession(address(), { idempotency_key: key }));
            setCloseKey(null);
            await Promise.all([
              queryClient.invalidateQueries({ queryKey: ["session", sessionID()] }),
              queryClient.invalidateQueries({ queryKey: ["sessions"] }),
            ]);
          }}
          errorMessage={(error) => sessionErrorMessage(error, "Could not close this Session.")}
        >
          The Session stops accepting new work and drains already queued Turns. Existing holds remain and require resolution. Closing does not interrupt the current Turn. Session <IDText value={sessionID()} />.
        </ConfirmModal>
      </Show>
    </section>
  );
}
