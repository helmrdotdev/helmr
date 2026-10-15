import { children, createUniqueId, Show, type JSX } from "solid-js";
import { cx, ui } from "./styles";

// The one header of an in-page container: 32px, title, optional count, actions
// on the right (compact buttons, `ui.compactButton`).
export function ContainerHeader(props: { id: string; title: string; count?: number | undefined; actions?: JSX.Element; tone?: "danger" }) {
  return (
    <div class={props.tone === "danger" ? ui.containerHeaderDanger : ui.containerHeader}>
      <h2 class={cx(ui.containerTitle, props.tone === "danger" ? "text-console-danger" : "text-console-text")}>
        <span id={props.id}>{props.title}</span>
        <Show when={props.count !== undefined}><span class={ui.containerCount}>{props.count}</span></Show>
      </h2>
      <Show when={props.actions}><div class={ui.containerActions}>{props.actions}</div></Show>
    </div>
  );
}

// The in-page container: one border, one header when titled, and a body whose
// blocks sit 12px apart. A description belongs on the body's first line. A table
// or list that is a direct child of the body runs flush to its edges with no frame
// of its own (`ui.containerBody`). The body is never rendered empty: when its
// content renders nothing (a query whose skeleton has not appeared yet), only
// the header shows. A Panel with nothing to say is not rendered by its page.
export function Panel(props: {
  title?: string;
  count?: number | undefined;
  actions?: JSX.Element;
  children: JSX.Element;
  class?: string;
}) {
  const id = createUniqueId();
  const body = children(() => props.children);
  const hasBody = () => body.toArray().some((node) => node !== undefined && node !== null && node !== false && node !== "");
  return (
    <section class={cx(ui.container, props.class)} aria-labelledby={props.title ? id : undefined}>
      <Show when={props.title}>{(title) => <ContainerHeader id={id} title={title()} count={props.count} actions={props.actions} />}</Show>
      <Show when={hasBody()}>
        <div class={ui.containerBody}>{body()}</div>
      </Show>
    </section>
  );
}

// A detail page: the main column, whose sections stack 24px apart, and a 300px
// rail that moves under it on narrow screens.
export function DetailLayout(props: { children: JSX.Element; rail: JSX.Element }) {
  return (
    <div class="grid grid-cols-[minmax(0,1fr)_300px] items-start gap-6 max-[960px]:grid-cols-1">
      <div class={cx(ui.sectionStack, "min-w-0")}>{props.children}</div>
      {props.rail}
    </div>
  );
}

// The rail's label/value pairs, under the container header.
export function KeyValueList(props: { title: string; children: JSX.Element }) {
  const id = createUniqueId();
  return (
    <aside class={cx(ui.container, "sticky top-13.5 max-[960px]:static")} aria-labelledby={id}>
      <ContainerHeader id={id} title={props.title} />
      <dl class="m-0 grid gap-2.5 p-3">{props.children}</dl>
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

// A destructive action that belongs to the page: what it does and its one button,
// which opens a ConfirmDialog. Sits last on the page or in the rail. The container
// header in the danger tone, inside the danger border.
export function DangerZone(props: { title: string; children: JSX.Element; action: JSX.Element; class?: string }) {
  const id = createUniqueId();
  return (
    <section class={cx("border border-console-danger-border bg-console-surface", props.class)} aria-labelledby={id}>
      <ContainerHeader id={id} title={props.title} tone="danger" />
      <div class={cx(ui.containerBody, "items-start")}>
        <p class={cx(ui.muted, "m-0")}>{props.children}</p>
        {props.action}
      </div>
    </section>
  );
}
