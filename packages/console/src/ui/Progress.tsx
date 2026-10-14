import { cx } from "./styles";

// The striped bar for redirects and long operations. Without `value` it is
// indeterminate; with `value` (0 to 1) it fills that share of the track.
export function Progress(props: { label: string; value?: number; class?: string }) {
  const percent = () => (props.value === undefined ? 100 : Math.round(Math.min(1, Math.max(0, props.value)) * 100));
  return (
    <div
      class={cx("h-2 w-full overflow-hidden border border-console-border-strong bg-console-surface", props.class)}
      role="progressbar"
      aria-label={props.label}
      aria-valuemin={props.value === undefined ? undefined : 0}
      aria-valuemax={props.value === undefined ? undefined : 100}
      aria-valuenow={props.value === undefined ? undefined : percent()}
    >
      <div class="h-full progress-stripes" style={{ width: `${percent()}%` }} />
    </div>
  );
}
