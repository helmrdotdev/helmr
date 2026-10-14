import { isOrganizationAdmin, useMe } from "../lib/me";
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
import { agentHref } from "../lib/definitions";
import { computerHref } from "../features/computers/navigation";
import { CopyLinkButton } from "../components/CopyLinkButton";
import { deploymentHref } from "../features/deployments/navigation";
import { errorMessage } from "../lib/error";
import { hasPermission } from "../lib/auth";
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
import { environmentChip } from "../features/projects/display";
import { CodeBlock } from "../ui/CodeBlock";
import { LoadMore, PrimaryLink } from "../ui/DataTable";
import { ConfirmDialog } from "../ui/Dialog";
import { IDText } from "../ui/IDText";
import { List, ListItem } from "../ui/List";
import { Notice } from "../ui/Notice";
import { Page } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { DetailLayout, KeyValue, KeyValueList, Panel } from "../ui/Panel";
import { EmptyState, QueryState } from "../ui/QueryState";
import { RelativeTime } from "../ui/RelativeTime";
import { StatusBadge, type BadgeStatus } from "../ui/StatusBadge";
import { ui } from "../ui/styles";

const sessionError = (error: unknown) => errorMessage(error, "The operation could not be completed.");

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
  const me = useMe();
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
    <Page>
      <PageHeader
        title="Session"
        breadcrumb={[
          { href: "/agents", label: "Agents" },
          ...(session.data ? [{ href: agentHref(session.data.agent), label: session.data.agent }] : []),
        ]}
        subtitle={<IDText value={sessionID()} mode="full" />}
        badge={
          <Show when={session.data}>
            {(current) => <StatusBadge resource="session" status={current().status} />}
          </Show>
        }
        actions={
          <>
            <CopyLinkButton />
            <Show when={can("sessions.interrupt") && polling()}>
              <button type="button" class={ui.secondaryButton} onClick={() => setControl({ kind: "interrupt" })}>Interrupt Session</button>
            </Show>
            <Show when={can("sessions.cancel") && polling()}>
              <button type="button" class={ui.dangerOutlineButton} onClick={() => setControl({ kind: "cancel" })}>Cancel Session</button>
            </Show>
          </>
        }
      />
      <Show when={enabled()} fallback={<EmptyState title="Select a project and environment." />}>
        <QueryState query={session} pending="Loading Session..." error={sessionError}>
          {(current) => (
            <DetailLayout
              rail={
                <KeyValueList title="Session details">
                  <Show when={current().activity.process}>
                    {process => <>
                      <KeyValue label="Process">
                        <StatusBadge resource="process" status={(process().reconstruction_required ? "recovery_required" : process().status) as BadgeStatus<"process">} />
                      </KeyValue>
                      <Show when={process().stop}>
                        {stop => (
                          <KeyValue label="Process stop">
                            <span class="inline-flex flex-wrap items-center gap-1.5">
                              <StatusBadge resource="process_stop" status={stop().state} />
                              <Show when={stop().turn_id}>{id => <>for <IDText value={id()} mode="link" href={`#turn-${id()}`} /></>}</Show>
                            </span>
                          </KeyValue>
                        )}
                      </Show>
                    </>}
                  </Show>
                  <KeyValue label="Agent">
                    <PrimaryLink href={agentHref(current().agent)}>{current().agent}</PrimaryLink>
                  </KeyValue>
                  <KeyValue label="Initial Turn">
                    <Show when={current().initial_turn} fallback="No input yet">
                      {turn => <span class="inline-flex flex-wrap items-center gap-2"><IDText value={turn().id} mode="link" href={`#turn-${turn().id}`} /><StatusBadge resource="turn" status={turn().status} /></span>}
                    </Show>
                  </KeyValue>
                  <Show when={current().parent_session_id}>
                    {id => <KeyValue label="Owner Session"><IDText value={id()} mode="link" href={sessionConsolePath(id())} /></KeyValue>}
                  </Show>
                  <Show when={current().requester_session_id}>
                    {id => <KeyValue label="Requester Session"><IDText value={id()} mode="link" href={sessionConsolePath(id())} /></KeyValue>}
                  </Show>
                  <Show when={current().key}>
                    {(key) => <KeyValue label="Key"><code>{key()}</code></KeyValue>}
                  </Show>
                  <KeyValue label="Status">
                    <StatusBadge resource="session" status={current().status} />
                  </KeyValue>
                  <KeyValue label="Template"><code>{current().template}</code></KeyValue>
                  <KeyValue label="Deployment">
                    <IDText value={current().deployment_id} mode="link" href={deploymentHref(current().deployment_id)} />
                  </KeyValue>
                  <KeyValue label="Computer">
                    <Show when={current().computer_id}>
                      {(id) => <IDText value={id()} mode="link" href={computerHref(id())} />}
                    </Show>
                  </KeyValue>
                  <KeyValue label="Created">
                    <RelativeTime value={current().created_at} />
                  </KeyValue>
                  <KeyValue label="Holds">
                    <Show when={current().holds.length} fallback="None">
                      <List label="Holds">
                        <For each={current().holds}>
                          {(hold) => (
                            <ListItem class="flex flex-col items-start gap-1">
                              <span>{hold.reason || "Paused"} ({hold.scope})</span>
                              <IDText value={hold.id} />
                              <Show when={hold.session_id !== sessionID()}>
                                <span>Inherited from <IDText value={hold.session_id} mode="link" href={sessionConsolePath(hold.session_id)} /></span>
                              </Show>
                            </ListItem>
                          )}
                        </For>
                      </List>
                    </Show>
                  </KeyValue>
                </KeyValueList>
              }
            >
              <Show when={receipt()}>
                {(value) => (
                  <Panel title="Operation receipt">
                    <p class={ui.muted}>
                      The request was accepted. Turn status shows its
                      outcome. A control request may still be stopping an active
                      process or draining admitted work.
                    </p>
                    <CodeBlock code={JSON.stringify(value(), null, 2)} />
                  </Panel>
                )}
              </Show>
              <SessionProgress address={address()} polling={polling()} />
              <Show when={current().slack_channel_id}><SessionDelivery address={address()} canManage={isOrganizationAdmin(me.data?.role)} /></Show>
              <Panel title="Turns">
                <QueryState
                  query={turns}
                  pending="Loading Turns..."
                  error={sessionError}
                  empty={{ when: (data) => data.pages.every((page) => page.turns.length === 0), title: "No Turns yet." }}
                >
                  {() => (
                    <List
                      label="Turns"
                      footer={<LoadMore count={items().length} noun={{ one: "Turn", other: "Turns" }} hasMore={turns.hasNextPage} loading={turns.isFetchingNextPage} failed={turns.isFetchNextPageError} onLoadMore={() => void turns.fetchNextPage()} />}
                    >
                      <For each={items()}>
                        {(turn) => (
                          <ListItem id={`turn-${turn.id}`} class={ui.blockStack}>
                            <div class="flex flex-wrap items-center gap-2">
                              <strong class="font-medium">Turn #{turn.sequence}</strong>
                              <StatusBadge resource="turn" status={turn.status} />
                              <IDText value={turn.id} />
                              <Show when={turn.status === "running" && can("sessions.interrupt")}>
                                <button type="button" class={ui.secondaryButton} onClick={() => setControl({ kind: "turn_interrupt", turnID: turn.id })}>Interrupt Turn</button>
                              </Show>
                            </div>
                            <Show when={turn.stop}>
                              {stop => (
                                <p class={`${ui.muted} flex flex-wrap items-center gap-1.5`}>
                                  Processing stop <StatusBadge resource="process_stop" status={stop().state} />
                                  <Show when={stop().state !== "stopped"}>New processing waits for stop confirmation.</Show>
                                </p>
                              )}
                            </Show>
                            <Show when={!turn.payload_expired_at} fallback={<p class={ui.muted}>Turn content expired.</p>}>
                              <p class={ui.muted}>Input</p>
                              <CodeBlock code={turn.input?.map(part => part.text).join("") ?? ""} label={`Turn #${turn.sequence} input`} />
                            </Show>
                            <Show when={turn.status === "finalizing"}>
                              <p class={ui.muted}>
                                The handler has returned. Helmr is saving
                                the required disk state before completion.
                              </p>
                            </Show>
                            <Show when={turn.error}>
                              {error => <Notice tone="danger" role="none"><code>{error().code}</code><Show when={error().message}>: {error().message}</Show></Notice>}
                            </Show>
                            <Show when={Object.hasOwn(turn, "result")}>
                              <p class={ui.muted}>Result</p>
                              <CodeBlock code={JSON.stringify(turn.result, null, 2)} label={`Turn #${turn.sequence} result`} />
                            </Show>
                            <Show when={turn.status === "completed" && turn.response}>
                              {response => <div class={ui.blockStack}><p class={ui.muted}>Response</p><ContentView content={response()} /></div>}
                            </Show>
                            <TurnQuestions address={address()} turnID={turn.id} polling={polling()} />
                            <Show when={turn.terminal_at}>
                              {(time) => <p class={ui.muted}>Settled <RelativeTime value={time()} /></p>}
                            </Show>
                          </ListItem>
                        )}
                      </For>
                    </List>
                  )}
                </QueryState>
              </Panel>
              <p class={ui.muted}>Send work from the CLI or Slack; close the Session or release holds from the CLI.</p>
            </DetailLayout>
          )}
        </QueryState>
      </Show>
      <Show when={control()} keyed>
        {operation => (
          <ConfirmDialog
            cancelLabel="Keep"
            title={controlLabels[operation.kind]}
            confirmLabel={controlLabels[operation.kind]}
            busyLabel="Requesting..."
            tone={operation.kind === "cancel" ? "danger" : "default"}
            target={<IDText value={operation.kind === "turn_interrupt" ? operation.turnID : sessionID()} />}
            environment={environmentChip(scope.selectedEnvironment())}
            onClose={() => setControl(undefined)}
            errorMessage={sessionError}
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
          </ConfirmDialog>
        )}
      </Show>
    </Page>
  );
}
