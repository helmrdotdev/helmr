import { A, useLocation } from "@solidjs/router";
import { createEffect, createSignal, For, on, Show, type JSX } from "solid-js";
import { focusableIn, Popover } from "../ui/Popover";
import { cx, ui } from "../ui/styles";

// A section of the bar. `match` lists further paths the section owns, such as the
// other pages of a section with sub-tabs.
export type NavItem = { href: string; label: string; match?: readonly string[] };

const NAV_TAB =
  "relative flex h-6.5 shrink-0 cursor-pointer items-center rounded-xs border border-transparent px-2.5 font-mono text-chrome font-medium text-console-muted transition duration-100";
const NAV_TAB_IDLE = "hover:border-console-border-strong hover:bg-console-surface-muted hover:text-console-text";
const NAV_TAB_ACTIVE =
  "text-console-text after:absolute after:bottom-0.75 after:left-2.5 after:right-2.5 after:h-px after:bg-console-accent";

// A section is current on its own paths and on every path below them ("/" only on
// itself).
function isCurrentSection(pathname: string, item: NavItem): boolean {
  return [item.href, ...(item.match ?? [])].some(
    (path) => pathname === path || (path !== "/" && pathname.startsWith(`${path}/`)),
  );
}

// A section tab in the bar.
export function NavTab(props: { item: NavItem }) {
  const location = useLocation();
  const active = () => isCurrentSection(location.pathname, props.item);
  return (
    <A href={props.item.href} class={cx(NAV_TAB, active() ? NAV_TAB_ACTIVE : NAV_TAB_IDLE)} aria-current={active() ? "page" : undefined}>
      {props.item.label}
    </A>
  );
}

function MenuIcon(props: { open: boolean }) {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true">
      <Show when={props.open} fallback={<path d="M4 6h16M4 12h16M4 18h16" />}>
        <path d="M6 6l12 12M18 6 6 18" />
      </Show>
    </svg>
  );
}

// The one header bar for the console, settings, account and admin areas: what
// leads (scope selector or brand), the section tabs, and the trailing controls.
// A protected Environment draws a 2px line in its color along the top edge of the
// bar, inside its 40px height. Below 720px the section tabs move into a menu
// drawer with 44px targets; `drawerStart` and `drawerEnd` add content above and
// below the sections and receive a function that closes the drawer.
export function AppBar(props: {
  leading: JSX.Element;
  nav?: readonly NavItem[];
  navLabel?: string;
  trailing?: JSX.Element;
  protectedColor?: string | undefined;
  drawerTitle?: string;
  drawerStart?: (close: () => void) => JSX.Element;
  drawerEnd?: (close: () => void) => JSX.Element;
}) {
  const location = useLocation();
  const [open, setOpen] = createSignal(false);
  const [menuButton, setMenuButton] = createSignal<HTMLButtonElement>();
  let drawer: HTMLDivElement | undefined;
  const close = () => setOpen(false);
  // Opening moves focus to the drawer's first control; closing returns it to the
  // menu button (Popover).
  const toggle = () => {
    if (open()) { close(); return; }
    setOpen(true);
    queueMicrotask(() => { if (drawer) focusableIn(drawer)[0]?.focus(); });
  };
  createEffect(on(() => location.pathname, close, { defer: true }));
  const label = () => props.navLabel ?? "Sections";
  return (
    <div class="sticky top-0 z-30">
      <header class="relative flex h-10 items-center gap-3 border-b border-console-border-strong gradient-bar px-5 max-[860px]:px-3">
        <Show when={props.protectedColor}>
          {(color) => <div class="pointer-events-none absolute inset-x-0 top-0 h-0.5" style={{ "background-color": color() }} aria-hidden="true" data-protected-stripe />}
        </Show>
        <Show when={props.nav?.length}>
          <button
            ref={setMenuButton}
            type="button"
            class={cx(ui.iconButton, "min-[720px]:hidden")}
            data-open={open() ? "true" : "false"}
            aria-label={open() ? "Close menu" : "Open menu"}
            aria-haspopup="dialog"
            aria-expanded={open()}
            onClick={toggle}
          >
            <MenuIcon open={open()} />
          </button>
        </Show>
        {props.leading}
        <Show when={props.nav?.length}>
          <span class="h-5 w-px shrink-0 bg-console-border max-[720px]:hidden" aria-hidden="true" />
          <nav class="flex min-w-0 flex-1 items-center gap-1 overflow-x-auto px-1 [scrollbar-width:none] max-[720px]:hidden [&::-webkit-scrollbar]:hidden" aria-label={label()}>
            <For each={props.nav}>{(item) => <NavTab item={item} />}</For>
          </nav>
        </Show>
        <div class={cx("ml-auto flex shrink-0 items-center gap-2", !!props.nav?.length && "min-[720px]:ml-0")}>{props.trailing}</div>
      </header>
      <Popover
        open={open()}
        anchor={menuButton()}
        onDismiss={close}
        role="dialog"
        label="Menu"
        ref={(element) => { drawer = element; }}
        title={props.drawerTitle ?? "Helmr"}
        width={320}
        maxHeight={2000}
        fullWidthOnPhones
      >
        {props.drawerStart?.(close)}
        <nav class="flex flex-col gap-0.5 border-b border-console-border p-2 last:border-b-0" aria-label={label()}>
          <For each={props.nav}>
            {(item) => {
              const active = () => isCurrentSection(location.pathname, item);
              return (
                <A href={item.href} class={cx(ui.drawerItem, active() ? ui.drawerItemActive : ui.drawerItemIdle)} aria-current={active() ? "page" : undefined} onClick={close}>
                  {item.label}
                </A>
              );
            }}
          </For>
        </nav>
        {props.drawerEnd?.(close)}
      </Popover>
    </div>
  );
}
