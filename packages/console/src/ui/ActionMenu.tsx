import { createMemo, createSignal, createUniqueId, For, onCleanup, onMount, Show } from "solid-js";
import { actionMenuItemClass, ui } from "./styles";

export type ActionMenuItem = {
  label: string;
  busyLabel?: string | undefined;
  disabled?: boolean | undefined;
  href?: string | undefined;
  external?: boolean | undefined;
  tone?: "default" | "danger";
  onSelect?: () => void;
};

const MENU_MIN_WIDTH = 160;
const MENU_MAX_WIDTH = 240;
const ITEM_HEIGHT = 31;
const SEPARATOR_HEIGHT = 9;
const ITEM_SELECTOR = "button:not(:disabled), a[href]";

// Items render navigation first, then ordinary actions, then the danger group
// after a separator, whatever order the caller passed them in.
function groupItems(items: ActionMenuItem[]): { main: ActionMenuItem[]; danger: ActionMenuItem[] } {
  const danger = items.filter((item) => item.tone === "danger");
  const safe = items.filter((item) => item.tone !== "danger");
  const main = [...safe.filter((item) => item.href), ...safe.filter((item) => !item.href)];
  return { main, danger };
}

export function ActionMenu(props: {
  items: ActionMenuItem[];
  label?: string;
}) {
  const [open, setOpen] = createSignal(false);
  const [menuStyle, setMenuStyle] = createSignal<Record<string, string>>({});
  const popoverId = createUniqueId();
  const groups = createMemo(() => groupItems(props.items));
  let wrapperRef: HTMLDivElement | undefined;
  let buttonRef: HTMLButtonElement | undefined;
  let menuRef: HTMLDivElement | undefined;

  const updateMenuPosition = () => {
    if (!buttonRef) return;
    const rect = buttonRef.getBoundingClientRect();
    const { main, danger } = groups();
    const menuHeight = Math.min(
      (main.length + danger.length) * ITEM_HEIGHT + (danger.length > 0 && main.length > 0 ? SEPARATOR_HEIGHT : 0) + 8,
      260,
    );
    const gap = 6;
    // Right edge stays on the trigger; the width fits the content between the
    // min and max, and the left bound is guarded with the widest possible menu.
    const right = Math.min(window.innerWidth - rect.right, window.innerWidth - MENU_MAX_WIDTH - 8);
    const opensUp = rect.bottom + gap + menuHeight > window.innerHeight && rect.top - gap - menuHeight > 8;
    const top = opensUp ? rect.top - gap - menuHeight : rect.bottom + gap;
    setMenuStyle({
      right: `${Math.max(8, right)}px`,
      top: `${Math.max(8, top)}px`,
    });
  };

  const menuItems = (): HTMLElement[] => Array.from(menuRef?.querySelectorAll<HTMLElement>(ITEM_SELECTOR) ?? []);

  const focusItem = (index: number) => {
    const items = menuItems();
    if (items.length === 0) return;
    const wrapped = ((index % items.length) + items.length) % items.length;
    items[wrapped]?.focus();
  };

  const openMenu = () => {
    updateMenuPosition();
    setOpen(true);
    requestAnimationFrame(() => focusItem(0));
  };

  const closeMenu = (options?: { restoreFocus?: boolean }) => {
    const wasOpen = open();
    setOpen(false);
    if (options?.restoreFocus && wasOpen) buttonRef?.focus();
  };

  const onMenuKeyDown = (event: KeyboardEvent) => {
    const items = menuItems();
    const current = items.indexOf(document.activeElement as HTMLElement);
    switch (event.key) {
      case "ArrowDown":
        event.preventDefault();
        focusItem(current + 1);
        break;
      case "ArrowUp":
        event.preventDefault();
        focusItem(current - 1);
        break;
      case "Home":
        event.preventDefault();
        focusItem(0);
        break;
      case "End":
        event.preventDefault();
        focusItem(items.length - 1);
        break;
      default:
    }
  };

  onMount(() => {
    const onMouseDown = (event: MouseEvent) => {
      if (!wrapperRef?.contains(event.target as Node)) closeMenu();
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (!open() || event.key !== "Escape") return;
      event.preventDefault();
      closeMenu({ restoreFocus: true });
    };
    const onReposition = () => {
      if (open()) closeMenu();
    };
    document.addEventListener("mousedown", onMouseDown);
    document.addEventListener("keydown", onKeyDown);
    window.addEventListener("resize", onReposition);
    window.addEventListener("scroll", onReposition, true);
    onCleanup(() => {
      document.removeEventListener("mousedown", onMouseDown);
      document.removeEventListener("keydown", onKeyDown);
      window.removeEventListener("resize", onReposition);
      window.removeEventListener("scroll", onReposition, true);
    });
  });

  const renderItem = (item: ActionMenuItem) => {
    const label = () => item.busyLabel ?? item.label;
    const itemClass = () => actionMenuItemClass(item.tone);
    return (
      <Show
        when={!item.disabled ? item.href : undefined}
        fallback={
          <button
            type="button"
            class={itemClass()}
            disabled={item.disabled}
            onClick={() => {
              if (item.disabled) return;
              closeMenu();
              item.onSelect?.();
            }}
          >
            {label()}
          </button>
        }
      >
        {(href) => (
          <a
            class={itemClass()}
            href={href()}
            target={item.external ? "_blank" : undefined}
            rel={item.external ? "noreferrer" : undefined}
            onClick={() => closeMenu()}
          >
            {label()}
          </a>
        )}
      </Show>
    );
  };

  return (
    <div class="relative inline-flex justify-end" ref={wrapperRef}>
      <button
        ref={buttonRef}
        type="button"
        class={ui.actionMenuTrigger}
        data-open={open() ? "true" : "false"}
        aria-controls={popoverId}
        aria-expanded={open()}
        aria-haspopup="true"
        aria-label={props.label ?? "Actions"}
        onClick={() => {
          if (open()) {
            closeMenu();
          } else {
            openMenu();
          }
        }}
      >
        <svg class="size-4" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
          <circle cx="3" cy="8" r="1.4" />
          <circle cx="8" cy="8" r="1.4" />
          <circle cx="13" cy="8" r="1.4" />
        </svg>
      </button>

      <Show when={open()}>
        <div
          ref={menuRef}
          class={ui.actionMenu}
          id={popoverId}
          style={{ ...menuStyle(), "min-width": `${MENU_MIN_WIDTH}px`, "max-width": `${MENU_MAX_WIDTH}px` }}
          role="group"
          aria-label={props.label ?? "Actions"}
          onKeyDown={onMenuKeyDown}
        >
          <For each={groups().main}>{renderItem}</For>
          <Show when={groups().danger.length > 0 && groups().main.length > 0}>
            <div class={ui.actionMenuSeparator} role="separator" />
          </Show>
          <For each={groups().danger}>{renderItem}</For>
        </div>
      </Show>
    </div>
  );
}
