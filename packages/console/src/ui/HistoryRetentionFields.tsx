import { For, Show } from "solid-js";
import { ui } from "./styles";

const units = [
  { seconds: 86400, suffix: "d", label: "Days" },
  { seconds: 3600, suffix: "h", label: "Hours" },
  { seconds: 60, suffix: "m", label: "Minutes" },
  { seconds: 1, suffix: "s", label: "Seconds" },
];
const maxSeconds = 9223372036854775807n;

export type HistoryRetentionDraft = { mode: string; amount: string; unit: number };

export function historyRetentionDraft(policy: string): HistoryRetentionDraft {
  if (policy === "never") return { mode: "never", amount: "30", unit: 86400 };
  const unit = units.find(unit => unit.suffix === policy.slice(-1))!;
  return { mode: "duration", amount: policy.slice(0, -1), unit: unit.seconds };
}

export function historyRetentionSeconds(policy: string): bigint | null {
  if (policy === "never") return null;
  const draft = historyRetentionDraft(policy);
  return BigInt(draft.amount) * BigInt(draft.unit);
}

export function historyRetentionPolicy(draft: HistoryRetentionDraft): string {
  if (draft.mode === "never") return "never";
  if (/^[1-9][0-9]*$/.test(draft.amount)) {
    const seconds = BigInt(draft.amount) * BigInt(draft.unit);
    if (seconds <= maxSeconds) {
      const unit = units.find(unit => seconds % BigInt(unit.seconds) === 0n)!;
      return `${seconds / BigInt(unit.seconds)}${unit.suffix}`;
    }
  }
  throw new Error("Enter a positive whole number for the retention period.");
}

export function HistoryRetentionFields(props: { value: HistoryRetentionDraft; onChange: (value: HistoryRetentionDraft) => void }) {
  return (
    <fieldset>
      <legend>Session history retention</legend>
      <p class={ui.muted}>Applies to all remaining Session history in every Environment of this Project.</p>
      <label class={ui.field}>
        <span>History retention</span>
        <select class={ui.input} value={props.value.mode} onChange={event => props.onChange({ ...props.value, mode: event.currentTarget.value })}>
          <option value="never">Never expire</option>
          <option value="duration">Keep for a duration</option>
        </select>
      </label>
      <Show when={props.value.mode === "duration"} fallback={<p class={ui.muted}>Automatic expiry is disabled. Previously expired content cannot be restored.</p>}>
        <div class="flex gap-2">
          <label class={ui.field}>
            <span>Retention period</span>
            <input
              class={ui.input}
              type="number"
              min="1"
              max={String(maxSeconds / BigInt(props.value.unit))}
              step="1"
              required
              value={props.value.amount}
              onInput={event => props.onChange({ ...props.value, amount: event.currentTarget.value })}
            />
          </label>
          <label class={ui.field}>
            <span>Unit</span>
            <select class={ui.input} value={props.value.unit} onChange={event => props.onChange({ ...props.value, unit: Number(event.currentTarget.value) })}>
              <For each={units}>{unit => <option value={unit.seconds}>{unit.label}</option>}</For>
            </select>
          </label>
        </div>
        <p class={ui.muted}>The period starts after a Session ends and its pending work finishes. Expiry deletes inputs, results, messages, questions, answers and event content. Open Sessions keep their history.</p>
        <p class={ui.muted}>Session identities and statuses, Computer files, and administrative audit records are separate. Very short periods can expire content before a delayed reader fetches it.</p>
      </Show>
    </fieldset>
  );
}
