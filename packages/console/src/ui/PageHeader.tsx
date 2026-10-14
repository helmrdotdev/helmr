import { A } from "@solidjs/router";
import { For, Show, type JSX } from "solid-js";
import { ui } from "./styles";

export type Crumb = { href: string; label: string };

// Every route page starts with it: breadcrumb to the parent pages, title with an
// optional status badge, a one-line subtitle, a metadata row (IDs, times) and the
// page's actions (Copy link first, the primary action last).
export function PageHeader(props: {
  title: JSX.Element;
  subtitle?: JSX.Element;
  badge?: JSX.Element;
  breadcrumb?: readonly Crumb[];
  meta?: JSX.Element;
  actions?: JSX.Element;
}) {
  return (
    <div class={ui.pageHeader}>
      <div class="min-w-0">
        <Show when={props.breadcrumb?.length}>
          <nav class="mb-2 font-mono text-chrome text-console-muted" aria-label="Breadcrumb">
            <ol class="m-0 flex list-none flex-wrap items-center gap-1.5 p-0">
              <For each={props.breadcrumb}>
                {(crumb) => (
                  <li class="flex items-center gap-1.5">
                    <A href={crumb.href} class="cursor-pointer text-console-accent hover:text-console-accent-hover">{crumb.label}</A>
                    <span class="text-console-subtle" aria-hidden="true">/</span>
                  </li>
                )}
              </For>
            </ol>
          </nav>
        </Show>
        <div class={ui.pageTitle}>
          <h1 class={ui.h1}>{props.title}</h1>
          {props.badge}
        </div>
        <Show when={props.subtitle}>
          <div class={ui.pageSubtitle}>{props.subtitle}</div>
        </Show>
        <Show when={props.meta}>
          <div class="mt-1.5 flex flex-wrap items-center gap-x-4 gap-y-1 font-mono text-chrome text-console-muted">{props.meta}</div>
        </Show>
      </div>
      <Show when={props.actions}>
        <div class="flex shrink-0 flex-wrap items-center gap-2">{props.actions}</div>
      </Show>
    </div>
  );
}
