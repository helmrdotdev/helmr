import { Show, type JSX } from "solid-js";
import { cx, ui } from "./styles";

// Non-tabular rows (turns, holds, delivery issues, linked identities) with one
// divider style. `footer` renders under the rows inside the
// frame (see LoadMore). Like a table, it has no frame of its own as a direct child
// of a Panel body.
export function List(props: { children: JSX.Element; label?: string; footer?: JSX.Element; class?: string }) {
  return (
    <div class={cx(ui.container, props.class)} data-list-frame>
      <ul class="m-0 list-none divide-y divide-console-border-soft p-0" aria-label={props.label}>
        {props.children}
      </ul>
      <Show when={props.footer}>{props.footer}</Show>
    </div>
  );
}

// A row is one of two kinds. A single-line row (`line`) is 36px like a table
// row: one flex line, 4px vertical padding so a 24px compact button fits. A
// multi-block row (a Turn, a hold) pads 12px all round.
export function ListItem(props: { children: JSX.Element; id?: string; class?: string; label?: string; line?: boolean }) {
  return (
    <li id={props.id} class={cx("text-body", props.line ? "flex min-h-9 items-center gap-3 px-3 py-1" : "p-3", props.class)} aria-label={props.label}>
      {props.children}
    </li>
  );
}
