import { Show } from "solid-js";
import { cx } from "./styles";

// The Environment's color as a small dot. The color is Environment data, so it is
// applied inline.
export function EnvDot(props: { colorHex: string | undefined; class?: string | undefined }) {
  return (
    <span
      class={cx("inline-block size-1.5 shrink-0 rounded-full bg-console-faint", props.class)}
      style={props.colorHex ? { "background-color": props.colorHex } : undefined}
      aria-hidden="true"
    />
  );
}

// The only way an Environment is shown: its color and its name.
export function EnvChip(props: { name: string; colorHex: string | undefined; class?: string | undefined }) {
  return (
    <span class={cx("inline-flex min-w-0 items-center gap-1.5 font-mono text-chrome text-console-text", props.class)}>
      <EnvDot colorHex={props.colorHex} />
      <span class="truncate">{props.name}</span>
    </span>
  );
}

// Where a dialog's action applies, as one line: "Console demo · ● Demo".
export function EnvironmentTarget(props: { label: string; environmentName: string; environmentColorHex: string | undefined; projectName: string }) {
  return (
    <p class="mb-3 flex min-w-0 flex-wrap items-center gap-1.5 font-mono text-chrome text-console-text" role="group" aria-label={props.label}>
      <span>{props.projectName}</span>
      <span class="text-console-subtle" aria-hidden="true">·</span>
      <Show when={props.environmentColorHex}><EnvDot colorHex={props.environmentColorHex} /></Show>
      <span>{props.environmentName}</span>
    </p>
  );
}
