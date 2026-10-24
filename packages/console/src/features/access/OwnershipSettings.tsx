import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createSignal, For, Show } from "solid-js";
import { request } from "../../lib/api";
import { errorMessage } from "../../lib/error";
import { updateMemberOwnership } from "../../lib/members";
import { ConfirmDialog } from "../../ui/Dialog";
import { Notice } from "../../ui/Notice";
import { Panel } from "../../ui/Panel";
import { ui } from "../../ui/styles";

type OwnerMember = {membership_id:string;display_name:string;is_owner:boolean};
export function OwnershipSettings() {
  const cache=useQueryClient();
  const members=createQuery(()=>({queryKey:["ownership","members"],queryFn:()=>request<{members:OwnerMember[]}>("/api/ownership/members")}));
  const [changing,setChanging]=createSignal<OwnerMember>(); const [recovering,setRecovering]=createSignal(false);
  const refresh=async()=>{await Promise.all([cache.invalidateQueries({queryKey:["ownership"]}),cache.invalidateQueries({queryKey:["me"]}),cache.invalidateQueries({queryKey:["projects"]}),cache.invalidateQueries({queryKey:["members"]}),cache.invalidateQueries({queryKey:["access"]})]);};
  return <Panel title="Organization ownership">
    <p class="text-control text-console-muted">Owners can appoint or remove owners and explicitly recover their own Access Administrator assignment. At least one active owner must remain. Ownership changes preserve existing Role assignments.</p>
    <Show when={members.error}><Notice tone="danger">{errorMessage(members.error)}</Notice></Show>
    <div class="divide-y divide-console-border"><For each={members.data?.members}>{member=><div class="flex items-center justify-between gap-3 py-2"><span>{member.display_name} {member.is_owner ? "· Owner" : ""}</span><button class={ui.secondaryButton} onClick={()=>setChanging(member)}>{member.is_owner ? "Remove ownership" : "Make owner"}</button></div>}</For></div>
    <button class={ui.secondaryButton} onClick={()=>setRecovering(true)}>Recover my access administration</button>
    <Show when={changing()}>{member=><ConfirmDialog title={member().is_owner ? "Remove ownership" : "Appoint owner"} confirmLabel="Change ownership" onClose={()=>setChanging(undefined)} onConfirm={async()=>{await updateMemberOwnership(member().membership_id,!member().is_owner,member().is_owner);await refresh();}}><p>Change ownership for {member().display_name}? An owner can change other owners and recover access administration, including the ability to grant workload access. This does not assign or remove ordinary Roles.</p></ConfirmDialog>}</Show>
    <Show when={recovering()}><ConfirmDialog title="Recover access administration" confirmLabel="Recover my access" onClose={()=>setRecovering(false)} onConfirm={async()=>{await request("/api/ownership/recover-access",{method:"POST"});await refresh();}}><p>Add the fixed Access Administrator Role to your own membership at organization scope. This audited action lets you manage Roles and assignments. It grants no workload access by itself.</p></ConfirmDialog></Show>
  </Panel>;
}
