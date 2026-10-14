import type { JSX } from "solid-js";
import { cx, ui } from "./styles";

// Width, padding and vertical rhythm of a route page.
export function Page(props: { children: JSX.Element; class?: string }) {
  return <section class={cx(ui.page, props.class)}>{props.children}</section>;
}

// Above a list: search and filters at the start, secondary then primary actions
// at the end. Filters never live in the page header.
export function Toolbar(props: { start?: JSX.Element; end?: JSX.Element }) {
  return (
    <div class={ui.toolbar}>
      <div class={ui.toolbarSide}>{props.start}</div>
      <div class={ui.toolbarSide}>{props.end}</div>
    </div>
  );
}
