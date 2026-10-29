import { createQuery } from "@tanstack/solid-query";
import { createSignal, Show } from "solid-js";
import { usePermission } from "../../lib/me";
import { listRoles } from "../../lib/permissions";
import { Notice } from "../../ui/Notice";
import { ui } from "../../ui/styles";
import { GrantAccessDialog } from "./GrantAccessDialog";

// Access administrators can grant access even without deployment permissions.
export function EnvironmentAccessSetup(props: { projectID: string; environmentID: string }) {
  const permission = usePermission();
  const [open, setOpen] = createSignal(false);
  const roles = createQuery(() => ({ queryKey: ["access", "roles"], queryFn: listRoles, enabled: permission.has("roles.read") }));
  return <div class="mb-4 grid gap-3">
    <Notice tone="info" action={<button type="button" class={ui.compactButton} onClick={() => setOpen(true)}>Grant access</button>}>
      You can manage access to this environment. Grant a role to yourself, a person or a group.
    </Notice>
    <Show when={open()}>
      <GrantAccessDialog environment={{ project: props.projectID, environment: props.environmentID }} roleID={roles.data?.roles.find(r => r.builtin_key === "environment_resource_administrator")?.id} onClose={() => setOpen(false)} />
    </Show>
  </div>;
}
