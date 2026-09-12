import { Show, type JSX } from "solid-js";
import { ui } from "./styles";

export function SectionHeader(props: { title: string; count?: number | undefined; subtitle?: JSX.Element; actions?: JSX.Element }) {
  return (
    <div class="mb-2 flex min-h-7 items-start justify-between gap-3">
      <div class="min-w-0">
        <h2 class={ui.h2}>
          {props.title}
          <Show when={props.count}>
            <span class="ml-2 font-mono text-[11px] font-medium text-console-subtle">{props.count}</span>
          </Show>
        </h2>
        <Show when={props.subtitle}>
          <p class={ui.pageSubtitle}>{props.subtitle}</p>
        </Show>
      </div>
      <Show when={props.actions}>
        <div class="flex shrink-0 items-center gap-1">{props.actions}</div>
      </Show>
    </div>
  );
}
