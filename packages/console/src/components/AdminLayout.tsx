import { A, useLocation } from "@solidjs/router";
import type { JSX } from "solid-js";
import { cx, ui } from "../ui/styles";

function AdminLink(props: { href: string; children: JSX.Element }) {
  const location = useLocation();
  const active = () => location.pathname === props.href;
  return <A href={props.href} class={cx(ui.tabLink, active() ? ui.tabLinkActive : ui.tabLinkHover)}>{props.children}</A>;
}

export function AdminLayout(props: { children?: JSX.Element }) {
  return (
    <div class="min-h-dvh bg-transparent font-sans text-sm text-console-text antialiased">
      <header class="sticky top-0 z-30 flex h-10 items-center gap-3 border-b border-console-border-strong bg-[linear-gradient(to_bottom,#fafafa_0%,#eceff2_100%)] px-5 max-[860px]:px-3">
        <A href="/" class="font-mono text-[12px] font-semibold text-console-text">Helmr Admin</A>
        <span class="h-5 w-px shrink-0 bg-console-border" aria-hidden="true" />
        <nav class="flex min-w-0 flex-1 items-center gap-1" aria-label="Admin sections">
          <AdminLink href="/admin/worker-groups">Worker Groups</AdminLink>
          <AdminLink href="/admin/regions">Regions</AdminLink>
        </nav>
        <A href="/" class={ui.secondaryButton}>Console</A>
      </header>
      <main>{props.children}</main>
    </div>
  );
}
