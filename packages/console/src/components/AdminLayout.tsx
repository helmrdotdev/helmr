import { A } from "@solidjs/router";
import type { JSX } from "solid-js";
import { BrandMark } from "../ui/BrandMark";
import { ui } from "../ui/styles";
import { AppBar } from "./AppBar";

const ADMIN_NAV = [
  { href: "/admin/worker-groups", label: "Worker Groups" },
  { href: "/admin/regions", label: "Regions" },
];

export function AdminLayout(props: { children?: JSX.Element }) {
  return (
    <div class="min-h-dvh bg-transparent font-sans text-sm text-console-text">
      <AppBar
        leading={<BrandMark area="Admin" href="/" />}
        nav={ADMIN_NAV}
        navLabel="Admin sections"
        trailing={<A href="/" class={ui.secondaryButton}>Console</A>}
      />
      <main>{props.children}</main>
    </div>
  );
}
