import { createEffect, createMemo, createSignal, createUniqueId, For, onCleanup, onMount, Show } from "solid-js";
import { Portal } from "solid-js/web";
import { cx, ui } from "./styles";

export type SelectOption<T extends string> = {
  value: T;
  label: string;
  hint?: string;
};

export function Select<T extends string>(props: {
  value: T;
  options: readonly SelectOption<T>[];
  onChange: (value: T) => void;
  disabled?: boolean;
  placeholder?: string;
  ariaLabel?: string;
  minWidth?: string;
  align?: "start" | "end";
}) {
  const [open, setOpen] = createSignal(false);
  const [activeIndex, setActiveIndex] = createSignal(0);
  const [menuStyle, setMenuStyle] = createSignal<Record<string, string>>({});
  let wrapperRef: HTMLDivElement | undefined;
  let buttonRef: HTMLButtonElement | undefined;
  let listboxRef: HTMLUListElement | undefined;
  let optionRefs: HTMLButtonElement[] = [];
  let typeahead = "";
  let typeaheadReset: number | undefined;
  const selected = createMemo(() => props.options.find((option) => option.value === props.value));
  const selectedIndex = createMemo(() => {
    const index = props.options.findIndex((option) => option.value === props.value);
    return index >= 0 ? index : 0;
  });
  const listboxID = createUniqueId();

  const focusOption = (index: number) => {
    const last = props.options.length - 1;
    if (last < 0) return;
    const next = Math.max(0, Math.min(index, last));
    setActiveIndex(next);
    queueMicrotask(() => optionRefs[next]?.focus());
  };

  const openAt = (index: number) => {
    if (props.disabled || props.options.length === 0) return;
    updateMenuPosition();
    setOpen(true);
    focusOption(index);
  };

  const updateMenuPosition = () => {
    if (!buttonRef) return;
    const rect = buttonRef.getBoundingClientRect();
    const viewportPadding = 8;
    const minWidth = Math.ceil(rect.width);
    const measuredWidth = Math.ceil(listboxRef?.offsetWidth ?? minWidth);
    const maxLeft = Math.max(viewportPadding, window.innerWidth - viewportPadding - measuredWidth);
    const spaceBelow = window.innerHeight - rect.bottom - 5 - viewportPadding;
    const spaceAbove = rect.top - 5 - viewportPadding;
    const placeAbove = spaceBelow < 120 && spaceAbove > spaceBelow;
    const maxHeight = Math.max(80, Math.min(260, placeAbove ? spaceAbove : spaceBelow));
    const top = placeAbove
      ? Math.max(viewportPadding, Math.floor(rect.top - maxHeight - 5))
      : Math.ceil(rect.bottom + 5);
    const inlinePosition = props.align === "end"
      ? { right: `${Math.max(viewportPadding, Math.ceil(window.innerWidth - rect.right))}px` }
      : { left: `${Math.min(Math.max(viewportPadding, Math.floor(rect.left)), maxLeft)}px` };
    setMenuStyle({
      top: `${top}px`,
      "max-height": `${maxHeight}px`,
      "min-width": `${minWidth}px`,
      ...inlinePosition,
    });
  };

  const chooseOption = (index: number) => {
    const option = props.options[index];
    if (!option) return;
    props.onChange(option.value);
    setOpen(false);
    queueMicrotask(() => buttonRef?.focus());
  };

  const focusByPrefix = (key: string) => {
    window.clearTimeout(typeaheadReset);
    typeahead = `${typeahead}${key}`.toLowerCase();
    typeaheadReset = window.setTimeout(() => {
      typeahead = "";
    }, 700);
    const start = activeIndex() + 1;
    const options = props.options;
    const match = [...options.slice(start), ...options.slice(0, start)].findIndex((option) =>
      option.label.toLowerCase().startsWith(typeahead),
    );
    if (match < 0) return;
    focusOption((start + match) % options.length);
  };

  const handleTriggerKeyDown = (event: KeyboardEvent) => {
    if (event.key.length === 1 && event.key !== " " && !event.metaKey && !event.ctrlKey && !event.altKey) {
      event.preventDefault();
      openAt(selectedIndex());
      focusByPrefix(event.key);
      return;
    }
    switch (event.key) {
      case "ArrowDown":
      case "ArrowUp":
        event.preventDefault();
        openAt(selectedIndex());
        break;
      case "Enter":
      case " ":
        event.preventDefault();
        if (open()) {
          setOpen(false);
        } else {
          openAt(selectedIndex());
        }
        break;
    }
  };

  const handleListboxKeyDown = (event: KeyboardEvent) => {
    if (event.key.length === 1 && event.key !== " " && !event.metaKey && !event.ctrlKey && !event.altKey) {
      event.preventDefault();
      focusByPrefix(event.key);
      return;
    }
    switch (event.key) {
      case "ArrowDown":
        event.preventDefault();
        focusOption(activeIndex() + 1);
        break;
      case "ArrowUp":
        event.preventDefault();
        focusOption(activeIndex() - 1);
        break;
      case "Home":
        event.preventDefault();
        focusOption(0);
        break;
      case "End":
        event.preventDefault();
        focusOption(props.options.length - 1);
        break;
      case "Enter":
      case " ":
        event.preventDefault();
        chooseOption(activeIndex());
        break;
      case "Escape":
        event.preventDefault();
        setOpen(false);
        queueMicrotask(() => buttonRef?.focus());
        break;
    }
  };

  onMount(() => {
    const onMouseDown = (event: MouseEvent) => {
      const target = event.target as Node;
      if (wrapperRef?.contains(target) || listboxRef?.contains(target)) return;
      setOpen(false);
    };
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape" && open()) {
        event.preventDefault();
        event.stopImmediatePropagation();
        setOpen(false);
        queueMicrotask(() => buttonRef?.focus());
      }
    };
    document.addEventListener("mousedown", onMouseDown);
    document.addEventListener("keydown", onKey);
    window.addEventListener("resize", updateMenuPosition);
    window.addEventListener("scroll", updateMenuPosition, true);
    onCleanup(() => {
      document.removeEventListener("mousedown", onMouseDown);
      document.removeEventListener("keydown", onKey);
      window.removeEventListener("resize", updateMenuPosition);
      window.removeEventListener("scroll", updateMenuPosition, true);
      window.clearTimeout(typeaheadReset);
    });
  });

  createEffect(() => {
    if (open()) updateMenuPosition();
  });

  return (
    <div
      class="relative inline-block"
      ref={wrapperRef}
      style={props.minWidth ? { "min-width": props.minWidth } : undefined}
    >
      <button
        ref={buttonRef}
        type="button"
        class={ui.selectTrigger}
        data-open={open() ? "true" : "false"}
        disabled={props.disabled}
        aria-haspopup="listbox"
        aria-expanded={open()}
        aria-controls={open() ? listboxID : undefined}
        aria-label={props.ariaLabel}
        onClick={() => {
          if (open()) {
            setOpen(false);
          } else {
            openAt(selectedIndex());
          }
        }}
        onKeyDown={handleTriggerKeyDown}
      >
        <span class="overflow-hidden text-ellipsis whitespace-nowrap">
          {selected()?.label ?? props.placeholder ?? "Select…"}
        </span>
        <span
          class={cx(
            "grid size-4 place-items-center transition-transform",
            open() && "rotate-180",
          )}
          aria-hidden="true"
        >
          <span class="block size-1.5 -translate-y-px rotate-45 border-b border-r border-console-muted" />
        </span>
      </button>
      <Portal>
        <Show when={open()}>
          <ul
            ref={listboxRef}
            id={listboxID}
            class={ui.selectMenu}
            style={menuStyle()}
            role="listbox"
            aria-label={props.ariaLabel}
            onKeyDown={handleListboxKeyDown}
          >
            <For each={props.options}>
              {(option, index) => (
                <li>
                  <button
                    ref={(el) => {
                      optionRefs[index()] = el;
                    }}
                    id={`${listboxID}-${index()}`}
                    type="button"
                    class={cx(
                      ui.selectOption,
                      option.value === props.value && ui.selectedOption,
                    )}
                    role="option"
                    aria-selected={option.value === props.value}
                    tabIndex={index() === activeIndex() ? 0 : -1}
                    onMouseEnter={() => setActiveIndex(index())}
                    onFocus={() => setActiveIndex(index())}
                    onClick={() => {
                      chooseOption(index());
                    }}
                  >
                    <span class="text-center text-[12px] font-medium leading-none text-console-accent" aria-hidden="true">
                      {option.value === props.value ? "✓" : ""}
                    </span>
                    <span class="overflow-hidden text-ellipsis whitespace-nowrap">{option.label}</span>
                    <Show when={option.hint}>
                      <span class="font-mono text-[11px] text-console-subtle">{option.hint}</span>
                    </Show>
                  </button>
                </li>
              )}
            </For>
          </ul>
        </Show>
      </Portal>
    </div>
  );
}
