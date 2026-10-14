import { Show, type JSX } from "solid-js";
import { cx, ui } from "./styles";

// Non-tabular rows (turns, holds, delivery issues, linked identities) with one
// divider style and 12px padding. `footer` renders under the rows inside the
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

export function ListItem(props: { children: JSX.Element; id?: string; class?: string; label?: string }) {
  return <li id={props.id} class={cx("p-3 text-body", props.class)} aria-label={props.label}>{props.children}</li>;
}
