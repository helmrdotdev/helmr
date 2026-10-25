import { createSignal, For, Show } from "solid-js";
import { hasPermission } from "../../lib/auth";
import { controlComputerExecution, getComputerExecutionScope, type Computer, type ComputerScope, type ExecutionScope } from "../../lib/computers";
import { errorMessage } from "../../lib/error";
import { useMe } from "../../lib/me";
import { sessionConsolePath } from "../../lib/sessions";
import { ConfirmDialog } from "../../ui/Dialog";
import { IDText } from "../../ui/IDText";
import { Notice } from "../../ui/Notice";
import { ui } from "../../ui/styles";

function failureMessage(code: string | undefined): string {
  switch (code) {
    case "capture_refused": return "A consistent snapshot could not be made. Review active work and try again.";
    case "capture_resumed": return "The Computer resumed before hibernation finished.";
    case "new_execution_requested": return "New work cancelled the hibernation request.";
    case "execution_deadline_exceeded": return "The operation did not finish in time. Review capacity and the Computer state before trying again.";
    case "computer_deleted": return "The Computer was deleted.";
    case "checkpoint_unavailable": return "The saved execution state is unavailable. Review the Computer details.";
    default: return "Review the Computer state before trying again.";
  }
}

export function ComputerExecutionControls(props: { computer: Computer; scope: ComputerScope; onChanged: () => Promise<unknown> }) {
  const me = useMe();
  const [preview, setPreview] = createSignal<{ id: string; scope: ComputerScope; members: ExecutionScope; key: string }>();
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string>();
  const [receipt, setReceipt] = createSignal<string>();
  let wakeKey: string | undefined;
  const prepare = async () => {
    setBusy(true); setError(undefined);
    const id = props.computer.id, scope = { ...props.scope };
    try { setPreview({ id, scope, members: await getComputerExecutionScope(id, scope), key: crypto.randomUUID() }); }
    catch (error) { setError(errorMessage(error)); }
    finally { setBusy(false); }
  };
  const wake = async () => {
    setBusy(true); setError(undefined);
    wakeKey ??= crypto.randomUUID();
    try {
      await controlComputerExecution(props.computer.id, props.scope, "wake", wakeKey);
      wakeKey = undefined;
      setReceipt("Wake accepted. Background execution resumes when ready. No prompt was sent and Stop holds remain unchanged.");
      await props.onChanged();
    } catch (error) { setError(errorMessage(error)); }
    finally { setBusy(false); }
  };
  return <div class="flex flex-col gap-2">
    <Show when={props.computer.residency === "parked"}><Notice tone="info">Background work is paused. Wake this Computer or send a new request to continue.</Notice></Show>
    <Show when={props.computer.execution_control?.status === "accepted"}><Notice tone="info">{props.computer.execution_control?.kind === "hibernate" ? "Hibernating…" : "Waking…"}</Notice></Show>
    <Show when={props.computer.execution_control?.status === "failed"}><Notice tone="danger">Could not {props.computer.execution_control?.kind}. {failureMessage(props.computer.execution_control?.error_code)}</Notice></Show>
    <Show when={props.computer.status === "available"}>
      <div class="flex gap-2">
        <Show when={hasPermission(me.data, "computers.hibernate", props.scope)}><button class={ui.secondaryButton} disabled={busy() || props.computer.execution_control?.status === "accepted"} onClick={() => void prepare()}>Hibernate Computer</button></Show>
        <Show when={hasPermission(me.data, "computers.wake", props.scope)}><button class={ui.secondaryButton} disabled={busy() || props.computer.execution_control?.status === "accepted"} onClick={() => void wake()}>Wake Computer</button></Show>
      </div>
    </Show>
    <Show when={error()}>{message => <Notice tone="danger" role="alert">{message()}</Notice>}</Show>
    <Show when={receipt()}>{message => <Notice tone="info" role="status">{message()}</Notice>}</Show>
    <Show when={preview()} keyed>{target => <ConfirmDialog title="Hibernate Computer" confirmLabel="Hibernate Computer" onClose={() => setPreview(undefined)} target={<IDText value={target.id} />} onConfirm={async () => {
      await controlComputerExecution(target.id, target.scope, "hibernate", target.key, target.members.digest);
      setReceipt("Hibernate accepted. Background progress pauses when capture succeeds. The Computer state shows completion.");
      await props.onChanged();
    }}>
      <p>Background tasks on this Computer will stop progressing until you wake it or send a new request. State is preserved when capture succeeds. Active foreground work prevents hibernation.</p>
      <p>Affected Sessions ({target.members.sessions.length}):</p>
      <For each={target.members.sessions}>{id => <div><IDText value={id} mode="link" href={sessionConsolePath(id)} /></div>}</For>
      <Show when={target.members.commands.length}><p>Active Commands ({target.members.commands.length}):</p><For each={target.members.commands}>{id => <div><IDText value={id} /></div>}</For></Show>
    </ConfirmDialog>}</Show>
  </div>;
}
