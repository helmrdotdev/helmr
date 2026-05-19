import { createSignal, createUniqueId, For, onCleanup, onMount, Show } from "solid-js";
import { cx, ui } from "./styles";

export type ActionMenuItem = {
  label: string;
  busyLabel?: string | undefined;
  disabled?: boolean | undefined;
  href?: string | undefined;
  external?: boolean | undefined;
  tone?: "default" | "danger";
  onSelect?: () => void;
};

export function ActionMenu(props: {
  items: ActionMenuItem[];
  label?: string;
}) {
  const [open, setOpen] = createSignal(false);
  const [menuStyle, setMenuStyle] = createSignal<Record<string, string>>({});
  const popoverId = createUniqueId();
  let wrapperRef: HTMLDivElement | undefined;
  let buttonRef: HTMLButtonElement | undefined;

  const updateMenuPosition = () => {
    if (!buttonRef) return;
    const rect = buttonRef.getBoundingClientRect();
    const menuWidth = 180;
    const menuHeight = Math.min(props.items.length * 31 + 8, 260);
    const gap = 6;
    const left = Math.min(Math.max(8, rect.right - menuWidth), window.innerWidth - menuWidth - 8);
    const opensUp = rect.bottom + gap + menuHeight > window.innerHeight && rect.top - gap - menuHeight > 8;
    const top = opensUp ? rect.top - gap - menuHeight : rect.bottom + gap;
    setMenuStyle({
      left: `${left}px`,
      top: `${Math.max(8, top)}px`,
      width: `${menuWidth}px`,
    });
  };

  const openMenu = () => {
    updateMenuPosition();
    setOpen(true);
  };

  const closeMenu = (options?: { restoreFocus?: boolean }) => {
    const wasOpen = open();
    setOpen(false);
    if (options?.restoreFocus && wasOpen) buttonRef?.focus();
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

  return (
    <div class="relative inline-flex justify-end" ref={wrapperRef}>
      <button
        ref={buttonRef}
        type="button"
        class={ui.actionMenuTrigger}
        data-open={open() ? "true" : "false"}
        aria-controls={popoverId}
        aria-expanded={open()}
        aria-label={props.label ?? "Open row actions"}
        onClick={() => {
          if (open()) {
            closeMenu();
          } else {
            openMenu();
          }
        }}
      >
        <span aria-hidden="true">Action</span>
        <span class={ui.actionMenuCaret} aria-hidden="true" />
      </button>

      <Show when={open()}>
        <div
          class={ui.actionMenu}
          id={popoverId}
          style={menuStyle()}
          role="group"
          aria-label={props.label ?? "Row actions"}
        >
          <For each={props.items}>
            {(item) => {
              const label = () => item.busyLabel ?? item.label;
              const itemClass = () => cx(
                ui.actionMenuItem,
                item.tone === "danger" && ui.actionMenuItemDanger,
              );
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
            }}
          </For>
        </div>
      </Show>
    </div>
  );
}
