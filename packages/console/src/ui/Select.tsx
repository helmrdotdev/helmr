import { createMemo, createSignal, createUniqueId, For, onCleanup, Show, type JSX } from "solid-js";
import { Icon } from "./Icon";
import { useFieldControl } from "./Field";
import { Popover } from "./Popover";
import { cx, ui } from "./styles";

export type SelectOption<T extends string> = {
  value: T;
  label: string;
  // Tags after the label in a fixed trailing column (a working tag), also
  // shown in the trigger for the chosen option.
  trailing?: () => JSX.Element;
};

// Past this many options the list gets a filter box.
const SEARCHABLE_AFTER = 10;

// The only select in the console: a combobox button that opens a listbox in a
// popover. Inside a `Field` it takes the field's id, label, hint and error; with
// `name` it also submits its value with the surrounding form.
export function Select<T extends string>(props: {
  value: T;
  options: readonly SelectOption<T>[];
  onChange: (value: T) => void;
  disabled?: boolean;
  placeholder?: string;
  ariaLabel?: string;
  name?: string;
  minWidth?: string;
  align?: "start" | "end";
}) {
  const field = useFieldControl();
  const [open, setOpen] = createSignal(false);
  const [query, setQuery] = createSignal("");
  const [activeIndex, setActiveIndex] = createSignal(0);
  const [trigger, setTrigger] = createSignal<HTMLButtonElement>();
  let optionRefs: HTMLButtonElement[] = [];
  let searchRef: HTMLInputElement | undefined;
  let typeahead = "";
  let typeaheadReset: number | undefined;
  const listboxID = createUniqueId();
  const searchable = () => props.options.length > SEARCHABLE_AFTER;
  const selected = createMemo(() => props.options.find((option) => option.value === props.value));
  const visible = createMemo(() => {
    const q = query().trim().toLowerCase();
    if (!q) return props.options;
    return props.options.filter((option) => option.label.toLowerCase().includes(q) || option.value.toLowerCase().includes(q));
  });

  const focusOption = (index: number) => {
    const last = visible().length - 1;
    if (last < 0) return;
    const next = Math.max(0, Math.min(index, last));
    setActiveIndex(next);
    queueMicrotask(() => optionRefs[next]?.focus());
  };

  const openList = () => {
    if (props.disabled || props.options.length === 0) return;
    typeahead = "";
    setQuery("");
    setOpen(true);
    const index = Math.max(0, props.options.findIndex((option) => option.value === props.value));
    if (searchable()) {
      setActiveIndex(index);
      queueMicrotask(() => searchRef?.focus());
    } else {
      focusOption(index);
    }
  };

  const close = () => {
    setOpen(false);
    queueMicrotask(() => trigger()?.focus());
  };

  const choose = (index: number) => {
    const option = visible()[index];
    if (!option) return;
    props.onChange(option.value);
    close();
  };

  const focusByPrefix = (key: string) => {
    window.clearTimeout(typeaheadReset);
    typeahead = `${typeahead}${key}`.toLowerCase();
    typeaheadReset = window.setTimeout(() => {
      typeahead = "";
    }, 700);
    const options = visible();
    const start = activeIndex() + 1;
    const match = [...options.slice(start), ...options.slice(0, start)].findIndex((option) =>
      option.label.toLowerCase().startsWith(typeahead),
    );
    if (match >= 0) focusOption((start + match) % options.length);
  };

  const printable = (event: KeyboardEvent) =>
    event.key.length === 1 && event.key !== " " && !event.metaKey && !event.ctrlKey && !event.altKey;

  const onTriggerKeyDown = (event: KeyboardEvent) => {
    if (printable(event) && !searchable()) {
      event.preventDefault();
      openList();
      focusByPrefix(event.key);
      return;
    }
    if (["ArrowDown", "ArrowUp", "Enter", " "].includes(event.key)) {
      event.preventDefault();
      if (open()) setOpen(false);
      else openList();
    }
  };

  const onListKeyDown = (event: KeyboardEvent) => {
    const inSearch = event.target === searchRef;
    if (printable(event) && !inSearch) {
      event.preventDefault();
      if (searchable()) {
        filter(`${query()}${event.key}`);
        searchRef?.focus();
      } else {
        focusByPrefix(event.key);
      }
      return;
    }
    switch (event.key) {
      case "ArrowDown":
        event.preventDefault();
        focusOption(inSearch ? 0 : activeIndex() + 1);
        break;
      case "ArrowUp":
        event.preventDefault();
        if (!inSearch && activeIndex() === 0 && searchable()) searchRef?.focus();
        else focusOption(activeIndex() - 1);
        break;
      case "Home":
        if (inSearch) return;
        event.preventDefault();
        focusOption(0);
        break;
      case "End":
        if (inSearch) return;
        event.preventDefault();
        focusOption(visible().length - 1);
        break;
      case "Enter":
        event.preventDefault();
        choose(inSearch ? 0 : activeIndex());
        break;
      case " ":
        if (inSearch) return;
        event.preventDefault();
        choose(activeIndex());
        break;
    }
  };

  const filter = (value: string) => {
    setQuery(value);
    setActiveIndex(0);
  };

  onCleanup(() => window.clearTimeout(typeaheadReset));

  return (
    <div class="relative inline-block" style={props.minWidth ? { "min-width": props.minWidth } : undefined}>
      <button
        ref={setTrigger}
        id={field?.id}
        type="button"
        role="combobox"
        class={ui.selectTrigger}
        data-open={open() ? "true" : "false"}
        disabled={props.disabled}
        aria-haspopup="listbox"
        aria-expanded={open()}
        aria-controls={open() ? listboxID : undefined}
        aria-label={props.ariaLabel}
        aria-labelledby={props.ariaLabel ? undefined : field?.labelID}
        aria-describedby={field?.describedBy()}
        aria-invalid={field?.invalid() || undefined}
        onClick={() => (open() ? setOpen(false) : openList())}
        onKeyDown={onTriggerKeyDown}
      >
        <span class="flex min-w-0 items-center gap-1.5">
          <span class="overflow-hidden text-ellipsis whitespace-nowrap">{selected()?.label ?? props.placeholder ?? "Select…"}</span>
          {selected()?.trailing?.()}
        </span>
        <span class={cx("grid size-4 place-items-center transition-transform", open() && "rotate-180")} aria-hidden="true">
          <span class="block size-1.5 -translate-y-px rotate-45 border-b border-r border-console-muted" />
        </span>
      </button>
      <Show when={props.name}>
        {(name) => <input type="hidden" name={name()} value={props.value} />}
      </Show>
      <Popover
        open={open()}
        anchor={trigger()}
        onDismiss={() => setOpen(false)}
        align={props.align ?? "start"}
        width="anchor"
        bodyClass="p-0.75"
        onKeyDown={onListKeyDown}
      >
        <Show when={searchable()}>
          <input
            ref={searchRef}
            class={cx(ui.input, "mb-0.75 h-6 min-h-6")}
            type="search"
            placeholder="Filter…"
            aria-label={`Filter ${props.ariaLabel ?? "options"}`}
            aria-controls={listboxID}
            value={query()}
            onInput={(event) => filter(event.currentTarget.value)}
            autocomplete="off"
            spellcheck={false}
          />
        </Show>
        <ul
          id={listboxID}
          class="m-0 list-none p-0"
          role="listbox"
          aria-label={props.ariaLabel}
          aria-labelledby={props.ariaLabel ? undefined : field?.labelID}
        >
          <Show when={visible().length > 0} fallback={<li class="px-2 py-1.5 text-control text-console-subtle">No matches</li>}>
            <For each={visible()}>
              {(option, index) => (
                <li>
                  <button
                    ref={(element) => {
                      optionRefs[index()] = element;
                    }}
                    type="button"
                    class={cx(ui.selectOption, option.value === props.value && ui.selectedOption)}
                    role="option"
                    aria-selected={option.value === props.value}
                    tabIndex={index() === activeIndex() ? 0 : -1}
                    onMouseEnter={() => setActiveIndex(index())}
                    onFocus={() => setActiveIndex(index())}
                    onClick={() => choose(index())}
                  >
                    <span class="grid place-items-center text-console-accent">
                      {option.value === props.value ? <Icon name="check" /> : null}
                    </span>
                    <span class="overflow-hidden text-ellipsis whitespace-nowrap">{option.label}</span>
                    <span class="flex items-center justify-end gap-1.5">{option.trailing?.()}</span>
                  </button>
                </li>
              )}
            </For>
          </Show>
        </ul>
      </Popover>
    </div>
  );
}
