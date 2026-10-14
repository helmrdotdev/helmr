import { createMemo, createSignal, createUniqueId, For, Show } from "solid-js";
import { Popover } from "./Popover";
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
  const [trigger, setTrigger] = createSignal<HTMLButtonElement>();
  const popoverId = createUniqueId();
  const groups = createMemo(() => groupItems(props.items));
  let menuRef: HTMLDivElement | undefined;

  const menuItems = (): HTMLElement[] => Array.from(menuRef?.querySelectorAll<HTMLElement>(ITEM_SELECTOR) ?? []);

  const focusItem = (index: number) => {
    const items = menuItems();
    if (items.length === 0) return;
    const wrapped = ((index % items.length) + items.length) % items.length;
    items[wrapped]?.focus();
  };

  const openMenu = () => {
    setOpen(true);
    requestAnimationFrame(() => focusItem(0));
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
              setOpen(false);
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
            onClick={() => setOpen(false)}
          >
            {label()}
          </a>
        )}
      </Show>
    );
  };

  return (
    <div class="relative inline-flex justify-end">
      <button
        ref={setTrigger}
        type="button"
        class={ui.iconButton}
        data-open={open() ? "true" : "false"}
        aria-controls={popoverId}
        aria-expanded={open()}
        aria-haspopup="true"
        aria-label={props.label ?? "Actions"}
        onClick={() => (open() ? setOpen(false) : openMenu())}
      >
        <svg class="size-4" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
          <circle cx="3" cy="8" r="1.4" />
          <circle cx="8" cy="8" r="1.4" />
          <circle cx="13" cy="8" r="1.4" />
        </svg>
      </button>

      <Popover
        open={open()}
        anchor={trigger()}
        onDismiss={() => setOpen(false)}
        id={popoverId}
        role="group"
        label={props.label ?? "Actions"}
        align="end"
        minWidth={160}
        maxWidth={240}
        bodyClass="p-0.75"
        ref={(element) => (menuRef = element)}
        onKeyDown={onMenuKeyDown}
      >
        <For each={groups().main}>{renderItem}</For>
        <Show when={groups().danger.length > 0 && groups().main.length > 0}>
          <div class={ui.actionMenuSeparator} role="separator" />
        </Show>
        <For each={groups().danger}>{renderItem}</For>
      </Popover>
    </div>
  );
}
