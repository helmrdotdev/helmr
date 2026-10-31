import { createQuery } from "@tanstack/solid-query";
import { createSignal, For, Show } from "solid-js";
import { applyAgentChange, previewAgentChange } from "../../lib/agent-delete";
import type { DefinitionScope } from "../../lib/definitions";
import { errorMessage } from "../../lib/error";
import { Dialog } from "../../ui/Dialog";
import { Notice } from "../../ui/Notice";
import { QueryState } from "../../ui/QueryState";
import { LinesSkeleton } from "../../ui/Skeleton";
import { ui } from "../../ui/styles";

const blockerLabel = (code: string) => ({ defined_in_current: "Remove this agent from code and promote the updated deployment first." } as Record<string, string>)[code] ?? code;

export function AgentDeleteDialog(props: { agent: string; scope: DefinitionScope; restore: boolean; onClose: () => void; onSaved: () => void }) {
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");
  const [refreshRequired, setRefreshRequired] = createSignal(false);
  const action = () => props.restore ? "Restore" : "Delete";
  const preview = createQuery(() => ({ queryKey: ["agents", "change-preview", props.scope.projectID, props.scope.environmentID, props.agent, props.restore], queryFn: () => previewAgentChange(props.agent, props.scope, props.restore), retry: false, refetchOnWindowFocus: false, staleTime: 0 }));
  const refresh = async () => { setRefreshRequired(true); const result = await preview.refetch(); if (!result.isError) { setError(""); setRefreshRequired(false); } };
  return <Dialog title={`${action()} ${props.agent}`} onClose={props.onClose} closeDisabled={busy()}>
    <div class="space-y-4">
      <p>{props.restore ? "Restore this agent with its existing identity and history." : "Delete this agent and remove its individual access assignments and Slack connection. Session and execution history will remain available to authorized users."}</p>
      <Show when={error()}><Notice tone="danger">{error()} Refresh this preview before trying again. If you lost access after deletion, ask a manager with remaining access to check the agent's status.</Notice></Show>
      <QueryState query={preview} pending={<LinesSkeleton label="Checking agent changes" />} error={e => errorMessage(e)}>{() => <>
        <For each={preview.data!.blockers}>{blocker => <Notice tone="warning"><p>{blockerLabel(blocker.code)}</p></Notice>}</For>
        <Show when={!props.restore}>
          <Show when={preview.data!.effects.sessions_cancelled > 0}>
            <Notice tone="warning">Deleting cancels {preview.data!.effects.sessions_cancelled === 1 ? "1 session that is" : `${preview.data!.effects.sessions_cancelled} sessions that are`} still open or closing. Their cleanup continues after deletion.</Notice>
          </Show>
          <dl class="grid grid-cols-2 gap-2 text-control">
            <dt>Sessions cancelled</dt><dd>{preview.data!.effects.sessions_cancelled}</dd>
            <dt>Individual agent access removed</dt><dd>{preview.data!.effects.selected_agent_edges_removed}</dd>
            <dt>Empty assignments removed</dt><dd>{preview.data!.effects.ineffective_assignments_removed}</dd>
            <dt>Assignments retaining other access</dt><dd>{preview.data!.effects.mixed_assignments_preserved}</dd>
            <dt>Slack connection</dt><dd>{preview.data!.effects.slack_connection_disconnected ? "Disconnect" : "None"}</dd>
            <dt>Old schedules with pending work</dt><dd>{preview.data!.effects.trigger_activations_sealed}</dd>
          </dl>
        </Show>
        <For each={preview.data!.warnings}>{warning => <Notice tone="warning">{warning}</Notice>}</For>
      </>}</QueryState>
      <div class="flex flex-wrap justify-end gap-2">
        <button class={ui.secondaryButton} disabled={busy()} onClick={props.onClose}>Cancel</button>
        <button class={ui.secondaryButton} disabled={busy() || preview.isFetching} onClick={() => void refresh()}>Refresh preview</button>
        <button class={props.restore ? ui.secondaryButton : ui.dangerOutlineButton} disabled={busy() || preview.isFetching || preview.isError || refreshRequired() || !preview.data?.can_apply} onClick={async () => {
          if (!preview.data || !preview.data.can_apply) return;
          setBusy(true); setError("");
          try { await applyAgentChange(props.agent, props.scope, props.restore, preview.data.revision); props.onSaved(); }
          catch (e) { setError(errorMessage(e)); setRefreshRequired(true); }
          finally { setBusy(false); }
        }}>{busy() ? "Saving…" : `${action()} agent`}</button>
      </div>
    </div>
  </Dialog>;
}
