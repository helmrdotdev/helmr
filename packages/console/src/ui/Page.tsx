import { Show, type JSX } from "solid-js";
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

// A titled part of a page: heading with an optional count and actions, then its
// content. Sections stack 24px apart inside `ui.sectionStack`.
export function Section(props: {
  title: string;
  count?: number | undefined;
  actions?: JSX.Element;
  children: JSX.Element;
  class?: string;
}) {
  return (
    <section class={props.class}>
      <div class="mb-2 flex min-h-7 items-start justify-between gap-3">
        <div class="min-w-0">
          <h2 class={ui.h2}>
            {props.title}
            <Show when={props.count}>
              <span class="ml-2 font-mono text-badge font-medium text-console-subtle">{props.count}</span>
            </Show>
          </h2>
        </div>
        <Show when={props.actions}>
          <div class={cx(ui.buttonGroup, "shrink-0")}>{props.actions}</div>
        </Show>
      </div>
      {props.children}
    </section>
  );
}
