import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createSignal, For, Show } from "solid-js";
import { targetSummary, getMemberPermissions, listAccessTargets, listGrants, listGroups, listRoles, removeGrant, type MemberPermissionSource, type RoleGrant } from "../../lib/permissions";
import { hasPermission } from "../../lib/auth";
import { useMe } from "../../lib/me";
import { useScope } from "../../lib/scope";
import { errorMessage } from "../../lib/error";
import { AssignRoleDialog, EditGrantDialog } from "../../routes/groups";
import { ConfirmDialog } from "../../ui/Dialog";
import { Notice } from "../../ui/Notice";
import { ui } from "../../ui/styles";
import { GrantRemovalImpact } from "./GrantRemovalImpact";

export function accessSourceLabel(source: MemberPermissionSource): string {
  return source.subject_kind === "member" ? "Direct assignment" : source.subject_kind === "all_members" ? "All members" : source.group_name || "Group";
}

export function MemberAccess(props: { membershipID: string }) {
  const me = useMe(); const scope = useScope(); const cache = useQueryClient();
  const canManage = () => hasPermission(me.data,"access.manage");
  const access = createQuery(()=>({queryKey:["access","member",props.membershipID,"permissions"],queryFn:()=>getMemberPermissions(props.membershipID),enabled:!!props.membershipID}));
  const grants = createQuery(()=>({queryKey:["access","grants"],queryFn:listGrants,enabled:canManage()}));
  const roles = createQuery(()=>({queryKey:["access","roles"],queryFn:listRoles,enabled:canManage()}));
  const groups = createQuery(()=>({queryKey:["access","groups"],queryFn:listGroups,enabled:canManage()}));
  const targets = createQuery(()=>({queryKey:["access","targets",scope.selectedProjectID()],queryFn:()=>listAccessTargets(scope.selectedProjectID()),enabled:canManage() && !!scope.selectedProjectID()}));
  const [editing,setEditing]=createSignal<RoleGrant>();
  const [assigning,setAssigning] = createSignal(false); const [removing,setRemoving] = createSignal<RoleGrant>();
  const directAssignments=()=>grants.data?.grants.filter(g=>g.subject_kind === "member" && g.member_id === props.membershipID) ?? [];
  const directGrant = (source:MemberPermissionSource) => source.subject_kind === "member" ? grants.data?.grants.find(g=>g.id === source.grant_id) : undefined;
  const refresh = async()=>{await Promise.all([cache.invalidateQueries({queryKey:["access"]}),cache.invalidateQueries({queryKey:["me"]}),cache.invalidateQueries({queryKey:["projects"]})]);};
  return <div class="space-y-4">
    <Show when={access.error}><Notice tone="danger">{errorMessage(access.error)}</Notice></Show>
    <Show when={access.isPending}><p>Loading access…</p></Show>
    <Show when={access.data}>{data=><>
      <Show when={data().is_owner}><Notice tone="info">Owner: can appoint or remove owners and explicitly recover their own Access Administrator assignment. Ownership grants no ordinary permissions.</Notice></Show>
      <Show when={canManage() && data().status === "active"}><button class={ui.secondaryButton} disabled={!roles.data || !groups.data || (!!scope.selectedProjectID() && !targets.data)} onClick={()=>setAssigning(true)}>Assign role to this person</button></Show>
      <For each={[{kind:"member",label:"Direct assignments"},{kind:"group",label:"Via Groups"},{kind:"all_members",label:"All members"}]}>{section=><section class="space-y-2"><h3 class="font-medium">{section.label}</h3>
        <Show when={section.kind === "member" && canManage()} fallback={<>
        <For each={data().scopes.flatMap(scope=>scope.sources.filter(source=>source.subject_kind===section.kind).map(source=>({scope,source})))} fallback={<p class="text-control text-console-muted">No assignments through this path.</p>}>{entry=><div class="flex items-start gap-3 border-l border-console-border pl-3 text-control"><div class="flex-1"><p>{accessSourceLabel(entry.source)} → {entry.source.role_name}</p><p>{entry.scope.type === "organization" ? "Organization" : `${entry.scope.project} / ${entry.scope.environment} / ${entry.source.agent || "Entire environment"}`}</p><p class="text-console-muted">{entry.source.included_permissions.join(" · ")}</p></div><Show when={canManage() && directGrant(entry.source)}>{grant=><div class="flex gap-2"><button class={ui.compactButton} onClick={()=>setEditing(grant())}>Edit targets</button><button class={ui.compactButton} onClick={()=>setRemoving(grant())}>Remove assignment</button></div>}</Show></div>}</For>
        </>}><Show when={grants.error}><Notice tone="danger">{errorMessage(grants.error)}</Notice></Show><For each={directAssignments()} fallback={<p class="text-control text-console-muted">{grants.isPending ? "Loading assignments…" : "No direct assignments."}</p>}>{grant=><div class="flex items-start gap-3 border-l border-console-border pl-3 text-control"><div class="flex-1"><p>{grant.role_name}</p><p>{grant.target_kind === "organization" ? "Organization" : `${grant.project_name} / ${grant.environment_name}`}</p><p>{targetSummary(grant)}</p></div><button class={ui.compactButton} onClick={()=>setEditing(grant)}>Edit targets</button><button class={ui.compactButton} onClick={()=>setRemoving(grant)}>Remove assignment</button></div>}</For></Show>
      </section>}</For>
      <p class="text-control text-console-muted">{data().notice} Access from all paths adds together. Removing one assignment leaves other paths in effect.</p>
    </>}</Show>
    <Show when={assigning() && roles.data && groups.data}><AssignRoleDialog project={scope.selectedProjectID()} projectName={scope.selectedProject()?.name ?? ""} roles={roles.data!.roles} groups={groups.data!.groups} catalog={roles.data!.catalog} targets={targets.data?.targets ?? []} initialGroup="" initialMember={props.membershipID} initialRole="" initialEnvironment={scope.selectedEnvironmentID()} onClose={()=>setAssigning(false)} onSaved={async()=>{await refresh();setAssigning(false);}} /></Show>
    <Show when={editing()} keyed>{grant=><EditGrantDialog grant={grant} onClose={()=>setEditing(undefined)} onSaved={refresh} />}</Show>
    <Show when={removing()}>{grant=><ConfirmDialog title="Remove direct assignment" confirmLabel="Remove assignment" tone="danger" onClose={()=>setRemoving(undefined)} onConfirm={async()=>{await removeGrant(grant().id, grant().revision);await refresh();}}><p>Remove {grant().role_name} from this person? Other direct assignments, Groups and All members still apply.</p><GrantRemovalImpact grant={grant()} /></ConfirmDialog>}</Show>
  </div>;
}
