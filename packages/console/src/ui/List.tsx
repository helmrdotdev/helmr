import type { JSX } from "solid-js";
import { cx } from "./styles";

// Non-tabular rows (turns, holds, delivery issues, linked identities) with one
// divider style and 12px padding.
export function List(props: { children: JSX.Element; label?: string; class?: string }) {
  return (
    <ul class={cx("m-0 list-none divide-y divide-console-border-soft border border-console-border bg-console-surface p-0", props.class)} aria-label={props.label}>
      {props.children}
    </ul>
  );
}

export function ListItem(props: { children: JSX.Element; class?: string }) {
  return <li class={cx("p-3 text-body", props.class)}>{props.children}</li>;
}
