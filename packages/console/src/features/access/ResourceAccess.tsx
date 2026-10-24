import { GrantRemovalImpact } from "./GrantRemovalImpact";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { errorMessage } from "../../lib/error";
import { targetSummary, listAccessTargets, listGrants, listGroups, listRoles, includedPermissions, assignRole, removeGrant, type RoleGrant } from "../../lib/permissions";
import { useScope } from "../../lib/scope";
import { AssignRoleDialog, EditGrantDialog } from "../../routes/groups";
import { ConfirmDialog, Dialog } from "../../ui/Dialog";
import { Notice } from "../../ui/Notice";
import { ui } from "../../ui/styles";

// Resource and group views edit the same assignments through the same writer.
export function ResourceAccess(props: { environment: string; agent?: string; onClose: () => void }) {
  const scope = useScope(); const cache = useQueryClient();
  const project = scope.selectedProjectID(); const projectName = scope.selectedProject()?.name ?? project;
  const targets = createQuery(() => ({ queryKey: ["access", "targets", project], queryFn: () => listAccessTargets(project) }));
  const groups = createQuery(() => ({ queryKey: ["access", "groups"], queryFn: listGroups }));
  const roles = createQuery(() => ({ queryKey: ["access", "roles"], queryFn: listRoles }));
  const grants = createQuery(() => ({ queryKey: ["access", "grants"], queryFn: listGrants }));
  const environment = () => targets.data?.targets.find(t => t.environment === props.environment);
  const agent = () => environment()?.agents.find(a => a.key === props.agent);
  const assignments = createMemo(() => grants.data?.grants.filter(g => g.environment_id === environment()?.environment_id && (!props.agent || g.agents_mode === "all" || g.agent_ids.includes(agent()?.id ?? ""))) ?? []);
  const [editing,setEditing] = createSignal<RoleGrant>();
  const audience = () => grants.data?.grants.find(g=>g.subject_kind === "all_members" && g.environment_id === environment()?.environment_id && g.role_id === roles.data?.roles.find(r=>r.builtin_key === "agent_user")?.id);
  const shared = () => audience()?.agent_ids.includes(agent()?.id ?? "") ?? false;
  const [sharing,setSharing] = createSignal(false); const [shareError,setShareError] = createSignal("");
  const [assigning, setAssigning] = createSignal(false); const [removing, setRemoving] = createSignal<RoleGrant>();
  const error = () => targets.error || groups.error || roles.error || grants.error;
  const ready = () => !!targets.data && !!groups.data && !!roles.data && !!grants.data && !!environment() && (!props.agent || !!agent());
  const refresh = async () => { await cache.invalidateQueries({ queryKey: ["access"] }); await cache.invalidateQueries({ queryKey: ["me"] }); };
  return <>
    <Show when={!assigning() && !removing() && !editing()}><Dialog title={props.agent ? `Agent access: ${props.agent}` : `Environment access: ${environment()?.name ?? props.environment}`} onClose={props.onClose} class="max-w-180">
      <div class="space-y-4">
        <p class="text-control text-console-muted">{projectName} / {environment()?.name ?? props.environment}. Direct assignments, Groups and All members add together. Ownership alone grants no workload access.</p>
        <Show when={error()}><Notice tone="danger">{errorMessage(error())}</Notice></Show>
        <Show when={ready()} fallback={<p class={ui.muted}>Loading access…</p>}>
          <Show when={assignments().length} fallback={<p>No roles assigned to this scope.</p>}>
            <div class="divide-y divide-console-border"><For each={assignments()}>{grant => <div class="flex items-start gap-3 py-3">
              <div class="flex-1"><p class="font-medium">{grant.subject_kind === "all_members" ? "All members" : grant.subject_kind === "api_key" ? grant.api_key_name || grant.api_key_id : grant.subject_kind === "member" ? grant.member_name : groups.data?.groups.find(g => g.id === grant.group_id)?.name} → {grant.role_name}</p>
                <p class="text-control">{targetSummary(grant)}</p>
                <p class="text-control text-console-muted">{includedPermissions(grant.permissions, roles.data?.catalog ?? []).map(id => roles.data?.catalog.find(c => c.id === id)?.name ?? id).join(" · ")}</p>
              </div><button class={ui.secondaryButton} onClick={() => setEditing(grant)}>Edit targets</button><button class={ui.secondaryButton} onClick={() => setRemoving(grant)}>Remove assignment</button>
            </div>}</For></div>
          </Show>
        </Show>
        <Show when={props.agent}><div class="border border-console-border p-3 space-y-2"><p class="text-control">Share this Agent in {environment()?.name ?? props.environment} with current and future active organization members. This grants Agent use; other Agents and environments are not shared.</p><Show when={shareError()}><Notice tone="danger">{shareError()}</Notice></Show><button class={ui.secondaryButton} disabled={!ready() || sharing()} onClick={async()=>{const standard=roles.data?.roles.find(r=>r.builtin_key === "agent_user");if(!standard || !agent() || !environment()) return;setSharing(true);setShareError("");try{const existing=audience();const ids=shared() ? existing!.agent_ids.filter(id=>id!==agent()!.id) : [...(existing?.agent_ids ?? []),agent()!.id];if(existing && ids.length===0){await removeGrant(existing.id,existing.revision);}else{await assignRole(project,{subject_kind:"all_members",group_id:null,role_id:standard.id,environment_id:environment()!.environment_id,target_kind:"environment",agents_mode:"selected",agent_ids:ids,expected_role_revision:standard.revision},existing);}await refresh();}catch(e){setShareError(`${errorMessage(e)} Review the refreshed targets before retrying.`);await refresh();}finally{setSharing(false);}}}>{sharing() ? "Saving…" : shared() ? "Unshare this Agent from All members" : "Share with everyone in this organization"}</button></div></Show>
        <div class="flex justify-end"><button class={ui.button} disabled={!ready()} onClick={() => setAssigning(true)}>Assign role</button></div>
      </div>
    </Dialog></Show>
    <Show when={assigning() && ready()}><AssignRoleDialog project={project} projectName={projectName} groups={groups.data!.groups} roles={roles.data!.roles} catalog={roles.data!.catalog} targets={targets.data!.targets} initialGroup="" initialRole={props.agent ? "" : roles.data!.roles.find(r=>r.builtin_key === "environment_administrator")?.id ?? ""} initialEnvironment={props.environment} fixedTarget={{ environment_id: environment()!.environment_id, ...(agent() ? { agent_id: agent()!.id } : {}) }} onClose={() => setAssigning(false)} onSaved={async () => { await refresh(); setAssigning(false); }} /></Show>
    <Show when={editing()} keyed>{grant=><EditGrantDialog grant={grant} onClose={()=>setEditing(undefined)} onSaved={refresh} />}</Show>
    <Show when={removing()}>{grant => <ConfirmDialog title="Remove role assignment" confirmLabel="Remove assignment" onClose={() => setRemoving(undefined)} onConfirm={async () => { await removeGrant(grant().id, grant().revision); await refresh(); setRemoving(undefined); }}>
      <p>Remove {grant().role_name}: {targetSummary(grant())}? This removes the complete assignment, including all listed targets in {grant().environment_name}. To change selected Agents, use Edit targets.</p>
      <p class="mt-2">Other sources of access remain. Effects already admitted by running workloads are not stopped.</p>
      <GrantRemovalImpact grant={grant()} />
    </ConfirmDialog>}</Show>
  </>;
}
