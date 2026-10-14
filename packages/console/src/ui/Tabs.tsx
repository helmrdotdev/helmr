import { A, useNavigate } from "@solidjs/router";
import { For, Show, type JSX } from "solid-js";
import { cx } from "./styles";

// A page tab. The underline of the selected tab sits on the tablist's bottom
// edge under its label; the first tab has no leading padding, so its label lines
// up with the page title.
const TAB =
  "relative inline-flex h-9 cursor-pointer items-center gap-1.5 border-0 bg-transparent px-3 py-0 font-mono text-chrome font-medium text-console-muted transition-colors hover:text-console-text aria-selected:text-console-text aria-selected:after:absolute aria-selected:after:-bottom-px aria-selected:after:left-3 aria-selected:after:right-3 aria-selected:after:h-px aria-selected:after:bg-console-accent first:pl-0 first:aria-selected:after:left-0";

export type TabItem<T extends string> = { value: T; label: string; count?: number | undefined };

// A tab with a count is named "Label, count" rather than its run-together text.
const tabName = (item: TabItem<string>) => (item.count === undefined ? undefined : `${item.label}, ${item.count}`);

const tabID = (base: string, value: string) => `${base}-tab-${value}`;
const panelID = (base: string, value: string) => `${base}-panel-${value}`;

// The tab an arrow key, Home or End moves to, or -1 for any other key.
function nextTab(key: string, current: number, length: number): number {
  return key === "ArrowRight" ? (current + 1) % length
    : key === "ArrowLeft" ? (current - 1 + length) % length
    : key === "Home" ? 0
    : key === "End" ? length - 1
    : -1;
}

function TabLabel(props: { label: string; count?: number | undefined }) {
  return (
    <>
      {props.label}
      <Show when={props.count !== undefined}>
        <span class="font-mono text-badge text-console-subtle">{props.count}</span>
      </Show>
    </>
  );
}

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
    const next = nextTab(event.key, values.indexOf(props.selected), values.length);
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
            aria-label={tabName(item)}
            onClick={() => props.onSelect(item.value)}
          >
            <TabLabel label={item.label} count={item.count} />
          </button>
        )}
      </For>
    </div>
  );
}

export type LinkTabItem<T extends string> = TabItem<T> & { href: string };

// Tabs that are pages of their own: each tab is a link to its page, so the tab is
// in the URL and the browser history. Keyboard behavior matches Tabs; the owning
// layout stays mounted across its tabs' pages, so focus stays on the tablist.
export function LinkTabs<T extends string>(props: {
  id: string;
  label: string;
  items: readonly LinkTabItem<T>[];
  selected: T;
  class?: string;
}) {
  const navigate = useNavigate();
  let list: HTMLDivElement | undefined;
  const move = (event: KeyboardEvent) => {
    const next = nextTab(event.key, props.items.findIndex((item) => item.value === props.selected), props.items.length);
    if (next < 0) return;
    event.preventDefault();
    const item = props.items[next]!;
    navigate(item.href);
    queueMicrotask(() => list?.querySelector<HTMLElement>(`#${CSS.escape(tabID(props.id, item.value))}`)?.focus());
  };
  return (
    <div ref={list} class={cx("mb-4 flex border-b border-console-border", props.class)} role="tablist" aria-label={props.label} onKeyDown={move}>
      <For each={props.items}>
        {(item) => (
          <A
            href={item.href}
            role="tab"
            id={tabID(props.id, item.value)}
            class={TAB}
            aria-selected={item.value === props.selected}
            aria-controls={item.value === props.selected ? panelID(props.id, item.value) : undefined}
            tabIndex={item.value === props.selected ? 0 : -1}
            aria-label={tabName(item)}
          >
            <TabLabel label={item.label} count={item.count} />
          </A>
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
