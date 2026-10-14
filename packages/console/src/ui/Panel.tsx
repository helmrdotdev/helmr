import { children, Show, type JSX } from "solid-js";
import { cx, ui } from "./styles";

// A bordered block with an optional title and actions; its blocks sit 12px apart.
// The `window` variant puts the title in a title strip and adds the hard shadow,
// for anything that frames a task.
export function Panel(props: {
  title?: string;
  actions?: JSX.Element;
  variant?: "panel" | "window";
  children: JSX.Element;
  class?: string;
}) {
  return (
    <Show
      when={props.variant === "window"}
      fallback={
        <section class={cx("flex flex-col gap-3 border border-console-border bg-console-surface p-4", props.class)}>
          <Show when={props.title || props.actions}>
            <div class="flex items-center justify-between gap-3">
              <Show when={props.title}><h2 class={ui.h2}>{props.title}</h2></Show>
              <Show when={props.actions}><div class={ui.buttonGroup}>{props.actions}</div></Show>
            </div>
          </Show>
          {props.children}
        </section>
      }
    >
      <section class={cx(ui.window, props.class)} aria-label={props.title}>
        <div class={ui.titleStrip}>
          <h2 class="m-0 min-w-0 flex-1 truncate font-mono text-micro font-normal">{props.title}</h2>
          <Show when={props.actions}><div class="flex items-center gap-1">{props.actions}</div></Show>
        </div>
        <div class={cx(ui.windowStack, "p-3")}>{props.children}</div>
      </section>
    </Show>
  );
}

// A detail page: the main column, whose sections stack 24px apart, and a 300px
// rail that moves under it on narrow screens.
export function DetailLayout(props: { children: JSX.Element; rail: JSX.Element }) {
  return (
    <div class="grid grid-cols-[minmax(0,1fr)_300px] items-start gap-3.5 max-[960px]:grid-cols-1">
      <div class={cx(ui.sectionStack, "min-w-0")}>{props.children}</div>
      {props.rail}
    </div>
  );
}

// The rail's label/value pairs.
export function KeyValueList(props: { title: string; children: JSX.Element }) {
  return (
    <aside class="sticky top-13.5 border border-console-border bg-console-surface px-4 py-3.5 max-[960px]:static">
      <h3 class={cx(ui.h3, "mb-3.5")}>{props.title}</h3>
      <dl class="m-0 grid gap-2.5">{props.children}</dl>
    </aside>
  );
}

// One label/value pair. A missing value renders "—".
export function KeyValue(props: { label: string; children?: JSX.Element }) {
  const value = children(() => props.children);
  const empty = () => {
    const resolved = value();
    return resolved === undefined || resolved === null || resolved === "" || (Array.isArray(resolved) && resolved.length === 0);
  };
  return (
    <div class="grid gap-0.75">
      <dt class={cx("m-0", ui.microLabel)}>{props.label}</dt>
      <dd class="m-0 text-body text-console-text [overflow-wrap:anywhere]">
        <Show when={!empty()} fallback={<span class={ui.emptyCell}>—</span>}>{value()}</Show>
      </dd>
    </div>
  );
}
