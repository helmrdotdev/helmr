import { For, Show, type JSX } from "solid-js";
import { cx } from "./styles";

const TAB =
  "relative inline-flex h-9 cursor-pointer items-center gap-1.5 border-0 bg-transparent px-3 py-0 font-mono text-chrome font-medium text-console-muted transition-colors hover:text-console-text aria-selected:text-console-text aria-selected:after:absolute aria-selected:after:-bottom-px aria-selected:after:left-3 aria-selected:after:right-3 aria-selected:after:h-px aria-selected:after:bg-console-accent";

export type TabItem<T extends string> = { value: T; label: string; count?: number | undefined };

const tabID = (base: string, value: string) => `${base}-tab-${value}`;
const panelID = (base: string, value: string) => `${base}-panel-${value}`;

// Sub-sections of a page that switch in place. Arrow keys, Home and End move
// between tabs; only the selected tab is in the tab order, and only its panel is
// rendered, so only it is referenced from its tab.
export function Tabs<T extends string>(props: {
  id: string;
  label: string;
  items: readonly TabItem<T>[];
  selected: T;
  onSelect: (value: T) => void;
  class?: string;
}) {
  let list: HTMLDivElement | undefined;
  const move = (event: KeyboardEvent) => {
    const values = props.items.map((item) => item.value);
    const current = values.indexOf(props.selected);
    const next =
      event.key === "ArrowRight" ? (current + 1) % values.length
      : event.key === "ArrowLeft" ? (current - 1 + values.length) % values.length
      : event.key === "Home" ? 0
      : event.key === "End" ? values.length - 1
      : -1;
    if (next < 0) return;
    event.preventDefault();
    props.onSelect(values[next]!);
    queueMicrotask(() => list?.querySelector<HTMLElement>(`#${CSS.escape(tabID(props.id, values[next]!))}`)?.focus());
  };
  return (
    <div ref={list} class={cx("mb-4 flex border-b border-console-border", props.class)} role="tablist" aria-label={props.label} onKeyDown={move}>
      <For each={props.items}>
        {(item) => (
          <button
            type="button"
            role="tab"
            id={tabID(props.id, item.value)}
            class={TAB}
            aria-selected={item.value === props.selected}
            aria-controls={item.value === props.selected ? panelID(props.id, item.value) : undefined}
            tabIndex={item.value === props.selected ? 0 : -1}
            onClick={() => props.onSelect(item.value)}
          >
            {item.label}
            <Show when={item.count !== undefined}>
              <span class="font-mono text-badge text-console-subtle">{item.count}</span>
            </Show>
          </button>
        )}
      </For>
    </div>
  );
}

// The content of the selected tab.
export function TabPanel<T extends string>(props: { tabs: string; value: T; selected: T; children: JSX.Element }) {
  return (
    <Show when={props.value === props.selected}>
      <div role="tabpanel" id={panelID(props.tabs, props.value)} aria-labelledby={tabID(props.tabs, props.value)}>
        {props.children}
      </div>
    </Show>
  );
}
