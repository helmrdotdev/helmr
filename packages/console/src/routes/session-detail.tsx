import { SessionDelivery } from "../features/slack/SessionDelivery";
import { SessionProgress } from "../features/asks/SessionProgress";
import { ContentView } from "../features/asks/Question";
import { TurnQuestions } from "../features/asks/TurnQuestions";
import { useParams } from "@solidjs/router";
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
import { definitionHref } from "../lib/definitions";
import { computerHref } from "../features/computers/navigation";
import { CopyLinkButton } from "../components/CopyLinkButton";
import { deploymentHref } from "../features/deployments/navigation";
import { ApiError } from "../lib/api";
import { getMe, hasPermission } from "../lib/auth";
import { useScope } from "../lib/scope";
import {
  cancelSession,
  interruptSession,
  interruptTurn,
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

function errorMessage(error: unknown): string {
  return error instanceof ApiError
    ? error.message
    : "The operation could not be completed.";
}

export function SessionDetail() {
  const params = useParams();
  const scope = useScope();
  const identity = () =>
    JSON.stringify([
      params["session_id"],
      scope.selectedProjectID(),
      scope.selectedEnvironmentID(),
    ]);
  return (
    <Show when={identity()} keyed>
      {(_identity) => <SessionDetailContent />}
    </Show>
  );
}

function SessionDetailContent() {
  const params = useParams();
  const scope = useScope();
  const queryClient = useQueryClient();
  const sessionID = () => params["session_id"]?.trim() ?? "";
  const projectID = () => scope.selectedProjectID();
  const environmentID = () => scope.selectedEnvironmentID();
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
  type Control = { kind: "interrupt" | "cancel" } | { kind: "turn_interrupt"; turnID: string };
  const [control, setControl] = createSignal<Control>();
  const controlKeys = new Map<string, string>();
  const controlLabels = { interrupt: "Interrupt Session", cancel: "Cancel Session", turn_interrupt: "Interrupt Turn" };
  const performControl = async (operation: Control) => {
    const operationKey = operation.kind === "turn_interrupt" ? `${operation.kind}:${operation.turnID}` : operation.kind;
    const key = controlKeys.get(operationKey) ?? crypto.randomUUID();
    controlKeys.set(operationKey, key);
    const input = { idempotency_key: key };
    const result = operation.kind === "turn_interrupt"
      ? await interruptTurn(address(), operation.turnID, input)
      : operation.kind === "interrupt"
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
            <CopyLinkButton />
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
                                  <Show when={turn.status === "running" && can("sessions.interrupt")}><button class={ui.secondaryButton} onClick={() => setControl({ kind: "turn_interrupt", turnID: turn.id })}>Interrupt Turn</button></Show>
                                </div>
                                <Show when={turn.stop}>{stop => <p class={ui.muted}>Processing stop: {stop().state}. <Show when={stop().state !== "stopped"}>New processing waits for stop confirmation.</Show></p>}</Show>
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
                  <Show when={current().activity.process}>{process => <DetailItem label="Process">{process().reconstruction_required ? "Recovery required" : process().status}<Show when={process().stop}>{stop => <span class="block">Stop {stop().state}<Show when={stop().turn_id}>{id => <> for <IDText value={id()} /></>}</Show></span>}</Show></DetailItem>}</Show>
                  <DetailItem label="Agent">
                    <a class="text-console-accent hover:text-console-accent-hover" href={definitionHref("agents", current().agent, current().deployment_id)}>{current().agent}</a>
                  </DetailItem>
                  <Show when={current().initial_turn} fallback={<DetailItem label="Work">No input yet</DetailItem>}>
                    {turn => <DetailItem label="Initial Turn"><span class="inline-flex flex-wrap items-center gap-2"><IDText value={turn().id} mode="link" href={`#turn-${turn().id}`} /><StatusBadge resource="turn" status={turn().status} /></span></DetailItem>}
                  </Show>
                  <Show when={current().parent_session_id}>
                    {id => <DetailItem label="Owner Session"><IDText value={id()} mode="link" href={sessionConsolePath(id())} /></DetailItem>}
                  </Show>
                  <Show when={current().requester_session_id}>
                    {id => <DetailItem label="Requester Session"><IDText value={id()} mode="link" href={sessionConsolePath(id())} /></DetailItem>}
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
                  <DetailItem label="Template"><a class="text-console-accent hover:text-console-accent-hover" href={definitionHref("templates", current().template, current().deployment_id)}>{current().template}</a></DetailItem>
                  <DetailItem label="Deployment">
                    <IDText
                      value={current().deployment_id}
                      mode="link"
                      href={deploymentHref(current().deployment_id)}
                    />
                  </DetailItem>
                  <DetailItem label="Computer">
                    <Show when={current().computer_id} fallback={<span>No Computer assigned</span>}>
                      {(id) => <IDText value={id()} mode="link" href={computerHref(id())} />}
                    </Show>
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
                                <span class="block">Inherited from <IDText value={hold.session_id} mode="link" href={sessionConsolePath(hold.session_id)} /></span>
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
              Request running work and initialization in this Session and its owned children to stop, and hold queued work. Results already being saved can finish. Interrupted Turns do not restart when the hold is released.
            </Show>
            <Show when={operation.kind === "turn_interrupt"}>
              Request this running Turn to stop. Queued work can proceed after stopping is confirmed. Existing Session holds remain in force.
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
