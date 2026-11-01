import { createEffect, createSignal, on, Show, type JSX } from "solid-js";
import { hasPermission } from "../../lib/auth";
import { controlComputerExecution, getComputerHibernatePreview, type ComputerListItem, type ComputerScope } from "../../lib/computers";
import { errorMessage } from "../../lib/error";
import { useMe } from "../../lib/me";
import type { ActionMenuItem } from "../../ui/ActionMenu";
import { ConfirmDialog } from "../../ui/Dialog";
import { IDText } from "../../ui/IDText";
import { Notice } from "../../ui/Notice";

function failureMessage(code: string | undefined): string {
  switch (code) {
    case "capture_refused": return "A consistent snapshot could not be made. Review active work and try again.";
    case "capture_resumed": return "The computer resumed before hibernation finished.";
    case "new_execution_requested": return "New work cancelled the hibernation request.";
    case "execution_deadline_exceeded": return "The operation did not finish in time. Review capacity and the computer state before trying again.";
    case "computer_deleted": return "The computer was deleted.";
    case "checkpoint_unavailable": return "The saved execution state is unavailable. Review the computer details.";
    default: return "Review the computer state before trying again.";
  }
}

// Hibernate and Wake for one Computer: menu items that appear only when its
// state allows them (Hibernate while awake, Wake while sleeping, neither while a
// request is in progress), and the notices and confirmation they need.
export function createComputerControls(props: { computer: () => ComputerListItem | undefined; scope: () => ComputerScope; onChanged: () => Promise<unknown> }) {
  const me = useMe();
  const [preview, setPreview] = createSignal<{ id: string; scope: ComputerScope; sessions: number; key: string }>();
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string>();
  const [receipt, setReceipt] = createSignal<string>();
  const wakeKeys = new Map<string, string>();
  // A detail page reused for another computer starts without the last one's feedback.
  createEffect(on(() => props.computer()?.id, () => { setError(undefined); setReceipt(undefined); }, { defer: true }));

  const prepare = async (computer: ComputerListItem) => {
    setBusy(true); setError(undefined);
    const scope = { ...props.scope() };
    try { setPreview({ id: computer.id, scope, sessions: (await getComputerHibernatePreview(computer.id, scope)).sessions, key: crypto.randomUUID() }); }
    catch (cause) { setError(errorMessage(cause)); }
    finally { setBusy(false); }
  };
  const wake = async (computer: ComputerListItem) => {
    setBusy(true); setError(undefined);
    const key = wakeKeys.get(computer.id) ?? crypto.randomUUID();
    wakeKeys.set(computer.id, key);
    try {
      await controlComputerExecution(computer.id, props.scope(), "wake", key);
      wakeKeys.delete(computer.id);
      setReceipt("Wake accepted. Background execution resumes when ready. No prompt was sent and Stop holds remain unchanged.");
      await props.onChanged();
    } catch (cause) { setError(errorMessage(cause)); }
    finally { setBusy(false); }
  };

  // A list passes the row's computer; a detail page uses its own.
  const items = (computer = props.computer()): ActionMenuItem[] => {
    if (!computer || computer.status !== "available" || busy() || computer.execution_control?.status === "accepted") return [];
    if (computer.residency === "running" && hasPermission(me.data, "computers.hibernate", props.scope())) return [{ label: "Hibernate", onSelect: () => void prepare(computer) }];
    if (computer.residency === "parked" && hasPermission(me.data, "computers.wake", props.scope())) return [{ label: "Wake", onSelect: () => void wake(computer) }];
    return [];
  };

  // The result of a request made from the menu, and its confirmation.
  const Feedback = (): JSX.Element => (
        <>
          <Show when={error()}>{message => <Notice tone="danger" role="alert">{message()}</Notice>}</Show>
          <Show when={receipt()}>{message => <Notice tone="info" role="status" onDismiss={() => setReceipt(undefined)}>{message()}</Notice>}</Show>
          <Show when={preview()} keyed>{target => <ConfirmDialog title="Hibernate computer" confirmLabel="Hibernate computer" onClose={() => setPreview(undefined)} target={<IDText value={target.id} />} onConfirm={async () => {
            await controlComputerExecution(target.id, target.scope, "hibernate", target.key);
            setReceipt("Hibernate accepted. Background progress pauses when capture succeeds. The computer state shows completion.");
            await props.onChanged();
          }}>
            <p>Background tasks on this computer will stop progressing until you wake it or send a new request. State is preserved when capture succeeds. Active foreground work prevents hibernation.</p>
            <p>{target.sessions === 1 ? "1 open session pauses." : `${target.sessions} open sessions pause.`}</p>
          </ConfirmDialog>}</Show>
        </>
  );

  // A detail page also says what the computer's state means for its work.
  const Notices = (): JSX.Element => (
    <>
      <Show when={props.computer()}>
        {computer => (
          <>
            <Show when={computer().residency === "parked"}><Notice tone="info">Background work is paused until you wake this computer or send new work.</Notice></Show>
            <Show when={computer().execution_control?.status === "accepted"}><Notice tone="info">{computer().execution_control?.kind === "hibernate" ? "Hibernate requested." : "Wake requested."} It finishes on its own; the status shows when.</Notice></Show>
            <Show when={computer().execution_control?.status === "failed"}><Notice tone="danger">Could not {computer().execution_control?.kind}. {failureMessage(computer().execution_control?.error_code)}</Notice></Show>
          </>
        )}
      </Show>
      <Feedback />
    </>
  );

  return { items, Notices, Feedback };
}
