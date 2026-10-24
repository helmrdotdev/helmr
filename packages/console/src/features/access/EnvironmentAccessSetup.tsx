import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createSignal, Show } from "solid-js";
import { usePermission } from "../../lib/me";
import { listAccessTargets, listGroups, listRoles } from "../../lib/permissions";
import { AssignRoleDialog } from "../../routes/groups";
import { errorMessage } from "../../lib/error";
import { Notice } from "../../ui/Notice";
import { ui } from "../../ui/styles";

export function EnvironmentAccessSetup(props:{projectID:string;environmentID:string}) {
 const permission=usePermission();const cache=useQueryClient();const [open,setOpen]=createSignal(false);
 const allowed=()=>permission.has("access.manage");
 const targets=createQuery(()=>({queryKey:["access","targets",props.projectID],queryFn:()=>listAccessTargets(props.projectID),enabled:open() && allowed()}));
 const roles=createQuery(()=>({queryKey:["access","roles"],queryFn:listRoles,enabled:open() && allowed()}));
 const groups=createQuery(()=>({queryKey:["access","groups"],queryFn:listGroups,enabled:open() && allowed()}));
 const environment=()=>targets.data?.targets.find(t=>t.environment===props.environmentID);
 const role=()=>roles.data?.roles.find(r=>r.builtin_key==="environment_administrator");
 return <div class="space-y-3 mb-4"><Notice tone="info">Creating an environment assigns no workload permissions. Assign a Role separately to yourself, another person, or a Group before deploying and operating Agents. The environment remains created if an assignment fails.</Notice>
 <Show when={allowed()} fallback={<p class="text-control">Ask a member with Manage access to assign the permissions needed in this environment.</p>}><button class={ui.secondaryButton} onClick={()=>setOpen(true)}>Assign environment access</button></Show>
 <Show when={open() && (targets.error || roles.error || groups.error)}><Notice tone="danger">{errorMessage(targets.error || roles.error || groups.error)}</Notice></Show>
 <Show when={open() && environment() && role() && roles.data && groups.data}><AssignRoleDialog project={props.projectID} projectName={props.projectID} targets={targets.data!.targets} roles={roles.data!.roles} groups={groups.data!.groups} catalog={roles.data!.catalog} initialGroup="" initialRole={role()!.id} initialEnvironment={props.environmentID} fixedTarget={{environment_id:environment()!.environment_id}} onClose={()=>setOpen(false)} onSaved={async()=>{await cache.invalidateQueries({queryKey:["me"]});await cache.invalidateQueries({queryKey:["access"]});setOpen(false);}} /></Show>
 </div>;
}
