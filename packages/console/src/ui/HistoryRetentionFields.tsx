import { For, Show } from "solid-js";
import type { HistoryRetentionPolicy } from "../lib/projects";
import { ui } from "./styles";

const units = [
  { seconds: 86400, label: "Days" },
  { seconds: 3600, label: "Hours" },
  { seconds: 60, label: "Minutes" },
  { seconds: 1, label: "Seconds" },
];

export type HistoryRetentionDraft = { mode: string; amount: string; unit: number };

export function historyRetentionDraft(policy: HistoryRetentionPolicy): HistoryRetentionDraft {
  const seconds = policy.history_retention_seconds;
  const unit = seconds === undefined ? 86400 : units.find(unit => seconds % unit.seconds === 0)!.seconds;
  return { mode: policy.history_retention_mode, amount: seconds === undefined ? "" : String(seconds / unit), unit };
}

export function historyRetentionLabel(policy: HistoryRetentionPolicy): string {
  if (policy.history_retention_mode === "until_environment_deletion") return "No expiry";
  const draft = historyRetentionDraft(policy);
  const label = units.find(unit => unit.seconds === draft.unit)!.label.toLowerCase();
  return `${draft.amount} ${draft.amount === "1" ? label.slice(0, -1) : label}`;
}

export function historyRetentionPolicy(draft: HistoryRetentionDraft): HistoryRetentionPolicy {
  if (draft.mode === "until_environment_deletion") return { history_retention_mode: draft.mode };
  const amount = Number(draft.amount);
  const seconds = amount * draft.unit;
  if (draft.mode === "duration" && Number.isSafeInteger(amount) && amount > 0 && Number.isSafeInteger(seconds))
    return { history_retention_mode: draft.mode, history_retention_seconds: seconds };
  throw new Error("Enter a positive whole number for the retention period.");
}

export function HistoryRetentionFields(props: { value: HistoryRetentionDraft; onChange: (value: HistoryRetentionDraft) => void }) {
  return <fieldset>
    <legend>Session history retention</legend>
    <p class={ui.muted}>Changes apply only to new Sessions. Existing Sessions keep their original retention policy.</p>
    <label class={ui.field}><span>History retention</span>
      <select class={ui.input} value={props.value.mode} onChange={event => props.onChange({ ...props.value, mode: event.currentTarget.value })}>
        <option value="until_environment_deletion">No expiry</option>
        <option value="duration">Keep for a duration</option>
      </select>
    </label>
    <Show when={props.value.mode === "duration"} fallback={<p class={ui.muted}>History is kept until this Environment is deleted.</p>}>
      <div class="flex gap-2">
        <label class={ui.field}><span>Retention period</span><input class={ui.input} type="number" min="1" max={Math.floor(Number.MAX_SAFE_INTEGER / props.value.unit)} step="1" required value={props.value.amount} onInput={event => props.onChange({ ...props.value, amount: event.currentTarget.value })} /></label>
        <label class={ui.field}><span>Unit</span><select class={ui.input} value={props.value.unit} onChange={event => props.onChange({ ...props.value, unit: Number(event.currentTarget.value) })}>
          <For each={units}>{unit => <option value={unit.seconds}>{unit.label}</option>}</For>
        </select></label>
      </div>
      <p class={ui.muted}>The period starts after a Session ends and its pending work finishes. After expiry, inputs, results, messages and answers are deleted. Open Sessions keep their history.</p>
    </Show>
  </fieldset>;
}
