import { Show, type JSX } from "solid-js";
import { cx } from "./styles";

// Non-tabular rows (turns, holds, delivery issues, linked identities) with one
// divider style and 12px padding. `footer` renders under the rows inside the
// frame (see LoadMore).
export function List(props: { children: JSX.Element; label?: string; footer?: JSX.Element; class?: string }) {
  return (
    <div class={cx("border border-console-border bg-console-surface", props.class)}>
      <ul class="m-0 list-none divide-y divide-console-border-soft p-0" aria-label={props.label}>
        {props.children}
      </ul>
      <Show when={props.footer}>{props.footer}</Show>
    </div>
  );
}

export function ListItem(props: { children: JSX.Element; id?: string; class?: string }) {
  return <li id={props.id} class={cx("p-3 text-body", props.class)}>{props.children}</li>;
}
