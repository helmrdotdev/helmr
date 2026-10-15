import { A, useLocation } from "@solidjs/router";
import { createUniqueId, For, type JSX } from "solid-js";
import { Page } from "../ui/Page";
import { cx, ui } from "../ui/styles";

type NavLink = { href: string; label: string };
type NavGroup = { label: string; links: NavLink[] };

// Settings pages have flat topic paths; the group a page sits in names the level
// it applies to: the signed-in account, the tab's Project, or the organization.
export function SettingsLayout(props: { children: JSX.Element }) {
  const location = useLocation();

  const groups: NavGroup[] = [
    { label: "Account", links: [{ href: "/settings/account", label: "My account" }] },
    {
      label: "Project",
      links: [
        { href: "/settings/project", label: "General" },
        { href: "/settings/environments", label: "Environments" },
        { href: "/settings/secrets", label: "Secrets" },
        { href: "/settings/api-keys", label: "API keys" },
      ],
    },
    {
      label: "Organization",
      links: [{ href: "/settings/members", label: "Members" }],
    },
  ];

  const current = () => groups.flatMap((group) => group.links).find((link) => link.href === location.pathname);
  let compact: HTMLDetailsElement | undefined;
  const nav = (props: { class: string; onNavigate?: () => void }) => (
    <nav class={props.class} aria-label="Settings sections">
      <For each={groups}>
        {(group) => {
          const labelID = createUniqueId();
          return (
            <div role="group" aria-labelledby={labelID} class="flex flex-col gap-px">
              <span id={labelID} class={cx(ui.microLabel, "px-2 pb-1.5")}>{group.label}</span>
              <For each={group.links}>
                {(link) => (
                  <A
                    href={link.href}
                    class={cx(
                      ui.settingsNavLink,
                      location.pathname === link.href ? ui.activeBar : ui.settingsNavLinkHover,
                    )}
                    aria-current={location.pathname === link.href ? "page" : undefined}
                    onClick={() => props.onNavigate?.()}
                  >
                    {link.label}
                  </A>
                )}
              </For>
            </div>
          );
        }}
      </For>
    </nav>
  );

  return (
    <Page class="grid grid-cols-[190px_minmax(0,1fr)] items-start gap-5 max-[960px]:grid-cols-1 max-[960px]:gap-4">
      {nav({ class: "sticky top-13 flex flex-col gap-3.5 max-[960px]:hidden" })}
      {/* Narrow screens: one select-like control that opens the same grouped list. */}
      <details ref={compact} class="relative min-[961px]:hidden">
        <summary class="flex h-8 cursor-pointer list-none items-center justify-between border border-console-border bg-console-surface px-3 font-mono text-control [&::-webkit-details-marker]:hidden">
          <span>Settings · {current()?.label ?? "Sections"}</span>
          <span aria-hidden="true" class="text-console-subtle">▾</span>
        </summary>
        {nav({ class: "mt-1 flex flex-col gap-3 border border-console-border bg-console-surface p-1.5", onNavigate: () => { if (compact) compact.open = false; } })}
      </details>
      <div class={"min-w-0"}>{props.children}</div>
    </Page>
  );
}
