import { A } from "@solidjs/router";
import { For, Show, type JSX } from "solid-js";
import { ui } from "./styles";

export type Crumb = { href: string; label: string };

// Every route page starts with it: breadcrumb from the parent pages to the
// current page (the title when it is text, or `current`), title with an
// optional status badge, a one-line subtitle, a metadata row (IDs, times) and the
// page's actions (Copy link first, the primary action last).
export function PageHeader(props: {
  title: JSX.Element;
  subtitle?: JSX.Element;
  badge?: JSX.Element;
  breadcrumb?: readonly Crumb[];
  current?: string;
  meta?: JSX.Element;
  actions?: JSX.Element;
}) {
  const currentLabel = () => props.current ?? (typeof props.title === "string" ? props.title : undefined);
  return (
    <div class={ui.pageHeader}>
      <div class="min-w-0">
        <Show when={props.breadcrumb?.length}>
          <nav class="mb-2 font-mono text-chrome text-console-muted" aria-label="Breadcrumb">
            <ol class="m-0 flex list-none flex-wrap items-center gap-1.5 p-0">
              <For each={props.breadcrumb}>
                {(crumb, index) => (
                  <li class="flex items-center gap-1.5">
                    <Show when={index() > 0}><span class="text-console-subtle" aria-hidden="true">/</span></Show>
                    <A href={crumb.href} class="cursor-pointer text-console-accent hover:text-console-accent-hover">{crumb.label}</A>
                  </li>
                )}
              </For>
              <Show when={currentLabel()}>
                {(label) => (
                  <li class="flex min-w-0 items-center gap-1.5">
                    <span class="text-console-subtle" aria-hidden="true">/</span>
                    <span class="truncate text-console-text" aria-current="page">{label()}</span>
                  </li>
                )}
              </Show>
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
