import { Show } from "solid-js";
import { formatAbsolute, formatRelative } from "./time";

export function RelativeTime(props: { value: string | null | undefined; fallback?: string }) {
  return (
    <Show when={props.value} fallback={<span class="text-console-faint">{props.fallback ?? "—"}</span>}>
      {(value) => (
        <time class="whitespace-nowrap text-console-muted" datetime={value()} title={formatAbsolute(value())}>
          {formatRelative(value())}
        </time>
      )}
    </Show>
  );
}
