import { SessionDelivery } from "../features/slack/SessionDelivery";
import { SessionProgress } from "../features/asks/SessionProgress";
import { ContentView } from "../features/asks/Question";
import { TurnQuestions } from "../features/asks/TurnQuestions";
import { useParams, useSearchParams } from "@solidjs/router";
import {
  createInfiniteQuery,
  createQuery,
  useQueryClient,
} from "@tanstack/solid-query";
import {
  createEffect,
  createMemo,
  createSignal,
  For,
  on,
  Show,
} from "solid-js";
import { deploymentHref } from "../features/deployments/navigation";
import { ApiError } from "../lib/api";
import { getMe, hasPermission } from "../lib/auth";
import { useScope } from "../lib/scope";
import {
  cancelSession,
  interruptSession,
  sessionConsolePath,
  getSession,
  listSessionTurns,
  type SessionAddress,
  type SessionReceipt,
} from "../lib/sessions";
import { ConfirmModal } from "../ui/ConfirmModal";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { DetailItem, DetailList, Panel } from "../ui/Panel";
import { RelativeTime } from "../ui/RelativeTime";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";

function searchValue(value: string | string[] | undefined): string {
  return typeof value === "string" ? value.trim() : "";
}
function errorMessage(error: unknown): string {
  return error instanceof ApiError
    ? error.message
    : "The operation could not be completed.";
}

export function SessionDetail() {
  const params = useParams();
  const [search] = useSearchParams();
  const scope = useScope();
  const identity = () =>
    JSON.stringify([
      params["session_id"],
      searchValue(search["project_id"]) || scope.selectedProjectID(),
      searchValue(search["environment_id"]) || scope.selectedEnvironmentID(),
    ]);
  return (
    <Show when={identity()} keyed>
      {(_identity) => <SessionDetailContent />}
    </Show>
  );
}

function SessionDetailContent() {
  const params = useParams();
  const [search] = useSearchParams();
  const scope = useScope();
  const queryClient = useQueryClient();
  const sessionID = () => params["session_id"]?.trim() ?? "";
  const projectID = () =>
    searchValue(search["project_id"]) || scope.selectedProjectID();
  const environmentID = () =>
    searchValue(search["environment_id"]) || scope.selectedEnvironmentID();
  const address = (): SessionAddress => ({
    sessionID: sessionID(),
    projectID: projectID(),
    environmentID: environmentID(),
  });
  const enabled = () => !!sessionID() && !!projectID() && !!environmentID();
  const me = createQuery(() => ({
    queryKey: ["me"],
    queryFn: getMe,
    retry: false,
    staleTime: 60_000,
  }));
  const can = (permission: string) => hasPermission(me.data, permission);
  const session = createQuery(() => ({
    queryKey: ["session", sessionID(), projectID(), environmentID()],
    queryFn: () => getSession(address()),
    enabled: enabled(),
    retry: false,
    refetchInterval: (query) =>
      ["open", "closing"].includes(query.state.data?.status ?? "")
        ? 5_000
        : false,
  }));
  const polling = () =>
    ["open", "closing"].includes(session.data?.status ?? "");
  const turns = createInfiniteQuery(() => ({
    queryKey: ["session-turns", sessionID(), projectID(), environmentID()],
    queryFn: ({ pageParam }) =>
      listSessionTurns(address(), {
        cursor: pageParam || undefined,
        limit: 100,
      }),
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor,
    enabled: enabled(),
    retry: false,
    refetchInterval: polling() ? 5_000 : false,
  }));
  const items = createMemo(
    () => turns.data?.pages.flatMap((page) => page.turns) ?? [],
  );
  createEffect(
    on(
      () => session.data?.status,
      (status, previous) => {
        if (previous && (status === "closed" || status === "cancelled"))
          void turns.refetch();
      },
    ),
  );
  const [receipt, setReceipt] = createSignal<SessionReceipt>();
  type Control = { kind: "interrupt" | "cancel" };
  const [control, setControl] = createSignal<Control>();
  const controlKeys = new Map<string, string>();
  const controlLabels = { interrupt: "Interrupt Session", cancel: "Cancel Session" };
  const performControl = async (operation: Control) => {
    const operationKey = operation.kind;
    const key = controlKeys.get(operationKey) ?? crypto.randomUUID();
    controlKeys.set(operationKey, key);
    const input = { idempotency_key: key };
    const result = operation.kind === "interrupt"
      ? await interruptSession(address(), input)
      : await cancelSession(address(), input);
    setReceipt(result);
    controlKeys.delete(operationKey);
    await refresh();
  };
  const refresh = () =>
    Promise.all([
      session.refetch(),
      turns.refetch(),
      queryClient.invalidateQueries({ queryKey: ["sessions"] }),
    ]);
  return (
    <section class={ui.page}>
      <PageHeader
        title="Session"
        back={{ href: "/sessions", label: "Sessions" }}
        subtitle={<IDText value={sessionID()} mode="full" />}
        badge={
          <Show when={session.data}>
            {(current) => (
              <StatusBadge resource="session" status={current().status} />
            )}
          </Show>
        }
        actions={
          <div class="flex flex-wrap gap-2">
            <Show when={can("sessions.interrupt") && polling()}>
              <button class={ui.secondaryButton} onClick={() => setControl({ kind: "interrupt" })}>Interrupt Session</button>
            </Show>
            <Show when={can("sessions.cancel") && polling()}>
              <button class={ui.dangerOutlineButton} onClick={() => setControl({ kind: "cancel" })}>Cancel Session</button>
            </Show>
          </div>
        }
      />
      <Show when={session.isError}>
        <StatePanel error={errorMessage(session.error)} />
      </Show>
      <Show
        when={enabled()}
        fallback={
          <StatePanel error="Session ID and environment scope are required." />
        }
      >
        <Show
          when={!session.isPending}
          fallback={<StatePanel loading="Loading Session..." />}
        >
          <Show when={session.data}>
            {(current) => (
              <div class="grid grid-cols-[minmax(0,1fr)_300px] items-start gap-3.5 max-[960px]:grid-cols-1">
                <div class="flex min-w-0 flex-col gap-6">
                  <Show when={receipt()}>
                    {(value) => (
                      <Panel title="Operation receipt">
                        <p class={ui.muted}>
                          The request was accepted. Turn status shows its
                          outcome. A control request may still be stopping an active
                          process or draining admitted work.
                        </p>
                        <pre class="whitespace-pre-wrap break-words">
                          {JSON.stringify(value(), null, 2)}
                        </pre>
                      </Panel>
                    )}
                  </Show>
                  <SessionProgress address={address()} polling={polling()} />
                  <Show when={current().slack_channel_id}><SessionDelivery address={address()} canManage={me.data?.role === "owner" || me.data?.role === "admin"} /></Show>
                  <Panel title="Turns">
                    <p class={ui.muted}>
                      Inputs are shown in admission order. Queued work remains
                      available for inspection when execution is blocked.
                    </p>
                    <Show when={turns.isError}>
                      <StatePanel error={errorMessage(turns.error)} />
                    </Show>
                    <Show
                      when={!turns.isPending}
                      fallback={<StatePanel loading="Loading Turns..." />}
                    >
                      <Show
                        when={items().length}
                        fallback={<StatePanel empty="No Turns yet." />}
                      >
                        <ol class="m-0 list-none divide-y divide-console-border p-0">
                          <For each={items()}>
                            {(turn) => (
                              <li class="py-4" id={`turn-${turn.id}`}>
                                <div class="flex flex-wrap items-center gap-2">
                                  <strong>Turn #{turn.sequence}</strong>
                                  <StatusBadge resource="turn" status={turn.status} />
                                  <IDText value={turn.id} />
                                </div>
                                <Show when={!turn.payload_expired_at} fallback={<p class={ui.muted}>Turn content expired.</p>}>
                                <p class={ui.muted}>Input</p>
                                <pre class="whitespace-pre-wrap break-words">
                                  {turn.input?.map(part => part.text).join("")}
                                </pre>
                                </Show>
                                <Show when={turn.status === "finalizing"}>
                                  <p class={ui.muted}>
                                    The handler has returned. Helmr is saving
                                    the required disk state before completion.
                                  </p>
                                </Show>
                                <Show when={turn.error}>
                                  {error => <p class={ui.fieldError}><code>{error().code}</code><Show when={error().message}>: {error().message}</Show></p>}
                                </Show>
                                <Show when={Object.hasOwn(turn, "result")}>
                                  <p class={ui.muted}>Result</p>
                                  <pre class="whitespace-pre-wrap break-words">
                                    {JSON.stringify(turn.result, null, 2)}
                                  </pre>
                                </Show>
                                <Show when={turn.status === "completed" && turn.response}>
                                  {response => <div><p class={ui.muted}>Response</p><ContentView content={response()} /></div>}
                                </Show>
                                <TurnQuestions address={address()} turnID={turn.id} polling={polling()} />
                                <Show when={turn.terminal_at}>
                                  {(time) => (
                                    <p class={ui.muted}>
                                      Settled <RelativeTime value={time()} />
                                    </p>
                                  )}
                                </Show>
                              </li>
                            )}
                          </For>
                        </ol>
                      </Show>
                      <Show when={turns.hasNextPage}>
                        <button
                          class={ui.secondaryButton}
                          disabled={turns.isFetchingNextPage}
                          onClick={() => void turns.fetchNextPage()}
                        >
                          {turns.isFetchingNextPage
                            ? "Loading..."
                            : "Load more Turns"}
                        </button>
                      </Show>
                    </Show>
                  </Panel>
                  <p class={ui.muted}>Send work through the CLI or the connected Slack conversation. Use the CLI to close this Session or release a hold.</p>
                </div>
                <DetailList title="Session details">
                  <DetailItem label="Agent">
                    <IDText value={current().agent_id} />
                  </DetailItem>
                  <Show when={current().initial_turn}>
                    {turn => <DetailItem label="Initial Turn"><span class="inline-flex flex-wrap items-center gap-2"><IDText value={turn().id} mode="link" href={`#turn-${turn().id}`} /><StatusBadge resource="turn" status={turn().status} /></span></DetailItem>}
                  </Show>
                  <Show when={current().parent_session_id}>
                    {id => <DetailItem label="Owner Session"><IDText value={id()} mode="link" href={sessionConsolePath(id(), projectID(), environmentID())} /></DetailItem>}
                  </Show>
                  <Show when={current().requester_session_id}>
                    {id => <DetailItem label="Requester Session"><IDText value={id()} mode="link" href={sessionConsolePath(id(), projectID(), environmentID())} /></DetailItem>}
                  </Show>
                  <Show when={current().key}>
                    {(key) => (
                      <DetailItem label="Key">
                        <code>{key()}</code>
                      </DetailItem>
                    )}
                  </Show>
                  <DetailItem label="Status">
                    <StatusBadge resource="session" status={current().status} />
                  </DetailItem>
                  <DetailItem label="Deployment">
                    <IDText
                      value={current().deployment_id}
                      mode="link"
                      href={deploymentHref(current().deployment_id)}
                    />
                  </DetailItem>
                  <DetailItem label="Computer">
                    <IDText
                      value={current().computer_id}
                      mode="link"
                      href={`/computers/${current().computer_id}`}
                    />
                  </DetailItem>
                  <DetailItem label="Created">
                    <RelativeTime value={current().created_at} />
                  </DetailItem>
                  <DetailItem label="Holds">
                    <Show
                      when={current().holds.length}
                      fallback={<span>None</span>}
                    >
                      <ul class="space-y-3">
                        <For each={current().holds}>
                          {(hold) => (
                            <li class="flex flex-col items-start gap-1">
                              <span>
                                {hold.reason || "Paused"} ({hold.scope})
                              </span>
                              <IDText value={hold.id} />
                              <Show when={hold.session_id !== sessionID()}>
                                <span class="block">Inherited from <IDText value={hold.session_id} mode="link" href={sessionConsolePath(hold.session_id, projectID(), environmentID())} /></span>
                              </Show>
                            </li>
                          )}
                        </For>
                      </ul>
                    </Show>
                  </DetailItem>
                </DetailList>
              </div>
            )}
          </Show>
        </Show>
      </Show>
      <Show when={control()} keyed>
        {operation => (
          <ConfirmModal
            title={controlLabels[operation.kind]}
            confirmLabel={controlLabels[operation.kind]}
            busyLabel="Requesting..."
            tone={operation.kind === "cancel" ? "danger" : "default"}
            onClose={() => setControl(undefined)}
            errorMessage={errorMessage}
            onConfirm={() => performControl(operation)}
          >
            <Show when={operation.kind === "interrupt"}>
              Request active work in this Session and its owned children to stop, and hold queued work. Interrupted Turns do not restart when the hold is released.
            </Show>
            <Show when={operation.kind === "cancel"}>
              Cancel queued Turns and request that active work in this Session and its owned children stop. Retained inputs remain available for inspection. Other Sessions sharing the Computer are not cancelled.
            </Show>
          </ConfirmModal>
        )}
      </Show>
    </section>
  );
}
