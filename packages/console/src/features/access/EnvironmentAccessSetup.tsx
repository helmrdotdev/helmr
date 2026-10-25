import { createQuery } from "@tanstack/solid-query";
import { createSignal, Show } from "solid-js";
import { usePermission } from "../../lib/me";
import { listRoles } from "../../lib/permissions";
import { Notice } from "../../ui/Notice";
import { ui } from "../../ui/styles";
import { GrantAccessDialog } from "./GrantAccessDialog";

// Creating an environment grants nobody access to it; this offers the grant.
export function EnvironmentAccessSetup(props: { projectID: string; environmentID: string }) {
  const permission = usePermission();
  const [open, setOpen] = createSignal(false);
  const allowed = () => permission.has("assignments.create");
  const roles = createQuery(() => ({ queryKey: ["access", "roles"], queryFn: listRoles, enabled: permission.has("roles.read") }));
  return <div class="mb-4 grid gap-3">
    <Notice tone="info" action={<Show when={allowed()}><button type="button" class={ui.compactButton} onClick={() => setOpen(true)}>Grant access</button></Show>}>
      A new environment grants nobody access. {allowed() ? "Grant a role to yourself, a person or a group before deploying agents." : "Ask a member who can create assignments to grant you a role here."}
    </Notice>
    <Show when={open()}>
      <GrantAccessDialog environment={{ project: props.projectID, environment: props.environmentID }} roleID={roles.data?.roles.find(r => r.builtin_key === "environment_resource_administrator")?.id} onClose={() => setOpen(false)} />
    </Show>
  </div>;
}
