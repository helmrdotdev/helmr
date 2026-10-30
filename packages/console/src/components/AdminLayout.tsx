import { A } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import type { JSX } from "solid-js";
import { getAuthConfig } from "../lib/auth";
import { BrandMark } from "../ui/BrandMark";
import { ui } from "../ui/styles";
import { AppBar, type NavItem } from "./AppBar";
import { OperatorMenu } from "./OperatorMenu";

// The admin sections, in the order the admin area opens them; audit last, as in
// Settings. Account repair exists only on self-hosted deployments.
export function AdminLayout(props: { children?: JSX.Element }) {
  const config = createQuery(() => ({ queryKey: ["auth-config"], queryFn: getAuthConfig }));
  const nav = (): NavItem[] => [
    { href: "/admin/worker-groups", label: "Worker groups" },
    { href: "/admin/regions", label: "Regions" },
    { href: "/admin/operator-keys", label: "Operator keys" },
    ...(config.data?.deployment_mode === "self-hosted" ? [{ href: "/admin/account-repair", label: "Account repair" }] : []),
    { href: "/admin/audit-events", label: "Audit events" },
  ];
  return (
    <div class="min-h-dvh bg-transparent font-sans text-sm text-console-text">
      <AppBar
        leading={<BrandMark area="Admin" href="/admin" bar />}
        nav={nav()}
        navLabel="Admin sections"
        trailing={<><A href="/" class={ui.secondaryButton}>Console</A><OperatorMenu /></>}
      />
      <main>{props.children}</main>
    </div>
  );
}
