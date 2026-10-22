import { useParams } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createEffect, createSignal, For, onCleanup, Show, type JSX } from "solid-js";
import { CopyLinkButton } from "../components/CopyLinkButton";
import { computerHref } from "../features/computers/navigation";
import { deploymentHref } from "../features/deployments/navigation";
import { environmentChip } from "../features/projects/display";
import { createSessionLog, type SessionLog } from "../features/sessions/sessionLog";
import { SessionLogView } from "../features/sessions/SessionLogView";
import { SessionDelivery } from "../features/slack/SessionDelivery";
import { ApiError } from "../lib/api";
import { hasPermission } from "../lib/auth";
import { agentHref } from "../lib/definitions";
import { errorMessage, loadError } from "../lib/error";
import { isOrganizationAdmin, useMe } from "../lib/me";
import { useScope } from "../lib/scope";
import {
  askConsolePath,
  cancelSession,
  closeSession,
  getSessionCloseScope,
  getSession,
  watchSession,
  recoveryGapNotice,
  resumeSession,
  interruptSession,
  interruptTurn,
  listAsks,
  sessionConsolePath,
  type Session,
  type SessionAddress,
} from "../lib/sessions";
import { PrimaryLink } from "../ui/DataTable";
import { ConfirmDialog } from "../ui/Dialog";
import { IDText } from "../ui/IDText";
import { Notice } from "../ui/Notice";
import { Page } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { DangerZone } from "../ui/Panel";
import { EmptyState, isMissingSessionRead, NotFoundState, QueryState } from "../ui/QueryState";
import { PageSkeleton } from "../ui/Skeleton";
import { RelativeTime } from "../ui/RelativeTime";
import { StatusBadge } from "../ui/StatusBadge";
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
    // Recover a failed read without another hint, keeping the visible error state.
    // Successful reads and permanent API errors need no periodic refresh.
    refetchInterval: query => {
      const error = query.state.error;
      return error instanceof TypeError || error instanceof ApiError && (error.status >= 500 || error.status === 408 || error.status === 429) ? 5_000 : false;
    },
  }));
  const live = () => ["open", "closing"].includes(session.data?.status ?? "");
  const missing = (error: unknown) => isMissingSessionRead(error, can("sessions.read"));
  const notFound = () => !session.data && session.isError && missing(session.error);
  const log = createSessionLog({ address });
  const refresh = () => Promise.all([session.refetch(), log.refresh(), queryClient.invalidateQueries({ queryKey: ["session-active-ask"] })]);
  createEffect(() => {
    if (!enabled()) return;
    let refreshing = false;
    let again = false;
    const changed = async () => {
      if (refreshing) { again = true; return; }
      refreshing = true;
      try {
        do {
          again = false;
          await refresh();
        } while (again);
      } finally { refreshing = false; }
    };
    onCleanup(watchSession(address(), () => { void changed(); }));
  });
  const [receipt, setReceipt] = createSignal<string>();
  const [closePreview, setClosePreview] = createSignal<{ members: import("../lib/computers").ExecutionScope; key: string }>();
  const [closeError, setCloseError] = createSignal<string>();
  const prepareClose = async () => {
    setCloseError(undefined);
    try { setClosePreview({ members: await getSessionCloseScope(address()), key: crypto.randomUUID() }); }
    catch (error) { setCloseError(sessionError(error)); }
  };
  type Control = { kind: "interrupt" | "cancel" } | { kind: "turn_interrupt"; turnID: string } | { kind: "resume"; holdID: string };
  const [control, setControl] = createSignal<Control>();
  const controlKeys = new Map<string, string>();
  const controlLabels = { interrupt: "Interrupt session", cancel: "Cancel session", turn_interrupt: "Interrupt turn", resume: "Resume" };
  const performControl = async (operation: Control) => {
    const operationKey = operation.kind === "turn_interrupt" ? `${operation.kind}:${operation.turnID}` : operation.kind === "resume" ? `resume:${operation.holdID}` : operation.kind;
    const key = controlKeys.get(operationKey) ?? crypto.randomUUID();
    controlKeys.set(operationKey, key);
    const input = { idempotency_key: key };
    if (operation.kind === "resume") await resumeSession(address(), { ...input, hold_id: operation.holdID });
    else if (operation.kind === "turn_interrupt") await interruptTurn(address(), operation.turnID, input);
    else if (operation.kind === "interrupt") await interruptSession(address(), input);
    else await cancelSession(address(), input);
    setReceipt(operation.kind === "resume" ? "Resume requested. Completed requests will not run again. Other reasons for pausing remain in place." : `${controlLabels[operation.kind]} requested. The log shows when work stops. Completed results remain available.`);
    controlKeys.delete(operationKey);
    await Promise.all([refresh(), queryClient.invalidateQueries({ queryKey: ["sessions"] })]);
  };
  return (
    <Page>
      <PageHeader
        title={notFound() ? "Session not found" : "Session"}
        breadcrumb={[{ href: "/sessions", label: "Sessions" }]}
        subtitle={<Show when={!notFound()}><IDText value={sessionID()} mode="full" /></Show>}
        badge={
          <Show when={session.data}>
            {(current) => <StatusBadge resource="session" status={current().status} />}
          </Show>
        }
        actions={
          <Show when={!notFound()}>
            <CopyLinkButton />
            <Show when={can("sessions.close") && session.data?.status === "open"}><button type="button" class={ui.secondaryButton} onClick={() => void prepareClose()}>Close Session</button></Show>
            <Show when={can("sessions.interrupt") && live()}>
              <button type="button" class={ui.secondaryButton} onClick={() => setControl({ kind: "interrupt" })}>Interrupt session</button>
            </Show>
          </Show>
        }
      />
      <Show when={enabled()} fallback={<EmptyState title="Select a project and environment." />}>
        <QueryState query={session} pending={<PageSkeleton label="Loading session" sections={["Log"]} />} error={(error) => loadError(error, "Could not load this session.")}
          missing={missing}
          notFound={<NotFoundState noun="session" id={sessionID()} environment={scope.selectedEnvironment()?.name} back={{ href: "/sessions", label: "Go to sessions" }} />}
        >
          {(current) => (
            <div class="flex flex-col gap-3">
              <SessionProperties session={current()} />
              <NeedsYou session={current()} log={log} address={address()} canResume={can("sessions.resume")} onResume={holdID => setControl({ kind: "resume", holdID })} />
              <Show when={receipt()}>
                {(text) => <Notice tone="info" onDismiss={() => setReceipt(undefined)}>{text()}</Notice>}
              </Show>
              <SessionLogView
                log={log}
                address={address()}
                live={live()}
                liveHint={current().slack_channel_id ? "Send work from the CLI or Slack." : "Send work from the CLI."}
                canInterrupt={can("sessions.interrupt")}
                onInterruptTurn={(turnID) => setControl({ kind: "turn_interrupt", turnID })}
              />
              <Show when={current().slack_channel_id}><SessionDelivery address={address()} live={live()} canManage={isOrganizationAdmin(me.data?.role)} /></Show>
              <Show when={can("sessions.cancel") && live()}>
                <DangerZone
                  class="mt-3"
                  title="Cancel session"
                  action={<button type="button" class={ui.dangerOutlineButton} onClick={() => setControl({ kind: "cancel" })}>Cancel session</button>}
                >
                  Cancel queued turns and request that active work in this session and its owned children stop. The session ends as cancelled.
                </DangerZone>
              </Show>
            </div>
          )}
        </QueryState>
      </Show>
      <Show when={closeError()}>{message => <Notice tone="danger" role="alert">{message()}</Notice>}</Show>
      <Show when={closePreview()} keyed>{target => <ConfirmDialog title="Close Session" confirmLabel="Close Session" tone="danger" onClose={() => setClosePreview(undefined)} onConfirm={async () => {
        await closeSession(address(), { idempotency_key: target.key, expected_scope: target.members.digest });
        setReceipt("Close accepted. Accepted work will settle before closure. Wake cannot undo closure; other work may still occupy the Computer.");
        await refresh();
      }}>
        <p>Close this Session{target.members.sessions.length > 1 ? " and its owned Sessions" : ""}? New messages will no longer be accepted. Native background tasks stop when their Session closes. Wake cannot undo this operation.</p>
        <For each={target.members.sessions}>{id => <div><IDText value={id} mode="link" href={sessionConsolePath(id)} /></div>}</For>
      </ConfirmDialog>}</Show>
      <Show when={control()} keyed>
        {operation => (
          <ConfirmDialog
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
              Request running work and initialization in this session and its owned children to stop, and hold queued work. Completed results remain available. Interrupted turns do not restart when the hold is released.
            </Show>
            <Show when={operation.kind === "resume"}>
              Continue from the restored Computer state. Completed requests will not run again. Other reasons for pausing remain in place.
            </Show>
            <Show when={operation.kind === "turn_interrupt"}>
              Request this running turn to stop. Queued work can proceed after stopping is confirmed. Existing session holds remain in force.
            </Show>
            <Show when={operation.kind === "cancel"}>
              Cancel queued turns and request that active work in this session and its owned children stop. Retained inputs remain available for inspection. Other sessions sharing the computer are not cancelled.
            </Show>
          </ConfirmDialog>
        )}
      </Show>
    </Page>
  );
}

// What the Session is and where it runs, in one row of label/value cells.
function SessionProperties(props: { session: Session }) {
  const process = () => props.session.activity.process;
  const processStatus = () => {
    const value = process();
    if (!value) return undefined;
    return value.reconstruction_required ? "recovery required" : value.status;
  };
  return (
    <dl class={ui.propertyBar} aria-label="Session properties">
      <Property label="Agent"><PrimaryLink href={agentHref(props.session.agent)}>{props.session.agent}</PrimaryLink></Property>
      <Property label={props.session.slack_channel_id ? "Conversation" : "Started from"}>
        <Show when={props.session.slack_channel_id} fallback={<Show when={props.session.key} fallback="CLI or API">{key => <>key <code>{key()}</code></>}</Show>}>
          {channel => <>{channel().startsWith("D") ? "Slack DM" : "Slack channel"} · <code>{channel()}</code></>}
        </Show>
      </Property>
      <Property label="Suspension">{props.session.suspension === "after_turn" ? "After a settled Turn" : "Manual"}</Property>
      <Property label="Automatic close">{props.session.close_after_idle_ms == null ? "Off" : `${props.session.close_after_idle_ms / 86_400_000} days without new user work; background tasks may stop`}</Property>
      <Property label="Created"><RelativeTime value={props.session.created_at} /></Property>
      <Property label="Deployment"><IDText value={props.session.deployment_id} mode="link" href={deploymentHref(props.session.deployment_id)} /></Property>
      <Property label="Computer">
        <span class="inline-flex min-w-0 items-baseline gap-1.5">
          <Show when={props.session.computer_id} fallback={<span class={ui.emptyCell}>—</span>}>{id => <IDText value={id()} mode="link" href={computerHref(id())} />}</Show>
          <span class="truncate text-console-subtle">· {props.session.template}<Show when={processStatus()}>{status => <> · process {status()}</>}</Show></span>
        </span>
      </Property>
      <Property label="Related">
        <Show when={props.session.parent_session_id || props.session.requester_session_id} fallback={<span class={ui.emptyCell}>—</span>}>
          <span class="inline-flex flex-wrap items-baseline gap-1.5">
            {/* A child Session's requester is always its parent. */}
            <Show when={props.session.parent_session_id} fallback={<>Requested by session <IDText value={props.session.requester_session_id!} mode="link" href={sessionConsolePath(props.session.requester_session_id!)} /></>}>
              {id => <>Started by session <IDText value={id()} mode="link" href={sessionConsolePath(id())} /></>}
            </Show>
          </span>
        </Show>
      </Property>
    </dl>
  );
}

function Property(props: { label: string; children: JSX.Element }) {
  return (
    <div class={ui.propertyCell}>
      <dt class="m-0 font-mono text-micro text-console-subtle">{props.label}</dt>
      <dd class="m-0 min-w-0 truncate font-mono text-chrome">{props.children}</dd>
    </div>
  );
}

// What needs a person now: a running Turn waiting on a question, holds and a
// stopped or failed process. Nothing renders when nothing waits.
function NeedsYou(props: { session: Session; log: SessionLog; address: SessionAddress; canResume: boolean; onResume: (holdID: string) => void }) {
  // The active Turn comes from the timeline, so the notice does not depend on
  // which part of the log is loaded.
  const active = () => props.log.activeTurn();
  const pending = createQuery(() => ({
    queryKey: ["session-active-ask", props.address, active()?.id, props.log.events().length],
    queryFn: async () => {
      let cursor: string | undefined;
      do {
        const page = await listAsks({ ...props.address, turnID: active()!.id }, cursor);
        const found = page.items.find(ask => ask.status === "pending");
        if (found) return found;
        cursor = page.nextCursor;
      } while (cursor);
      return null;
    },
    enabled: !!active(),
    retry: false,
  }));
  const stop = () => props.session.activity.process?.stop;
  return (
    <>
      <Show when={active() && pending.data}>
        {ask => (
          <Notice tone="warning" role="status">
            <span>
              Turn #{active()!.sequence} is waiting on a question{ask().question?.title ? <>: <strong class="font-medium">{ask().question!.title}</strong></> : null}. Answer in Slack or the CLI.{" "}
              <a class={ui.monoLink} href={askConsolePath({ sessionID: props.address.sessionID, turnID: active()!.id, askID: ask().id })}>Open question</a>
            </span>
          </Notice>
        )}
      </Show>
      <For each={props.session.status === "open" ? props.session.holds : []}>
        {hold => (
          <Notice tone="warning" role="status">
            <span>
              <Show when={hold.kind === "recovery_gap" && hold.session_id === props.session.id} fallback={<>Held: {hold.reason || "paused"} ({hold.scope === "subtree" ? "this session and its children" : "this session"}).</>}>
                {hold.recovery_gap ? recoveryGapNotice(hold.recovery_gap) : hold.reason}
              </Show>
              <Show when={hold.session_id !== props.session.id}> Inherited from <IDText value={hold.session_id} mode="link" href={sessionConsolePath(hold.session_id)} />.</Show>
              <Show when={hold.kind === "storage_pressure"}>{" "}Queued work resumes when space is available. Stopped requests will not restart automatically.</Show>
              <Show when={hold.kind === "recovery_gap" && hold.session_id === props.session.id && props.canResume}>
                {" "}<button type="button" class={ui.secondaryButton} onClick={() => props.onResume(hold.id)}>Resume</button>
              </Show>
              <Show when={hold.kind !== "storage_pressure" && !(hold.kind === "recovery_gap" && hold.session_id === props.session.id && props.canResume)}>{" "}Resume held work from the CLI.</Show>
            </span>
          </Notice>
        )}
      </For>
      <Show when={stop()}>
        {current => (
          <Notice tone="info" role="status">
            Processing stop {current().state}. New processing waits until stopping is confirmed.
          </Notice>
        )}
      </Show>
      <Show when={props.session.activity.process?.reconstruction_required}>
        <Notice tone="danger" role="status">The session process failed and needs recovery before work continues.</Notice>
      </Show>
    </>
  );
}
