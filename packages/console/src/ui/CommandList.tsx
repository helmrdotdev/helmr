import { createEffect, createMemo, createSignal, createUniqueId, For, on, Show, untrack, type JSX } from "solid-js";
import { groupEntries, listKey, matchesQuery, type CommandEntry } from "./command";
import { cx, ui } from "./styles";

export type CommandItem = CommandEntry & {
  // Shown before the label (an Environment's color).
  leading?: JSX.Element;
  // Shown after the label (a protected tag, a hint).
  trailing?: JSX.Element;
  // The item in effect now; it is checked.
  current?: boolean;
  onSelect: () => void;
};

const OPTION =
  "grid min-h-7 w-full cursor-pointer grid-cols-[12px_minmax(0,1fr)_auto_12px] items-center gap-2 rounded-xs border border-transparent px-2 py-0.5 text-left font-mono text-badge font-medium text-console-text";
const OPTION_ACTIVE = "border-console-border bg-console-bg-panel";

// A search box over a list of items, for pickers and the command palette. Focus
// stays in the search box: typing filters, ArrowUp/ArrowDown move through the
// shown items (Home/End too while the box is empty; with text they move the
// cursor), Enter chooses one. Nothing reacts while an input
// method is composing. Items may be grouped under headings.
export function CommandList(props: {
  label: string;
  searchLabel: string;
  placeholder: string;
  items: readonly CommandItem[];
  empty: string;
  inputRef?: (element: HTMLInputElement) => void;
  // Caps each group once a query narrows the list, and without one.
  // How many matches a group shows for the query; undefined shows them all.
  limitPerGroup?: (query: string, group: string) => number | undefined;
  class?: string;
}) {
  const id = createUniqueId();
  const listID = `${id}-list`;
  const optionID = (index: number) => `${id}-option-${index}`;
  const [query, setQuery] = createSignal("");
  const [active, setActive] = createSignal(0);

  const groups = createMemo(() => {
    const matched = props.items.filter((item) => matchesQuery(item, query()));
    const capped = matched.filter((item, index) => {
      const limit = props.limitPerGroup?.(query().trim(), item.group ?? "");
      return limit === undefined || matched.slice(0, index).filter((other) => other.group === item.group).length < limit;
    });
    return groupEntries(capped);
  });
  const ordered = createMemo(() => groups().flatMap((group) => group.entries.map(({ entry }) => entry)));

  // The list opens on the current item and a new query starts on the first match;
  // items arriving later keep the position.
  let placed = false;
  createEffect(() => {
    const items = ordered();
    if (!placed && items.length > 0) {
      placed = true;
      setActive(Math.max(0, items.findIndex((item) => item.current)));
    } else if (untrack(active) > items.length - 1) {
      setActive(Math.max(0, items.length - 1));
    }
  });
  createEffect(on(query, () => setActive(0), { defer: true }));
  createEffect(() => {
    const index = active();
    if (ordered().length === 0) return;
    document.getElementById(optionID(index))?.scrollIntoView({ block: "nearest" });
  });

  const move = (index: number) => {
    const last = ordered().length - 1;
    if (last < 0) return;
    setActive(Math.max(0, Math.min(index, last)));
  };

  const onKeyDown = (event: KeyboardEvent) => {
    const key = listKey(event, query());
    if (!key) return;
    event.preventDefault();
    if (key === "next") move(active() + 1);
    else if (key === "previous") move(active() - 1);
    else if (key === "first") move(0);
    else if (key === "last") move(ordered().length - 1);
    else ordered()[active()]?.onSelect();
  };

  return (
    <div class={cx("flex min-h-0 flex-col", props.class)}>
      <div class="border-b border-console-border bg-console-bg-panel p-1.5">
        <input
          ref={props.inputRef}
          type="text"
          role="combobox"
          class={cx(ui.input, "h-6.5 min-h-6.5 font-mono text-badge")}
          aria-label={props.searchLabel}
          aria-expanded="true"
          aria-controls={listID}
          aria-autocomplete="list"
          aria-activedescendant={ordered().length > 0 ? optionID(active()) : undefined}
          placeholder={props.placeholder}
          autocomplete="off"
          spellcheck={false}
          value={query()}
          onInput={(event) => setQuery(event.currentTarget.value)}
          onKeyDown={onKeyDown}
        />
      </div>
      <div id={listID} role="listbox" aria-label={props.label} class="min-h-0 flex-1 overflow-y-auto p-1">
        <Show when={ordered().length > 0} fallback={<p class="m-0 px-3 py-5 text-center text-control text-console-subtle">{props.empty}</p>}>
          <For each={groups()}>
            {(group) => {
              const headingID = createUniqueId();
              const options = (
                <For each={group.entries}>
                  {({ entry, index }) => (
                    <div
                      id={optionID(index)}
                      role="option"
                      aria-selected={index === active()}
                      aria-checked={entry.current ? true : undefined}
                      class={cx(OPTION, index === active() && OPTION_ACTIVE)}
                      onMouseMove={() => setActive(index)}
                      onMouseDown={(event) => event.preventDefault()}
                      onClick={() => entry.onSelect()}
                    >
                      <span class="grid place-items-center">{entry.leading}</span>
                      <span class="truncate">{entry.label}</span>
                      <span class="flex items-center justify-end gap-1.5">{entry.trailing}</span>
                      <span class="text-control leading-none text-console-accent" aria-hidden="true">{entry.current ? "✓" : ""}</span>
                    </div>
                  )}
                </For>
              );
              return (
                <Show when={group.label} fallback={options}>
                  {(label) => (
                    <div role="group" aria-labelledby={headingID} class="not-first:mt-1">
                      <div id={headingID} class={cx(ui.microLabel, "px-2 pb-0.5 pt-1")}>{label()}</div>
                      {options}
                    </div>
                  )}
                </Show>
              );
            }}
          </For>
        </Show>
      </div>
    </div>
  );
}
