import { A } from "@solidjs/router";
import { Show, type JSX } from "solid-js";
import { ui } from "./styles";

export function PageHeader(props: {
  title: JSX.Element;
  subtitle?: JSX.Element;
  badge?: JSX.Element;
  back?: { href: string; label: string };
  actions?: JSX.Element;
}) {
  return (
    <div class={ui.pageHeader}>
      <div class="min-w-0">
        <Show when={props.back}>
          {(back) => <A href={back().href} class={ui.backLink}>{back().label}</A>}
        </Show>
        <div class={ui.pageTitle}>
          <h1 class={ui.h1}>{props.title}</h1>
          {props.badge}
        </div>
        <Show when={props.subtitle}>
          <div class={ui.pageSubtitle}>{props.subtitle}</div>
        </Show>
      </div>
      <Show when={props.actions}>
        <div class="flex shrink-0 flex-wrap items-center gap-1.5">{props.actions}</div>
      </Show>
    </div>
  );
}
