import { A, useLocation } from "@solidjs/router";
import { For, Show, type JSX } from "solid-js";
import { cx } from "../ui/styles";

export type NavItem = { href: string; label: string };

const NAV_TAB =
  "relative flex h-6.5 shrink-0 cursor-pointer items-center rounded-xs border border-transparent px-2.5 font-mono text-chrome font-medium text-console-muted transition duration-100";
const NAV_TAB_IDLE = "hover:border-console-border-strong hover:bg-console-surface-muted hover:text-console-text";
const NAV_TAB_ACTIVE =
  "text-console-text after:absolute after:bottom-0.75 after:left-2.5 after:right-2.5 after:h-px after:bg-console-accent";

// A section tab in the bar. It is current on its own path and on every path below
// it ("/" only on itself).
export function NavTab(props: NavItem) {
  const location = useLocation();
  const active = () =>
    location.pathname === props.href || (props.href !== "/" && location.pathname.startsWith(`${props.href}/`));
  return (
    <A href={props.href} class={cx(NAV_TAB, active() ? NAV_TAB_ACTIVE : NAV_TAB_IDLE)} aria-current={active() ? "page" : undefined}>
      {props.label}
    </A>
  );
}

// The one header bar for the console, settings, account and admin areas: what
// leads (scope selector or brand), the section tabs, and the trailing controls.
// A protected Environment draws a 2px line in its color along the top edge of the
// bar, inside its 40px height.
export function AppBar(props: {
  leading: JSX.Element;
  nav?: readonly NavItem[];
  navLabel?: string;
  trailing?: JSX.Element;
  protectedColor?: string | undefined;
}) {
  return (
    <div class="sticky top-0 z-30">
      <header class="relative flex h-10 items-center gap-3 border-b border-console-border-strong gradient-bar px-5 max-[860px]:px-3">
        <Show when={props.protectedColor}>
          {(color) => <div class="pointer-events-none absolute inset-x-0 top-0 h-0.5" style={{ "background-color": color() }} aria-hidden="true" data-protected-stripe />}
        </Show>
        {props.leading}
        <Show when={props.nav?.length}>
          <span class="h-5 w-px shrink-0 bg-console-border" aria-hidden="true" />
          <nav class="flex min-w-0 flex-1 items-center gap-1 overflow-x-auto px-1 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden" aria-label={props.navLabel ?? "Sections"}>
            <For each={props.nav}>{(item) => <NavTab href={item.href} label={item.label} />}</For>
          </nav>
        </Show>
        <div class={cx("flex shrink-0 items-center gap-2", !props.nav?.length && "ml-auto")}>{props.trailing}</div>
      </header>
    </div>
  );
}
