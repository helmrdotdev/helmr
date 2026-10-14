import { Show } from "solid-js";
import { formatID } from "./id";
import { cx, ui } from "./styles";

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

// The only way an Environment is shown: its color, its name and whether it is protected.
export function EnvChip(props: { name: string; colorHex: string | undefined; protected?: boolean | undefined; class?: string | undefined }) {
  return (
    <span class={cx("inline-flex min-w-0 items-center gap-1.5 font-mono text-chrome text-console-text", props.class)}>
      <EnvDot colorHex={props.colorHex} />
      <span class="truncate">{props.name}</span>
      <Show when={props.protected}>
        <span class="rounded-xs border border-console-accent/35 px-1 font-mono text-micro leading-normal text-console-accent">protected</span>
      </Show>
    </span>
  );
}

// The Environment a dialog writes to, shown above its fields: the Environment by
// name and color, then its Project and both IDs.
export function EnvironmentTarget(props: {
  label: string;
  environmentName: string;
  environmentColorHex: string | undefined;
  projectName: string;
  projectID: string;
  environmentID: string;
}) {
  return (
    <div class="mb-3 grid gap-1.5 border border-console-border-strong bg-console-bg-panel px-3 py-2.5 text-control" aria-label={props.label}>
      <span class={ui.microLabel}>Target environment</span>
      <strong class="text-body font-medium leading-tight text-console-text">{props.environmentName}</strong>
      <div class="flex min-w-0 flex-wrap items-center gap-1.5 font-mono text-badge text-console-muted">
        <Show when={props.environmentColorHex}>
          <EnvDot colorHex={props.environmentColorHex} />
        </Show>
        <span>{props.projectName}</span>
        <code class="text-micro text-console-subtle">{formatID(props.projectID)} / {formatID(props.environmentID)}</code>
      </div>
    </div>
  );
}
