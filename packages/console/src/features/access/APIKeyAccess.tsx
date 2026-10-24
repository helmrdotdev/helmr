import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createSignal, For, Show } from "solid-js";
import { errorMessage } from "../../lib/error";
import { listAccessTargets, listGrants, listRoles, removeGrant, targetSummary, type RoleGrant } from "../../lib/permissions";
import { type ApiKeySummary } from "../../lib/api-keys";
import { AssignRoleDialog, EditGrantDialog } from "../../routes/groups";
import { ConfirmDialog, Dialog } from "../../ui/Dialog";
import { Notice } from "../../ui/Notice";
import { ui } from "../../ui/styles";

export function APIKeyAccess(props: { keyItem: ApiKeySummary; project: string; projectName: string; environment: string; onClose: () => void }) {
  const cache = useQueryClient();
  const roles = createQuery(() => ({ queryKey: ["access", "roles"], queryFn: listRoles }));
  const grants = createQuery(() => ({ queryKey: ["access", "grants"], queryFn: listGrants }));
  const targets = createQuery(() => ({ queryKey: ["access", "targets", props.project], queryFn: () => listAccessTargets(props.project) }));
  const assignments = () => grants.data?.grants.filter(g => g.api_key_id === props.keyItem.id) ?? [];
  const environment = () => targets.data?.targets.find(t => t.environment === props.environment);
  const [assigning, setAssigning] = createSignal(false);
  const [editing, setEditing] = createSignal<RoleGrant>();
  const [removing, setRemoving] = createSignal<RoleGrant>();
  const refresh = async () => { await Promise.all([cache.invalidateQueries({ queryKey: ["access"] }), cache.invalidateQueries({ queryKey: ["api-keys"] })]); };
  const error = () => roles.error || grants.error || targets.error;
  const ready = () => !!roles.data && !!grants.data && !!environment();
  return <>
    <Show when={!assigning() && !editing() && !removing()}><Dialog title={`Access: ${props.keyItem.name}`} onClose={props.onClose} class="max-w-180">
      <div class="space-y-4">
        <p class="text-control text-console-muted">Roles grant access only in {props.projectName} / {environment()?.name ?? props.environment}. This key does not inherit its creator’s access or All members assignments. Everyone holding its secret acts as the same identity.</p>
        <Show when={error()}><Notice tone="danger">{errorMessage(error())}</Notice></Show>
        <Show when={ready()} fallback={<p>Loading assignments…</p>}>
          <For each={assignments()} fallback={<Notice tone="info">No Roles assigned. This key cannot perform workload operations.</Notice>}>{grant => <div class="space-y-2 border-b border-console-border pb-3">
            <p class="font-medium">{grant.role_name}</p><p class="text-control">{targetSummary(grant)}</p>
            <div class="flex gap-2"><Show when={props.keyItem.status === "active"}><button class={ui.secondaryButton} onClick={() => setEditing(grant)}>Edit targets</button></Show><button class={ui.secondaryButton} onClick={() => setRemoving(grant)}>Remove assignment</button></div>
          </div>}</For>
          <Show when={props.keyItem.status === "active"} fallback={<Notice tone="info">This key is {props.keyItem.status}. Its Role assignments do not allow authentication.</Notice>}><div class="flex justify-end"><button class={ui.button} onClick={() => setAssigning(true)}>Assign role</button></div></Show>
        </Show>
      </div>
    </Dialog></Show>
    <Show when={assigning() && ready()}><AssignRoleDialog project={props.project} projectName={props.projectName} groups={[]} roles={roles.data!.roles} catalog={roles.data!.catalog} targets={targets.data!.targets} initialGroup="" initialRole="" initialEnvironment={props.environment} initialAPIKey={{ id: props.keyItem.id, name: props.keyItem.name }} fixedTarget={{ environment_id: environment()!.environment_id }} onClose={() => setAssigning(false)} onSaved={async () => { await refresh(); setAssigning(false); }} /></Show>
    <Show when={editing()} keyed>{grant => <EditGrantDialog grant={grant} onClose={() => setEditing(undefined)} onSaved={refresh} />}</Show>
    <Show when={removing()}>{grant => <ConfirmDialog title="Remove role assignment" confirmLabel="Remove assignment" tone="danger" onClose={() => setRemoving(undefined)} onConfirm={async () => { await removeGrant(grant().id, grant().revision); await refresh(); setRemoving(undefined); }}>
      <p>Remove {grant().role_name} from {props.keyItem.name}? {targetSummary(grant())}.</p>
      <p class="mt-2">Other Role assignments remain. This may prevent future work in this key’s existing sessions; it does not undo work already performed.</p>
      <p class="mt-2">Remaining Roles: {assignments().filter(g => g.id !== grant().id).map(g => g.role_name).join(", ") || "None"}.</p>
    </ConfirmDialog>}</Show>
  </>;
}
