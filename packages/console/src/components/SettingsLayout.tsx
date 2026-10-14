import { A, useLocation } from "@solidjs/router";
import { createUniqueId, For, Show, type JSX } from "solid-js";
import { useMe } from "../lib/me";
import { useScope } from "../lib/scope";
import { Page } from "../ui/Page";
import { cx, ui } from "../ui/styles";

type NavLink = { href: string; label: string };
type NavGroup = { label: string; name?: () => string | undefined; links: NavLink[] };

// Settings pages have flat topic paths; the group a page sits in names the level
// it applies to: the signed-in account, the tab's Project, or the organization.
export function SettingsLayout(props: { children: JSX.Element }) {
  const location = useLocation();
  const scope = useScope();
  const me = useMe();

  const groups: NavGroup[] = [
    { label: "Account", links: [{ href: "/settings/account", label: "My account" }] },
    {
      label: "Project",
      name: () => scope.selectedProject()?.name,
      links: [
        { href: "/settings/project", label: "General" },
        { href: "/settings/environments", label: "Environments" },
        { href: "/settings/secrets", label: "Secrets" },
        { href: "/settings/api-keys", label: "API keys" },
      ],
    },
    {
      label: "Organization",
      name: () => me.data?.org_name || undefined,
      links: [{ href: "/settings/members", label: "Members" }],
    },
  ];

  return (
    <Page class="grid grid-cols-[190px_minmax(0,1fr)] items-start gap-5 max-[960px]:grid-cols-1 max-[960px]:gap-4">
      <nav class={"sticky top-13 flex flex-col gap-3.5 max-[960px]:static max-[960px]:gap-1 max-[960px]:border max-[960px]:border-console-border max-[960px]:bg-console-surface max-[960px]:p-1.5"} aria-label="Settings sections">
        <For each={groups}>
          {(group) => {
            const labelID = createUniqueId();
            return (
              <div role="group" aria-labelledby={labelID} class={"flex flex-col gap-px max-[960px]:flex-row max-[960px]:flex-wrap max-[960px]:items-center max-[960px]:gap-1"}>
                <div class={"flex min-w-0 flex-col gap-0.5 px-2 pb-1.5 max-[960px]:w-full max-[960px]:flex-row max-[960px]:items-baseline max-[960px]:gap-1.5 max-[960px]:px-1.5 max-[960px]:pb-0"}>
                  <span id={labelID} class={ui.microLabel}>{group.label}</span>
                  <Show when={group.name?.()}>
                    {(name) => <span class={"truncate font-mono text-badge text-console-muted"} title={name()}>{name()}</span>}
                  </Show>
                </div>
                <For each={group.links}>
                  {(link) => (
                    <A
                      href={link.href}
                      class={cx(
                        ui.settingsNavLink,
                        location.pathname === link.href ? ui.settingsNavLinkActive : ui.settingsNavLinkHover,
                      )}
                      aria-current={location.pathname === link.href ? "page" : undefined}
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
      <div class={"min-w-0"}>{props.children}</div>
    </Page>
  );
}
