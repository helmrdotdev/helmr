import { createEffect, createSignal, createUniqueId, on, onCleanup, Show, type JSX } from "solid-js";
import { Portal } from "solid-js/web";
import type { DismissReason } from "./dismiss";
import { ContainerHeader } from "./Panel";
import { placePopover } from "./position";
import { cx, ui } from "./styles";
import { useDismiss } from "./useDismiss";

const PHONE_WIDTH = 720;

// Open panels and the control that opened each, so a dialog opened from a menu
// item returns focus to the menu's trigger rather than to the vanished item.
const anchors = new WeakMap<Element, HTMLElement>();

// Where focus should return after something opened from `element` closes: the
// popover's anchor when `element` sits inside a popover, otherwise `element`.
export function focusReturnTarget(element: Element | null): HTMLElement | null {
  const panel = element?.closest("[data-popover]");
  const anchor = panel ? anchors.get(panel) : undefined;
  if (anchor) return anchor;
  return element instanceof HTMLElement ? element : null;
}

const FOCUSABLE = "a[href], button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea:not(:disabled), [tabindex]:not([tabindex='-1'])";

// The keyboard-reachable controls inside `root`, in tab order.
export function focusableIn(root: Element): HTMLElement[] {
  return Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE)).filter(
    (element) => element.tabIndex >= 0 && !element.closest("[hidden], [inert]") && element.getClientRects().length > 0,
  );
}

// The one floating panel: the container border and, with a title, the container
// header; no shadow. Placed against its anchor. Escape, a press outside, Tab, or
// focus moving elsewhere closes it. Focus returns to the anchor on every close except a press
// outside or focus moving elsewhere. Menus, pickers and the profile menu render
// inside it.
export function Popover(props: {
  open: boolean;
  anchor: HTMLElement | undefined;
  onDismiss: (reason: DismissReason) => void;
  children: JSX.Element;
  id?: string;
  role?: "menu" | "dialog" | "listbox" | "group";
  label?: string;
  // The container header; untitled popovers and menus have none.
  title?: string;
  align?: "start" | "end";
  // Fixed width, or `anchor` to be at least as wide as the anchor.
  width?: number | "anchor";
  minWidth?: number;
  maxWidth?: number;
  maxHeight?: number;
  // Below 720px the panel spans the viewport instead of following the anchor.
  fullWidthOnPhones?: boolean;
  class?: string;
  bodyClass?: string;
  ref?: (element: HTMLDivElement) => void;
  onKeyDown?: (event: KeyboardEvent) => void;
}) {
  const titleID = createUniqueId();
  const [style, setStyle] = createSignal<JSX.CSSProperties>({});
  let panel: HTMLDivElement | undefined;
  let closedBy: DismissReason | undefined;

  const dismiss = (reason: DismissReason) => {
    closedBy = reason;
    props.onDismiss(reason);
  };

  useDismiss({
    open: () => props.open,
    contains: (target) => !!panel?.contains(target) || !!props.anchor?.contains(target),
    onDismiss: dismiss,
  });

  createEffect(
    on(
      () => props.open,
      (open, wasOpen) => {
        if (open || !wasOpen) return;
        const reason = closedBy;
        closedBy = undefined;
        if (reason !== "outside" && reason !== "blur") props.anchor?.focus();
      },
      { defer: true },
    ),
  );

  const position = () => {
    const anchor = props.anchor;
    if (!anchor || !props.open) return;
    const rect = anchor.getBoundingClientRect();
    const viewport = { width: window.innerWidth, height: window.innerHeight };
    const maxHeight = props.maxHeight ?? 260;
    if (props.fullWidthOnPhones && viewport.width <= PHONE_WIDTH) {
      const top = Math.max(8, Math.min(rect.bottom + 6, viewport.height - 80));
      setStyle({ top: `${top}px`, left: "10px", right: "10px", "max-height": `calc(100dvh - ${top + 10}px)` });
      return;
    }
    const minWidth = props.width === "anchor" ? Math.ceil(rect.width) : props.minWidth;
    const width = typeof props.width === "number" ? props.width : undefined;
    const placement = placePopover({
      anchor: rect,
      width: width ?? panel?.offsetWidth ?? minWidth ?? 0,
      height: panel?.scrollHeight ?? maxHeight,
      viewport,
      align: props.align ?? "start",
      maxHeight,
    });
    setStyle({
      top: `${placement.top}px`,
      "max-height": `${placement.maxHeight}px`,
      ...(placement.left !== undefined ? { left: `${placement.left}px` } : { right: `${placement.right}px` }),
      ...(width !== undefined ? { width: `${width}px` } : {}),
      ...(minWidth !== undefined ? { "min-width": `${minWidth}px` } : {}),
      ...(props.maxWidth !== undefined ? { "max-width": `${props.maxWidth}px` } : { "max-width": "calc(100vw - 16px)" }),
    });
  };

  createEffect(() => {
    if (!props.open) return;
    position();
    // Measure again once the content has rendered at its natural size.
    const frame = requestAnimationFrame(position);
    window.addEventListener("resize", position);
    window.addEventListener("scroll", position, true);
    onCleanup(() => {
      cancelAnimationFrame(frame);
      window.removeEventListener("resize", position);
      window.removeEventListener("scroll", position, true);
    });
  });

  // Tab leaves a menu or picker at once. In a dialog-like panel it moves between
  // the panel's controls and leaves past the first or last one.
  const leavesPanel = (event: KeyboardEvent) => {
    if (props.role !== "dialog") return true;
    const controls = panel ? focusableIn(panel) : [];
    const edge = event.shiftKey ? controls[0] : controls[controls.length - 1];
    return controls.length === 0 || document.activeElement === edge;
  };

  const onKeyDown = (event: KeyboardEvent) => {
    if (event.key === "Tab" && leavesPanel(event)) {
      // Continue from the trigger, which also keeps focus inside a surrounding dialog.
      event.preventDefault();
      event.stopPropagation();
      dismiss("tab");
      return;
    }
    props.onKeyDown?.(event);
  };

  const onFocusOut = (event: FocusEvent) => {
    const next = event.relatedTarget;
    if (!(next instanceof Node)) return;
    if (panel?.contains(next) || props.anchor?.contains(next)) return;
    dismiss("blur");
  };

  return (
    <Show when={props.open}>
      <Portal>
        <div
          ref={(element) => {
            panel = element;
            if (props.anchor) anchors.set(element, props.anchor);
            props.ref?.(element);
          }}
          id={props.id}
          role={props.role}
          aria-label={props.label}
          data-popover
          class={cx(ui.popover, props.class)}
          style={style()}
          onKeyDown={onKeyDown}
          onFocusOut={onFocusOut}
        >
          <Show when={props.title}>{(title) => <ContainerHeader id={titleID} title={title()} />}</Show>
          <div class={cx("min-h-0 overflow-y-auto", props.bodyClass)}>{props.children}</div>
        </div>
      </Portal>
    </Show>
  );
}
