import { Show } from "solid-js";
import { Field, TextInput } from "./Field";
import { Select } from "./Select";

const units = [
  { seconds: 86400, suffix: "d", label: "Days" },
  { seconds: 3600, suffix: "h", label: "Hours" },
  { seconds: 60, suffix: "m", label: "Minutes" },
  { seconds: 1, suffix: "s", label: "Seconds" },
];
const maxSeconds = 9223372036854775807n;

const periods = [
  { value: "7d", label: "7 days" },
  { value: "30d", label: "30 days" },
  { value: "90d", label: "90 days" },
  { value: "never", label: "Never expire" },
  { value: "custom", label: "Custom" },
] as const;

type Mode = (typeof periods)[number]["value"];

export type HistoryRetentionDraft = { mode: Mode; amount: string; unit: number };

export function historyRetentionDraft(policy: string): HistoryRetentionDraft {
  if (policy === "never") return { mode: "never", amount: "30", unit: 86400 };
  const unit = units.find(unit => unit.suffix === policy.slice(-1))!;
  const amount = policy.slice(0, -1);
  const preset = periods.find(period => period.value === policy);
  return { mode: preset?.value ?? "custom", amount, unit: unit.seconds };
}

export function historyRetentionSeconds(policy: string): bigint | null {
  if (policy === "never") return null;
  const draft = historyRetentionDraft(policy);
  return BigInt(draft.amount) * BigInt(draft.unit);
}

export function historyRetentionPolicy(draft: HistoryRetentionDraft): string {
  if (draft.mode === "never") return "never";
  if (draft.mode !== "custom") return draft.mode;
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
    <div>
      <Field label="History retention" hint={props.value.mode === "never"
        ? "Session history is kept until the session is deleted."
        : "History older than this is removed and cannot be restored. Open sessions keep their history."}>
        <Select
          value={props.value.mode}
          options={periods}
          onChange={mode => props.onChange(mode === "custom"
            ? { ...props.value, mode }
            : historyRetentionDraft(mode))}
          minWidth="100%"
        />
      </Field>
      <Show when={props.value.mode === "custom"}>
        <div class="flex gap-2">
          <Field label="Retention period" class="flex-1">
            <TextInput
              type="number"
              min="1"
              max={String(maxSeconds / BigInt(props.value.unit))}
              step="1"
              required
              value={props.value.amount}
              onInput={event => props.onChange({ ...props.value, amount: event.currentTarget.value })}
            />
          </Field>
          <Field label="Unit" class="flex-1">
            <Select
              value={String(props.value.unit)}
              options={units.map(unit => ({ value: String(unit.seconds), label: unit.label }))}
              onChange={unit => props.onChange({ ...props.value, unit: Number(unit) })}
              minWidth="100%"
            />
          </Field>
        </div>
      </Show>
    </div>
  );
}
