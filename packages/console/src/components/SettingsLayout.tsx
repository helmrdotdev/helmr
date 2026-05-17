import { A, useLocation } from "@solidjs/router";
import type { JSX } from "solid-js";
import { cx, ui } from "../ui/styles";

type NavLink = { href: string; label: string };
type NavGroup = { label: string; links: NavLink[] };

const SETTINGS_NAV: NavGroup[] = [
  {
    label: "General",
    links: [
      { href: "/settings/projects", label: "Projects" },
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
  {
    label: "Integrations",
    links: [{ href: "/settings/github", label: "GitHub" }],
  },
];

export function SettingsLayout(props: { children: JSX.Element }) {
  const location = useLocation();

  return (
    <section class={"mx-auto grid w-full max-w-310 grid-cols-[190px_minmax(0,1fr)] items-start gap-5 px-5 pb-12 pt-10 max-[960px]:grid-cols-1 max-[960px]:gap-4"}>
      <aside class={"sticky top-13 flex flex-col gap-3.5 max-[960px]:static max-[960px]:flex-row max-[960px]:flex-wrap max-[960px]:gap-1 max-[960px]:border max-[960px]:border-console-border max-[960px]:bg-console-surface max-[960px]:p-1.5"} aria-label="Settings sections">
        {SETTINGS_NAV.map((group) => (
          <div class={"flex flex-col gap-px max-[960px]:flex-row max-[960px]:items-center max-[960px]:gap-1"}>
            <span class={"px-2 pb-1.5 font-mono text-[10.5px] font-medium uppercase tracking-[0.06em] text-console-subtle max-[960px]:px-1.5 max-[960px]:pb-0"}>{group.label}</span>
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
    </section>
  );
}
