import { A } from "@solidjs/router";
import type { JSX } from "solid-js";
import { BrandMark } from "../ui/BrandMark";
import { ui } from "../ui/styles";
import { AppBar } from "./AppBar";

// The admin sections, in the order the admin area opens them; audit last, as in
// Settings.
const ADMIN_NAV = [
  { href: "/admin/worker-groups", label: "Worker groups" },
  { href: "/admin/regions", label: "Regions" },
  { href: "/admin/audit-events", label: "Audit events" },
];

export function AdminLayout(props: { children?: JSX.Element }) {
  return (
    <div class="min-h-dvh bg-transparent font-sans text-sm text-console-text">
      <AppBar
        leading={<BrandMark area="Admin" href="/admin" bar />}
        nav={ADMIN_NAV}
        navLabel="Admin sections"
        trailing={<A href="/" class={ui.secondaryButton}>Console</A>}
      />
      <main>{props.children}</main>
    </div>
  );
}
