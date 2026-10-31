import { useQueryClient } from "@tanstack/solid-query";
import { createSignal, For, Show, type JSX } from "solid-js";
import type { ExecutionScope } from "../../lib/computers";
import { errorMessage } from "../../lib/error";
import { useScope } from "../../lib/scope";
import {
  cancelSession, closeSession, getSessionCloseScope, interruptSession, interruptTurn, resumeSession, sessionConsolePath,
  type SessionAddress,
} from "../../lib/sessions";
import type { ActionMenuItem } from "../../ui/ActionMenu";
import { ConfirmDialog } from "../../ui/Dialog";
import { IDText } from "../../ui/IDText";
import { environmentChip } from "../projects/display";

type Control = { kind: "interrupt" | "cancel" } | { kind: "turn_interrupt"; turnID: string } | { kind: "resume"; sessionID: string; holdID: string };
const LABELS = { interrupt: "Interrupt session", cancel: "Cancel session", turn_interrupt: "Interrupt turn", resume: "Resume" };
const sessionError = (error: unknown) => errorMessage(error, "The operation could not be completed.");

// Every control on a Session, shared by its row menu in a list and the menu by
// its name: what applies to it now, and the confirmations. `address` is the
// Session acted on; a list points it at the row whose menu was used.
export function createSessionControls(props: {
  address: () => SessionAddress;
  onChanged: () => Promise<unknown>;
  onReceipt?: (text: string) => void;
}) {
  const scope = useScope();
  const cache = useQueryClient();
  const [control, setControl] = createSignal<Control>();
  const [closePreview, setClosePreview] = createSignal<{ members: ExecutionScope; key: string; address: SessionAddress }>();
  const [error, setError] = createSignal<string>();
  const keys = new Map<string, string>();
  const changed = () => Promise.all([props.onChanged(), cache.invalidateQueries({ queryKey: ["sessions"] })]);

  const prepareClose = async () => {
    setError(undefined);
    const address = props.address();
    try { setClosePreview({ members: await getSessionCloseScope(address), key: crypto.randomUUID(), address }); }
    catch (cause) { setError(sessionError(cause)); }
  };
  const perform = async (operation: Control, address: SessionAddress) => {
    const operationKey = `${address.sessionID}:${operation.kind === "turn_interrupt" ? `turn:${operation.turnID}` : operation.kind === "resume" ? `resume:${operation.holdID}` : operation.kind}`;
    const key = keys.get(operationKey) ?? crypto.randomUUID();
    keys.set(operationKey, key);
    const input = { idempotency_key: key };
    if (operation.kind === "resume") await resumeSession(address, { ...input, hold_id: operation.holdID });
    else if (operation.kind === "turn_interrupt") await interruptTurn(address, operation.turnID, input);
    else if (operation.kind === "interrupt") await interruptSession(address, input);
    else await cancelSession(address, input);
    props.onReceipt?.(operation.kind === "resume" ? "Resume requested. Completed requests will not run again. Other holds remain in place." : `${LABELS[operation.kind]} requested. The log shows when work stops. Completed results remain available.`);
    keys.delete(operationKey);
    await changed();
  };

  // The object menu: interrupt while work can run, Copy link, then the ends.
  const items = (state: { live: boolean; open: boolean; can: (permission: string) => boolean; copyLink: ActionMenuItem }): ActionMenuItem[] => [
    ...(state.can("sessions.interrupt") && state.live ? [{ label: "Interrupt session", onSelect: () => setControl({ kind: "interrupt" }) }] : []),
    state.copyLink,
    ...(state.can("sessions.close") && state.open ? [{ label: "Close session", tone: "danger" as const, onSelect: () => void prepareClose() }] : []),
    ...(state.can("sessions.cancel") && state.live ? [{ label: "Cancel session", tone: "danger" as const, onSelect: () => setControl({ kind: "cancel" }) }] : []),
  ];

  const Dialogs = (): JSX.Element => (
    <>
      <Show when={closePreview()} keyed>
        {target => (
          <ConfirmDialog title="Close session" confirmLabel="Close session" tone="danger" target={<IDText value={target.address.sessionID} />} environment={environmentChip(scope.selectedEnvironment())} errorMessage={sessionError} onClose={() => setClosePreview(undefined)} onConfirm={async () => {
            await closeSession(target.address, { idempotency_key: target.key, expected_scope: target.members.digest });
            props.onReceipt?.("Close accepted. Accepted work will settle before closure. Wake cannot undo closure; other work may still occupy the computer.");
            await changed();
          }}>
            <p>Close this session{target.members.sessions.length > 1 ? " and its owned sessions" : ""}? New messages will no longer be accepted. Native background tasks stop when their session closes. Wake cannot undo this operation.</p>
            <For each={target.members.sessions}>{id => <div><IDText value={id} mode="link" href={sessionConsolePath(id)} /></div>}</For>
          </ConfirmDialog>
        )}
      </Show>
      <Show when={control()} keyed>
        {operation => {
          // A hold is released on the session that holds it, which may be an ancestor.
          const address = operation.kind === "resume" ? { ...props.address(), sessionID: operation.sessionID } : props.address();
          return (
            <ConfirmDialog
              title={LABELS[operation.kind]}
              confirmLabel={LABELS[operation.kind]}
              busyLabel="Requesting…"
              tone={operation.kind === "cancel" ? "danger" : "default"}
              target={<IDText value={operation.kind === "turn_interrupt" ? operation.turnID : address.sessionID} />}
              environment={environmentChip(scope.selectedEnvironment())}
              onClose={() => setControl(undefined)}
              errorMessage={sessionError}
              onConfirm={() => perform(operation, address)}
            >
              <Show when={operation.kind === "interrupt"}>
                Request running work and initialization in this session and its owned children to stop, and hold queued work. Completed results remain available. Interrupted turns do not restart when the hold is released.
              </Show>
              <Show when={operation.kind === "resume"}>
                Release this hold so queued work can continue. Completed requests will not run again. Other holds remain in place.
              </Show>
              <Show when={operation.kind === "turn_interrupt"}>
                Request this running turn to stop. Queued work can proceed after stopping is confirmed. Existing session holds remain in force.
              </Show>
              <Show when={operation.kind === "cancel"}>
                Cancel queued turns and request that active work in this session and its owned children stop. Retained inputs remain available for inspection. Other sessions sharing the computer are not cancelled.
              </Show>
            </ConfirmDialog>
          );
        }}
      </Show>
    </>
  );

  return { items, Dialogs, error, clearError: () => setError(undefined), interruptTurn: (turnID: string) => setControl({ kind: "turn_interrupt", turnID }), resume: (sessionID: string, holdID: string) => setControl({ kind: "resume", sessionID, holdID }) };
}
