import { For, Show } from "solid-js";

export function TagList(props: { tags: readonly string[] }) {
  return (
    <Show when={props.tags.length > 0} fallback={<span class="text-console-faint">—</span>}>
      <span class="inline-flex flex-wrap gap-1">
        <For each={props.tags}>
          {(tag) => (
            <span class="rounded-xs border border-console-border bg-console-bg-panel px-1.5 font-mono text-[10.5px] leading-normal text-console-muted">
              {tag}
            </span>
          )}
        </For>
      </span>
    </Show>
  );
}
