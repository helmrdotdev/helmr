import { A } from "@solidjs/router";
import { ActionMenu, type ActionMenuItem } from "./ActionMenu";
import { For, Show, type JSX } from "solid-js";
import { cx, ui } from "./styles";

export type Crumb = { href: string; label: string };

// Every route page starts with it. The first row is a 24px breadcrumb slot, kept
// on pages without a breadcrumb too, so every page's h1 sits at the same height;
// the breadcrumb runs from the parent pages to the current page (the title when
// it is text, or `current`). The title row holds the h1 (one line, truncated with
// the full text in `title`), an optional status badge and, on the right, the
// list page's create action. A detail page has no action buttons: its actions are
// in `menu`, the menu by its name, the same as its row menu in the list. Below sit a one-line
// subtitle and a metadata row (IDs, times). On phones the actions follow the
// metadata.
export function PageHeader(props: {
  title: JSX.Element;
  subtitle?: JSX.Element;
  badge?: JSX.Element;
  breadcrumb?: readonly Crumb[];
  current?: string;
  meta?: JSX.Element;
  actions?: JSX.Element;
  // A detail page's object menu: the same actions as the object's row menu in its list.
  menu?: ActionMenuItem[] | undefined;
  menuLabel?: string;
}) {
  const currentLabel = () => props.current ?? (typeof props.title === "string" ? props.title : undefined);
  return (
    <div class={ui.pageHeader}>
      <div class="col-span-full flex min-h-6 items-center">
        <Show when={props.breadcrumb?.length}>
          <nav class="min-w-0 font-mono text-chrome text-console-muted" aria-label="Breadcrumb">
            {/* Each separator ends the crumb before it, so a wrapped line never starts with one. */}
            <ol class="m-0 flex list-none flex-wrap items-center gap-x-1.5 p-0">
              <For each={props.breadcrumb}>
                {(crumb) => (
                  <li class="flex items-center gap-1.5">
                    <A href={crumb.href} class="cursor-pointer text-console-accent hover:text-console-accent-hover">{crumb.label}</A>
                    <Show when={currentLabel() !== undefined}><span class="text-console-subtle" aria-hidden="true">/</span></Show>
                  </li>
                )}
              </For>
              <Show when={currentLabel()}>
                {(label) => (
                  <li class="flex min-w-0 items-center">
                    <span class="truncate text-console-text" aria-current="page">{label()}</span>
                  </li>
                )}
              </Show>
            </ol>
          </nav>
        </Show>
      </div>
      <div class={cx(ui.pageTitle, "col-start-1 min-w-0")}>
        <h1 class={cx(ui.h1, "min-w-0 truncate")} title={typeof props.title === "string" ? props.title : undefined}>{props.title}</h1>
        <Show when={props.menu?.length}><ActionMenu items={props.menu!} trigger="chevron" label={props.menuLabel ?? `Actions for ${typeof props.title === "string" ? props.title : "this page"}`} /></Show>
        <Show when={props.badge}><span class="shrink-0">{props.badge}</span></Show>
      </div>
      <Show when={props.subtitle}>
        <div class={cx(ui.pageSubtitle, "col-start-1")}>{props.subtitle}</div>
      </Show>
      <Show when={props.meta}>
        <div class="col-start-1 mt-1.5 flex flex-wrap items-center gap-x-4 gap-y-1 font-mono text-chrome text-console-muted">{props.meta}</div>
      </Show>
      <Show when={props.actions}>
        <div class="col-start-2 row-start-2 flex flex-wrap items-center justify-end gap-2 self-center max-sm:col-start-1 max-sm:row-start-auto max-sm:mt-3 max-sm:justify-start">{props.actions}</div>
      </Show>
    </div>
  );
}
