import { Show } from "solid-js";
import type { HistoryRetentionPolicy } from "../lib/projects";
import { ui } from "./styles";

export type HistoryRetentionDraft = { mode: string; seconds: string };
export function historyRetentionPolicy(draft: HistoryRetentionDraft): HistoryRetentionPolicy {
  if (draft.mode === "until_environment_deletion") return { history_retention_mode: draft.mode };
  const seconds = Number(draft.seconds);
  if (draft.mode === "duration" && Number.isSafeInteger(seconds) && seconds > 0)
    return { history_retention_mode: draft.mode, history_retention_seconds: seconds };
  throw new Error("Choose a history retention policy and a positive whole number of seconds for duration mode.");
}
export function HistoryRetentionFields(props: { value: HistoryRetentionDraft; onChange: (value: HistoryRetentionDraft) => void }) {
  return <fieldset>
    <legend>Session history retention</legend>
    <p class={ui.muted}>Applies to new Sessions. A duration starts after the Session has ended, its process has stopped, and pending obligations have been released. Open Sessions retain their history. Computer disks have a separate lifetime.</p>
    <label class={ui.field}><span>History retention policy</span>
      <select class={ui.input} required value={props.value.mode} onChange={event => props.onChange({ ...props.value, mode: event.currentTarget.value })}>
        <option value="" disabled>Choose a policy</option>
        <option value="duration">Keep for a duration</option>
        <option value="until_environment_deletion">Keep until Environment deletion</option>
      </select>
    </label>
    <Show when={props.value.mode === "duration"}>
      <label class={ui.field}><span>History retention seconds</span><input class={ui.input} type="number" min="1" step="1" required value={props.value.seconds} onInput={event => props.onChange({ ...props.value, seconds: event.currentTarget.value })} /></label>
    </Show>
  </fieldset>;
}
