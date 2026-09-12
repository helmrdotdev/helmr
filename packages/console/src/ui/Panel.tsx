import type { JSX } from "solid-js";
import { cx, ui } from "./styles";

export function Panel(props: { title: string; children: JSX.Element; class?: string }) {
  return (
    <section class={cx("border border-console-border bg-console-surface p-4", props.class)}>
      <h2 class={cx(ui.h2, "mb-3")}>{props.title}</h2>
      {props.children}
    </section>
  );
}

export function DetailList(props: { title: string; children: JSX.Element }) {
  return (
    <aside class="sticky top-13.5 border border-console-border bg-console-surface px-4 py-3.5 max-[960px]:static">
      <h3 class={cx(ui.h3, "mb-3.5")}>{props.title}</h3>
      <dl class="m-0 grid gap-2.5">{props.children}</dl>
    </aside>
  );
}

export function DetailItem(props: { label: string; children: JSX.Element }) {
  return (
    <div class="grid gap-0.75">
      <dt class="m-0 font-mono text-[10px] font-medium uppercase tracking-[0.06em] text-console-subtle">{props.label}</dt>
      <dd class="m-0 text-[12.5px] text-console-text [overflow-wrap:anywhere]">{props.children}</dd>
    </div>
  );
}
