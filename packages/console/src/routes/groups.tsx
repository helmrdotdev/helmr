import { hasPermission } from "../lib/auth";
import { A, useSearchParams } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { GrantRemovalImpact, GroupMemberRemovalImpact } from "../features/access/GrantRemovalImpact";
import { errorMessage } from "../lib/error";
import { useMe } from "../lib/me";
import { listMembers, type OrganizationMember } from "../lib/members";
import { targetSummary, resourceLabel, assignRole, deleteGroup, listAccessTargets, listGrants, listGroups, listRoles, getMemberPermissions, removeGrant, saveGroup, setGroupMember, subjectGrants, supportsAgent, type AccessTarget, type Capability, type PermissionGroup, type PermissionRole, type RoleGrant } from "../lib/permissions";
import { useScope } from "../lib/scope";
import { ConfirmDialog, Dialog } from "../ui/Dialog";
import { Field, TextInput } from "../ui/Field";
import { Notice } from "../ui/Notice";
import { PageHeader } from "../ui/PageHeader";
import { Panel } from "../ui/Panel";
import { Select } from "../ui/Select";
import { cx, ui } from "../ui/styles";

const ALL = "all_members";
const memberName = (m: OrganizationMember) => m.display_name || m.email || m.membership_id;

export function Groups() {
  const scope = useScope(); const me = useMe(); const cache = useQueryClient(); const [params, setParams] = useSearchParams();
  const allowed = () => hasPermission(me.data, "access.manage");
  const groups = createQuery(() => ({ queryKey: ["access", "groups"], queryFn: listGroups, enabled: allowed() }));
  const roles = createQuery(() => ({ queryKey: ["access", "roles"], queryFn: listRoles, enabled: allowed() }));
  const grants = createQuery(() => ({ queryKey: ["access", "grants"], queryFn: listGrants, enabled: allowed() }));
  const members = createQuery(() => ({ queryKey: ["members"], queryFn: listMembers, enabled: allowed() }));
  const targets = createQuery(() => ({ queryKey: ["access", "targets", scope.selectedProjectID()], queryFn: () => listAccessTargets(scope.selectedProjectID()), enabled: allowed() && !!scope.selectedProjectID() }));
  const [selected, setSelected] = createSignal(typeof params["group"] === "string" ? params["group"] : ALL);
  const [tab, setTab] = createSignal<"assignments" | "members">("assignments");
  const [editGroup, setEditGroup] = createSignal<PermissionGroup | "new">();
  const [deleting, setDeleting] = createSignal<PermissionGroup>();
  const [assigning, setAssigning] = createSignal(typeof params["role"] === "string");
  const [editing, setEditing] = createSignal<RoleGrant>();
  const [removing, setRemoving] = createSignal<RoleGrant>();
  const [removingMember,setRemovingMember] = createSignal<OrganizationMember>();
  const [inspect, setInspect] = createSignal(false); const [busy, setBusy] = createSignal(false); const [error, setError] = createSignal("");
  const current = createMemo(() => groups.data?.groups.find(g => g.id === selected()));
  const groupName = () => selected() === ALL ? "All members" : current()?.name ?? "Select a group";
  const assignments = createMemo(() => subjectGrants(grants.data?.grants ?? [], selected()));
  const activeMembers = () => members.data?.members.filter(m => m.status === "active") ?? [];
  const refresh = async () => { await cache.invalidateQueries({ queryKey: ["access"] }); await cache.invalidateQueries({ queryKey: ["me"] }); };
  const choose = (id: string) => { setSelected(id); setParams({ group: id }); setError(""); };
  const closeAssign = () => { setAssigning(false); setParams({ role: undefined }); };
  const memberChange = async (id: string, add: boolean) => {
    setBusy(true); setError(""); try { await setGroupMember(selected(), id, add); await refresh(); } catch (e) { setError(errorMessage(e)); } finally { setBusy(false); }
  };
  const anyError = () => error() || (groups.error || roles.error || grants.error || members.error || targets.error ? errorMessage(groups.error || roles.error || grants.error || members.error || targets.error) : "");
  return <>
    <PageHeader title="Groups" subtitle="Organize people and choose targets for their roles. Access from different assignments adds together."
      actions={<Show when={allowed()}><button class={ui.secondaryButton} onClick={() => setInspect(true)}>Inspect access</button><button class={ui.button} onClick={() => setEditGroup("new")}>Create group</button></Show>} />
    <Show when={allowed()} fallback={<Notice tone="info">Manage access permission is required to manage groups and assignments.</Notice>}>
      <Show when={anyError()}><Notice tone="danger">{anyError()}</Notice></Show>
      <div class="grid items-start gap-4 lg:grid-cols-[190px_minmax(0,1fr)]">
        <div class="space-y-4"><Panel title="Organization audience"><button class={cx("px-2 py-2 text-left", selected() === ALL && "bg-console-surface-hover text-console-accent")} onClick={() => choose(ALL)}><span class="block font-medium">All members</span><span class="text-control text-console-muted">Every active member, including future members</span></button></Panel><Panel title="Custom groups"><div class="flex flex-col gap-1" aria-label="Permission groups">
            <For each={groups.data?.groups}>{group => <button class={cx("px-2 py-2 text-left", selected() === group.id && "bg-console-surface-hover text-console-accent")} onClick={() => choose(group.id)}><span class="block font-medium">{group.name}</span><span class="text-control text-console-muted">{group.member_ids.length} members</span></button>}</For>
            <Show when={groups.isPending}><p class="px-2 text-control text-console-muted">Loading groups…</p></Show>
          </div>
        </Panel></div>
        <div class={ui.sectionStack}>
          <Panel title={groupName()} actions={<Show when={current()}>{group => <><button class={ui.compactButton} onClick={() => setEditGroup(group())}>Rename</button><button class={ui.compactButton} onClick={() => setDeleting(group())}>Delete</button></>}</Show>}>
            <Show when={selected() === ALL}><p class="text-control text-console-muted">Every active organization member belongs here, including future members. API keys and pending or removed members are excluded. Only Agent use can be assigned, on selected Agents.</p></Show>
            <div class="flex gap-4 border-b border-console-border" role="tablist" aria-label="Group details">
              <button class={cx("border-b-2 pb-2 text-control", tab() === "assignments" ? "border-console-accent text-console-accent" : "border-transparent text-console-muted")} role="tab" aria-selected={tab() === "assignments"} onClick={() => setTab("assignments")}>Role assignments ({assignments().length})</button>
              <button class={cx("border-b-2 pb-2 text-control", tab() === "members" ? "border-console-accent text-console-accent" : "border-transparent text-console-muted")} role="tab" aria-selected={tab() === "members"} onClick={() => setTab("members")}>Members ({selected() === ALL ? activeMembers().length : current()?.member_ids.length ?? 0})</button>
            </div>
            <Show when={tab() === "assignments"} fallback={<div class="divide-y divide-console-border">
              <For each={activeMembers()}>{member => <label class="flex items-center gap-3 py-2"><input type="checkbox" class="accent-console-accent" checked={selected() === ALL || current()?.member_ids.includes(member.membership_id)} disabled={selected() === ALL || busy() || !current()} onChange={e => { if(e.currentTarget.checked) void memberChange(member.membership_id,true); else {e.currentTarget.checked=true;setRemovingMember(member);} }} /><span class="flex-1 text-control">{memberName(member)}<span class="block text-console-muted">{member.email}</span></span><span class="font-mono text-chrome text-console-muted">{member.is_owner ? "Owner" : ""}</span></label>}</For>
            </div>}>
              <div class="flex items-center justify-between gap-2"><p class="text-control text-console-muted">Role contents are shared across their assignments.</p><button class={ui.button} disabled={!roles.data || (!!scope.selectedProjectID() && !targets.data) || (selected() !== ALL && !current())} onClick={() => setAssigning(true)}>Assign role</button></div>
              <Show when={assignments().length} fallback={<div class="py-8 text-center"><p class="font-medium">No roles assigned</p><p class="mt-1 text-control text-console-muted">Choose a role and its target to grant access to this group.</p></div>}>
                <div class="divide-y divide-console-border">
                  <For each={assignments()}>{grant => <div class="flex flex-wrap items-start gap-3 py-3">
                    <div class="min-w-0 flex-1"><A href="/settings/roles" class="font-medium text-console-accent">{grant.role_name}</A>
                      <p class="mt-1 text-control">{grant.target_kind === "organization" ? "Organization" : `${grant.project_name} / ${grant.environment_name} / ${targetSummary(grant)}`}</p>
                      <p class="mt-1 text-control text-console-muted">{grant.permissions.map(p => roles.data?.catalog.find(c => c.id === p)?.name ?? p).join(" · ")}</p>
                      <Show when={grant.permissions.includes("runs.operate")}><p class="mt-1 text-chrome text-console-muted">Operate runs also allows using every Agent in this environment.</p></Show>
                    </div>
                    <button class={ui.secondaryButton} onClick={() => setEditing(grant)}>Edit targets</button><button class={ui.secondaryButton} onClick={() => setRemoving(grant)}>Remove</button>
                  </div>}</For>
                </div>
              </Show>
            </Show>
          </Panel>
        </div>
      </div>
      <Show when={editGroup()} keyed>{group => <GroupEditor group={group} onClose={() => setEditGroup(undefined)} onSaved={async g => { await refresh(); choose(g.id); setEditGroup(undefined); }} />}</Show>
      <Show when={deleting()}>{group => <ConfirmDialog title="Delete group" confirmLabel="Delete group" tone="danger" onClose={() => setDeleting(undefined)} onConfirm={async () => { await deleteGroup(group().id); choose(ALL); await refresh(); }}><p>Delete “{group().name}” and all its memberships and role assignments? Members keep access from their other groups and All members.</p></ConfirmDialog>}</Show>
      <Show when={assigning() && roles.data && groups.data && (!scope.selectedProjectID() || targets.data)}>
        <AssignRoleDialog project={scope.selectedProjectID()} projectName={scope.selectedProject()?.name ?? ""} groups={groups.data!.groups} roles={roles.data!.roles} catalog={roles.data!.catalog} targets={targets.data?.targets ?? []} initialGroup={selected()} initialRole={typeof params["role"] === "string" ? params["role"] : ""} initialEnvironment={scope.selectedEnvironmentID()} onClose={closeAssign} onSaved={async group => { await refresh(); choose(group); closeAssign(); }} />
      </Show>
      <Show when={editing()} keyed>{grant=><EditGrantDialog grant={grant} onClose={()=>setEditing(undefined)} onSaved={refresh} />}</Show>
      <Show when={removing()}>{grant => <ConfirmDialog title="Remove role assignment" confirmLabel="Remove assignment" tone="danger" onClose={() => setRemoving(undefined)} onConfirm={async () => { await removeGrant(grant().id, grant().revision); await refresh(); }}>
        <p>Remove {grant().role_name} on {grant().environment_name} / {targetSummary(grant())} from {groupName()}?</p>
        <p class="mt-2">Other group assignments and All members still apply. This does not stop effects already admitted by running workloads.</p>
        <GrantRemovalImpact grant={grant()} />
      </ConfirmDialog>}</Show>
      <Show when={removingMember()}>{member=><ConfirmDialog title="Remove group member" confirmLabel="Remove from group" onClose={()=>setRemovingMember(undefined)} onConfirm={async()=>{await setGroupMember(selected(),member().membership_id,false);await refresh();}}><p>Remove {memberName(member())} from {groupName()}?</p><GroupMemberRemovalImpact memberID={member().membership_id} groupID={selected()} /></ConfirmDialog>}</Show>
      <Show when={inspect()}><AccessInspector members={activeMembers()} groups={groups.data?.groups ?? []} grants={grants.data?.grants ?? []} catalog={roles.data?.catalog ?? []} onClose={() => setInspect(false)} /></Show>
    </Show>
  </>;
}

function GroupEditor(props: { group: PermissionGroup | "new"; onClose: () => void; onSaved: (g: PermissionGroup) => Promise<void> }) {
  const group = props.group === "new" ? undefined : props.group;
  const [name, setName] = createSignal(group?.name ?? ""); const [error, setError] = createSignal(""); const [busy, setBusy] = createSignal(false);
  return <Dialog title={group ? "Rename group" : "Create group"} onClose={props.onClose} closeDisabled={busy()}><form class="space-y-4" onSubmit={async event => { event.preventDefault(); setBusy(true); setError(""); try { await props.onSaved(await saveGroup(name().trim(), group?.id, group?.revision)); } catch (e) { setError(errorMessage(e)); } finally { setBusy(false); } }}>
    <Field label="Name"><TextInput autofocus value={name()} required maxlength={80} onInput={e => setName(e.currentTarget.value)} placeholder="e.g. Release team" /></Field>
    <Show when={!group}><p class="text-control text-console-muted">New groups have no members or access until you add members and assign roles.</p></Show>
    <Show when={error()}><Notice tone="danger">{error()}</Notice></Show><div class="flex justify-end gap-2"><button type="button" class={ui.secondaryButton} disabled={busy()} onClick={props.onClose}>Cancel</button><button class={ui.button} disabled={busy() || !name().trim()}>{busy() ? "Saving…" : group ? "Save name" : "Create group"}</button></div>
  </form></Dialog>;
}

export function AssignRoleDialog(props: { project:string; projectName:string; groups:PermissionGroup[]; roles:PermissionRole[]; catalog:Capability[]; targets:AccessTarget[]; initialGroup:string; initialMember?:string; initialRole:string; initialEnvironment:string; assignment?:RoleGrant; fixedTarget?:{environment_id:string;agent_id?:string}; onClose:()=>void; onSaved:(group:string)=>Promise<void> }) {
  const project=props.project; const existing=props.assignment; const cache=useQueryClient();
  const members=createQuery(()=>({queryKey:["members"],queryFn:listMembers}));
  const initialRole=props.roles.find(r=>r.id === (existing?.role_id ?? props.initialRole));
  const initialAudience=props.initialGroup === ALL && (!initialRole || initialRole.permissions.length === 1 && initialRole.permissions[0] === "agents.use");
  const [kind,setKind]=createSignal<"member"|"group"|"all_members">(existing?.subject_kind ?? (initialAudience ? "all_members" : props.initialGroup && props.initialGroup !== ALL ? "group" : "member"));
  const [recipient,setRecipient]=createSignal(existing?.member_id ?? existing?.group_id ?? props.initialMember ?? (props.initialGroup === ALL ? "" : props.initialGroup));
  const [roleID,setRoleID]=createSignal(existing?.role_id ?? (props.initialRole || props.roles.find(r=>r.builtin_key === "agent_user")?.id || ""));
  const [env,setEnv]=createSignal(existing?.environment_id ?? props.fixedTarget?.environment_id ?? props.targets.find(t=>t.environment === props.initialEnvironment)?.environment_id ?? "");
  const [mode,setMode]=createSignal<"selected"|"all">(existing?.agents_mode === "all" ? "all" : "selected");
  const [agents,setAgents]=createSignal<string[]>(existing?.agent_ids ?? (props.fixedTarget?.agent_id ? [props.fixedTarget.agent_id] : []));
  const [busy,setBusy]=createSignal(false);const [error,setError]=createSignal("");
  const role=()=>props.roles.find(r=>r.id === roleID());
  const resources=()=>[...new Set((role()?.permissions ?? []).map(p=>props.catalog.find(c=>c.id === p)?.resource_type).filter((r):r is string=>!!r))];
  const organizationRole=()=>resources().includes("organization");
  const hasAgents=()=>resources().includes("agents");
  const environment=()=>props.targets.find(t=>t.environment_id === env());
  const sharedRole=()=>role()?.permissions.length === 1 && role()?.permissions[0] === "agents.use";
  const quickIncompatible=()=>!!props.fixedTarget?.agent_id && !supportsAgent(role()?.permissions ?? [],props.catalog);
  const incompatible=()=>quickIncompatible() || (kind() === "all_members" && (!sharedRole() || mode() !== "selected"));
  const valid=()=>!!role() && (kind() === "all_members" || !!recipient()) && !incompatible() && (organizationRole() || !!environment()) && (!hasAgents() || mode() === "all" || agents().length>0);
  const actionNames=(resource:string)=>(role()?.permissions ?? []).filter(p=>props.catalog.find(c=>c.id===p)?.resource_type===resource).map(p=>props.catalog.find(c=>c.id===p)?.name ?? p).join(" · ");
  return <Dialog title={existing ? "Edit assignment targets" : "Assign role"} onClose={props.onClose} closeDisabled={busy()} class="max-w-180"><form class="space-y-4" onSubmit={async event=>{
    event.preventDefault();if(!valid())return;setBusy(true);setError("");
    try {
      await assignRole(project,{subject_kind:kind(),member_id:kind() === "member" ? recipient() : undefined,group_id:kind() === "group" ? recipient() : null,role_id:roleID(),environment_id:env(),target_kind:organizationRole() ? "organization" : "environment",agents_mode:hasAgents() ? mode() : "none",agent_ids:hasAgents() && mode() === "selected" ? agents() : [],expected_role_revision:role()!.revision},existing);
      await props.onSaved(kind() === "all_members" ? ALL : kind() === "group" ? recipient() : "");
    }catch(e){setError(errorMessage(e));await cache.invalidateQueries({queryKey:["access"]});}finally{setBusy(false);}
  }}>
    <div class="grid gap-4 sm:grid-cols-2"><Field label="Assign to"><Select value={kind()} options={[{value:"member" as const,label:"People"},{value:"group" as const,label:"Groups"},{value:"all_members" as const,label:"All members"}]} onChange={value=>{setKind(value);setRecipient("");if(value === "all_members")setMode("selected");}} disabled={busy() || !!existing} /></Field>
    <Show when={kind() === "member"}><Field label="Person"><Select value={recipient()} options={(members.data?.members ?? []).filter(m=>m.status === "active").map(m=>({value:m.membership_id,label:memberName(m)}))} onChange={setRecipient} disabled={busy() || !!existing} /></Field></Show>
    <Show when={kind() === "group"}><Field label="Group"><Select value={recipient()} options={props.groups.map(g=>({value:g.id,label:g.name}))} onChange={setRecipient} disabled={busy() || !!existing} /></Field></Show></div>
    <Field label="Role"><Select value={roleID()} options={props.roles.map(r=>({value:r.id,label:`${r.name}${r.builtin_key ? " · Standard" : " · Custom"}`}))} onChange={id=>{setRoleID(id);setMode("selected");setAgents(props.fixedTarget?.agent_id ? [props.fixedTarget.agent_id] : []);}} disabled={busy() || !!existing} /></Field>
    <Show when={!organizationRole()}><div class="grid gap-4 sm:grid-cols-2"><Field label="Project"><TextInput value={props.projectName} readonly /></Field><Field label="Environment"><Select value={env()} options={props.targets.map(t=>({value:t.environment_id,label:t.name}))} onChange={id=>{setEnv(id);setMode("selected");setAgents([]);}} disabled={busy() || !!props.fixedTarget || !!existing} /></Field></div><p class="text-control text-console-muted">This assignment applies to one environment. Create another assignment to choose different targets in another environment.</p></Show>
    <Show when={quickIncompatible()}><Notice tone="warning">This role also grants access to other resource types. <A href={`/settings/groups?role=${encodeURIComponent(roleID())}`} class="underline">Open the full assignment editor</A> to review all targets.</Notice></Show>
    <Show when={kind() === "all_members" && !sharedRole()}><Notice tone="warning">All members supports only Use agents on selected Agents.</Notice></Show>
    <div class="divide-y divide-console-border border border-console-border px-4"><For each={resources()}>{resource=><div class="space-y-2 py-3">
      <div class="flex flex-wrap items-center justify-between gap-2"><div><h3 class="font-medium">{resourceLabel(resource)}</h3><p class="text-control text-console-muted">{actionNames(resource)}</p></div>
      <Show when={resource === "agents"} fallback={<span class="text-control">{resource === "organization" ? "Entire organization" : `All ${resourceLabel(resource)} in ${environment()?.name ?? "the selected environment"}`}</span>}><Show when={!props.fixedTarget?.agent_id && kind() !== "all_members"} fallback={<span class="text-control">{props.fixedTarget?.agent_id ? "This Agent only" : "Selected Agents only"}</span>}><Field label="Agent targets"><Select value={mode()} options={[{value:"selected" as const,label:"Selected Agents"},{value:"all" as const,label:`All current and future Agents in ${environment()?.name ?? "this environment"}`}]} onChange={value=>{setMode(value);setAgents([]);}} disabled={busy()} /></Field></Show></Show></div>
      <Show when={resource === "agents" && mode() === "selected"}><fieldset class="max-h-44 space-y-2 overflow-y-auto rounded border border-console-border p-3"><legend class="px-1 text-control">Select Agents in {environment()?.name ?? "this environment"}</legend><For each={environment()?.agents} fallback={<p class="text-control text-console-muted">Choose an environment with Agents.</p>}>{agent=><label class="flex items-center gap-2 text-control"><input type="checkbox" class="accent-console-accent" checked={agents().includes(agent.id)} disabled={busy() || !!props.fixedTarget?.agent_id} onChange={e=>setAgents(ids=>e.currentTarget.checked ? [...ids,agent.id] : ids.filter(id=>id!==agent.id))} />{agent.key}</label>}</For></fieldset></Show>
      <Show when={resource === "agents" && mode() === "all"}><p class="text-control text-console-muted">Includes all current and future Agents in {environment()?.name}.</p></Show>
    </div>}</For></div>
    <Show when={role()?.permissions.includes("runs.operate")}><Notice tone="warning">Operate runs also grants View runs and Use agents for every Agent in {environment()?.name ?? "the selected environment"}. Any selected Agent management targets above remain unchanged.</Notice></Show>
    <div class="space-y-1 text-control"><p class="font-medium">Access summary</p><For each={resources()}>{resource=><p>{resourceLabel(resource)}: {resource === "agents" ? mode() === "all" ? "all current and future Agents" : environment()?.agents.filter(a=>agents().includes(a.id)).map(a=>a.key).join(", ") || "No Agents selected" : resource === "organization" ? "organization" : `entire ${environment()?.name ?? "selected environment"}`} — {actionNames(resource)}</p>}</For></div>
    <Show when={kind() === "all_members"}><Notice tone="info">Current and future active members receive Use agents for these selected Agents. New Agents are not included.</Notice></Show>
    <Show when={error() || members.error}><Notice tone="danger">{error() || errorMessage(members.error)}<Show when={error()}><p class="mt-2">If the role or assignment changed, close this editor and reopen it to review current access before retrying.</p></Show></Notice></Show>
    <div class="flex justify-end gap-2"><button type="button" class={ui.secondaryButton} disabled={busy()} onClick={props.onClose}>Cancel</button><button class={ui.button} disabled={busy() || !valid()}>{busy() ? "Saving…" : existing ? "Save targets" : "Assign role"}</button></div>
  </form></Dialog>;
}

export function EditGrantDialog(props:{grant:RoleGrant;onClose:()=>void;onSaved:()=>Promise<void>}) {
  const targets=createQuery(()=>({queryKey:["access","targets",props.grant.project],queryFn:()=>listAccessTargets(props.grant.project),enabled:props.grant.target_kind !== "organization"}));
  const roles=createQuery(()=>({queryKey:["access","roles"],queryFn:listRoles}));
  const groups=createQuery(()=>({queryKey:["access","groups"],queryFn:listGroups}));
  const error=()=>targets.error || roles.error || groups.error;
  return <Show when={roles.data && groups.data && (props.grant.target_kind === "organization" || targets.data)} fallback={<Dialog title="Edit assignment targets" onClose={props.onClose}><Show when={error()} fallback={<p>Loading targets…</p>}><Notice tone="danger">{errorMessage(error())}</Notice></Show></Dialog>}><AssignRoleDialog project={props.grant.project} projectName={props.grant.project_name} groups={groups.data!.groups} roles={roles.data!.roles} catalog={roles.data!.catalog} targets={targets.data?.targets ?? []} assignment={props.grant} initialGroup="" initialRole="" initialEnvironment="" onClose={props.onClose} onSaved={async()=>{await props.onSaved();props.onClose();}} /></Show>;
}

function AccessInspector(props: { members: OrganizationMember[]; groups: PermissionGroup[]; grants: RoleGrant[]; catalog: Capability[]; onClose: () => void }) {
  const [user, setUser] = createSignal(props.members[0]?.membership_id ?? "");
  const effective = createQuery(() => ({ queryKey: ["access", "member", user(), "permissions"], queryFn: () => getMemberPermissions(user()), enabled: !!user() }));
  return <Dialog title="Inspect effective access" onClose={props.onClose} class="max-w-180">
    <div class="space-y-4"><Field label="Member"><Select value={user()} options={props.members.map(m => ({ value: m.membership_id, label: memberName(m) }))} onChange={setUser} /></Field>

      <p class="text-control text-console-muted">Access is the union of these sources. Removing one assignment leaves the others effective. This preview is informational; each operation checks current permissions.</p>
      <Show when={effective.isPending}><p class="text-control">Loading permissions…</p></Show>
      <Show when={effective.error}><Notice tone="danger">{errorMessage(effective.error)}</Notice></Show>
      <Show when={effective.data}>{data => <div class="divide-y divide-console-border"><For each={data().scopes}>{scope => <div class="py-3"><div class="font-medium">{scope.project} / {scope.environment} / {scope.agent || "Entire environment"}</div><p class="mt-1 text-control">{scope.permissions.join(" · ") || "No permissions"}</p><For each={scope.sources}>{source => <p class="mt-1 text-control text-console-muted">{source.subject_kind === "member" ? `Direct assignment → ${source.role_name}` : source.subject_kind === "all_members" ? `All members → ${source.role_name}` : `${source.group_name} → ${source.role_name}`} — {source.agent || (source.target_kind === "organization" ? "Organization" : "Entire environment")} : {source.included_permissions.join(" · ")}</p>}</For></div>}</For></div>}</Show>
    </div>
  </Dialog>;
}
