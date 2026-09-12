import { useParams, useSearchParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { deploymentHref } from "../features/deployments/navigation";
import { runHref } from "../features/runs/navigation";
import { ApiError } from "../lib/api";
import { getMe, hasPermission } from "../lib/auth";
import { listRuns } from "../lib/runs";
import { useScope } from "../lib/scope";
import {
  closeSession,
  getSession,
  getSessionInput,
  getSessionOutput,
  interleaveSessionRecords,
  sendSessionInput,
  type ConversationEntry,
  type SessionAddress,
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
import { cx, ui } from "../ui/styles";

const pageSize = 100;
const pollInterval = 5_000;

function searchParamValue(value: string | string[] | undefined): string {
  return typeof value === "string" ? value.trim() : "";
}

function sessionErrorMessage(error: unknown, fallback = "Could not load this Session."): string {
  if (error instanceof ApiError && error.code === "forbidden") return "You do not have permission to do this.";
  if (error instanceof ApiError) return error.message;
  return fallback;
}

function ConversationRecord(props: { entry: ConversationEntry; projectID: string; environmentID: string }) {
  const input = () => (props.entry.direction === "input" ? props.entry.record : undefined);
  const output = () => (props.entry.direction === "output" ? props.entry.record : undefined);
  return (
    <li class="border-b border-console-border-soft px-3 py-2.5 last:border-b-0">
      <div class="flex flex-wrap items-center gap-2 font-mono text-[10.5px] text-console-subtle">
        <span
          class={cx(
            "inline-flex items-center gap-1 rounded-xs border px-1.5 font-medium leading-normal",
            props.entry.direction === "input"
              ? "border-[#9bb9e8] bg-[#eef4ff] text-console-info"
              : "border-console-border bg-console-bg-panel text-console-muted",
          )}
        >
          <span aria-hidden="true">{props.entry.direction === "input" ? "→" : "←"}</span>
          {props.entry.direction === "input" ? "Input" : "Output"}
        </span>
        <span>#{props.entry.record.sequence}</span>
        <Show when={input()}>
          {(record) => (
            <span class="inline-flex items-center gap-1">
              · source {record().source.type}
              <Show when={record().source.run_id}>
                {(runID) => <IDText value={runID()} mode="link" href={runHref(runID(), props.projectID, props.environmentID)} />}
              </Show>
            </span>
          )}
        </Show>
        <Show when={output()}>
          {(record) => (
            <span class="inline-flex items-center gap-1">
              · run <IDText value={record().provenance.run_id} mode="link" href={runHref(record().provenance.run_id, props.projectID, props.environmentID)} />
              · attempt {record().provenance.attempt_number}
              · deployment <IDText value={record().provenance.deployment_id} mode="link" href={deploymentHref(record().provenance.deployment_id)} />
            </span>
          )}
        </Show>
        <span class="ml-auto"><RelativeTime value={props.entry.record.created_at} /></span>
      </div>
      <pre class="my-2 whitespace-pre-wrap break-words font-mono text-[12px] text-console-text">
        {JSON.stringify(props.entry.record.data, null, 2)}
      </pre>
    </li>
  );
}

export function SessionDetail() {
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
    refetchInterval: (query) => (query.state.data?.status === "open" ? pollInterval : false),
  }));
  const open = () => session.data?.status === "open";
  const inputs = createInfiniteQuery(() => ({
    queryKey: ["session-inputs", sessionID(), projectID(), environmentID()],
    queryFn: ({ pageParam }) => getSessionInput(address(), { after: pageParam, limit: pageSize }),
    initialPageParam: undefined as number | undefined,
    getNextPageParam: (page) => (page.has_more ? page.next_after : undefined),
    enabled: enabled(),
    retry: false,
    refetchInterval: open() ? pollInterval : false,
  }));
  const outputs = createInfiniteQuery(() => ({
    queryKey: ["session-outputs", sessionID(), projectID(), environmentID()],
    queryFn: ({ pageParam }) => getSessionOutput(address(), { after: pageParam, limit: pageSize }),
    initialPageParam: undefined as number | undefined,
    getNextPageParam: (page) => (page.has_more ? page.next_after : undefined),
    enabled: enabled(),
    retry: false,
    refetchInterval: open() ? pollInterval : false,
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
    refetchInterval: open() ? pollInterval : false,
  }));

  // Polling replaces the page arrays but structural sharing keeps unchanged
  // record objects, so reusing one entry wrapper per record keeps the rendered
  // rows stable across refetches instead of rebuilding the list.
  const entryCache = new WeakMap<object, ConversationEntry>();
  const cached = (entry: ConversationEntry): ConversationEntry => {
    const existing = entryCache.get(entry.record);
    if (existing) return existing;
    entryCache.set(entry.record, entry);
    return entry;
  };
  const entries = createMemo(() => interleaveSessionRecords(
    inputs.data?.pages.flatMap((page) => page.records) ?? [],
    outputs.data?.pages.flatMap((page) => page.records) ?? [],
  ).map(cached));
  const hasMoreRecords = () => inputs.hasNextPage || outputs.hasNextPage;
  const fetchingMoreRecords = () => inputs.isFetchingNextPage || outputs.isFetchingNextPage;
  const loadMoreRecords = async () => {
    await Promise.all([
      inputs.hasNextPage ? inputs.fetchNextPage() : Promise.resolve(),
      outputs.hasNextPage ? outputs.fetchNextPage() : Promise.resolve(),
    ]);
  };
  const runs = createMemo(() => history.data?.pages.flatMap((page) => page.runs) ?? []);
  const optionalRunHref = (runID: string | undefined) => (runID ? runHref(runID, projectID(), environmentID()) : undefined);

  const [closing, setClosing] = createSignal(false);
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
      await sendSessionInput(address(), { input: parsed, idempotency_key: key });
      setDraft("{}");
      setDraftKey(null);
      await Promise.all([session.refetch(), inputs.refetch()]);
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
                  <Panel title="Conversation">
                    <Show when={inputs.isError}>
                      <StatePanel error={sessionErrorMessage(inputs.error, "Could not load the input log.")} />
                    </Show>
                    <Show when={outputs.isError}>
                      <StatePanel error={sessionErrorMessage(outputs.error, "Could not load the output log.")} />
                    </Show>
                    <Show when={!inputs.isPending && !outputs.isPending} fallback={<StatePanel loading="Loading conversation..." />}>
                      <Show
                        when={entries().length > 0}
                        fallback={<StatePanel empty="No input or output yet." hint="Input sent to the Session and output produced by its Runs appear here in order." />}
                      >
                        <ol class="m-0 list-none border border-console-border p-0">
                          <For each={entries()}>
                            {(entry) => <ConversationRecord entry={entry} projectID={projectID()} environmentID={environmentID()} />}
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

                    <Show when={can("sessions.input.send")}>
                      <form class="mt-4 border-t border-console-border pt-4" onSubmit={submitInput}>
                        <label class={ui.field}>
                          <span>Send input (JSON)</span>
                          <textarea
                            class={`${ui.textarea} font-mono`}
                            rows={4}
                            value={draft()}
                            disabled={!open() || sending()}
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
                            <span class={ui.muted}>Only an open Session accepts input.</span>
                          </Show>
                          <button type="submit" class={ui.button} disabled={!open() || sending()}>
                            {sending() ? "Sending..." : "Send input"}
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
            await closeSession(address(), { idempotency_key: key });
            setCloseKey(null);
            await Promise.all([
              queryClient.invalidateQueries({ queryKey: ["session", sessionID()] }),
              queryClient.invalidateQueries({ queryKey: ["sessions"] }),
            ]);
          }}
          errorMessage={(error) => sessionErrorMessage(error, "Could not close this Session.")}
        >
          The Session stops accepting input and shows as closed once its current Run finishes. Session <IDText value={sessionID()} />.
        </ConfirmModal>
      </Show>
    </section>
  );
}
