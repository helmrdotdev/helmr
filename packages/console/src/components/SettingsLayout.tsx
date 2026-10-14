import { A, useLocation } from "@solidjs/router";
import type { JSX } from "solid-js";
import { Page } from "../ui/Page";
import { cx, ui } from "../ui/styles";

type NavLink = { href: string; label: string };
type NavGroup = { label: string; links: NavLink[] };

const SETTINGS_NAV: NavGroup[] = [
  {
    label: "General",
    links: [
      { href: "/settings/projects", label: "Project" },
      { href: "/settings/environments", label: "Environments" },
      { href: "/settings/members", label: "Members" },
    ],
  },
  {
    label: "Security",
    links: [
      { href: "/settings/api-keys", label: "API keys" },
      { href: "/settings/secrets", label: "Secrets" },
    ],
  },
];

export function SettingsLayout(props: { children: JSX.Element }) {
  const location = useLocation();

  return (
    <Page class="grid grid-cols-[190px_minmax(0,1fr)] items-start gap-5 max-[960px]:grid-cols-1 max-[960px]:gap-4">
      <aside class={"sticky top-13 flex flex-col gap-3.5 max-[960px]:static max-[960px]:flex-row max-[960px]:flex-wrap max-[960px]:gap-1 max-[960px]:border max-[960px]:border-console-border max-[960px]:bg-console-surface max-[960px]:p-1.5"} aria-label="Settings sections">
        {SETTINGS_NAV.map((group) => (
          <div class={"flex flex-col gap-px max-[960px]:flex-row max-[960px]:items-center max-[960px]:gap-1"}>
            <span class={cx("px-2 pb-1.5 max-[960px]:px-1.5 max-[960px]:pb-0", ui.microLabel)}>{group.label}</span>
            {group.links.map((link) => (
              <A
                href={link.href}
                class={cx(
                  ui.settingsNavLink,
                  location.pathname === link.href ? ui.settingsNavLinkActive : ui.settingsNavLinkHover,
                )}
              >
                {link.label}
              </A>
            ))}
          </div>
        ))}
      </aside>
      <div class={"min-w-0"}>{props.children}</div>
    </Page>
  );
}
