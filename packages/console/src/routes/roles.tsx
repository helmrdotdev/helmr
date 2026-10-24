import { hasPermission } from "../lib/auth";
import { A, useNavigate } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createSignal, For, Show } from "solid-js";
import { errorMessage } from "../lib/error";
import { useMe } from "../lib/me";
import { deleteRole, resourceLabel, targetSummary, includedPermissions, listRoles, listGroups, listGrants, saveRole, type Capability, type PermissionRole } from "../lib/permissions";
import { ConfirmDialog } from "../ui/Dialog";
import { Field, TextArea, TextInput } from "../ui/Field";
import { Notice } from "../ui/Notice";
import { PageHeader } from "../ui/PageHeader";
import { Panel } from "../ui/Panel";
import { ui } from "../ui/styles";

export function Roles() {
  const me = useMe(); const navigate = useNavigate(); const cache = useQueryClient();
  const allowed = () => hasPermission(me.data, "access.manage");
  const query = createQuery(() => ({ queryKey: ["access", "roles"], queryFn: listRoles, enabled: allowed() }));
  const [editing, setEditing] = createSignal<PermissionRole | "new">();
  const [deleting, setDeleting] = createSignal<PermissionRole>();
  const [saved, setSaved] = createSignal("");
  const refresh = () => cache.invalidateQueries({ queryKey: ["access"] });
  const copy = (role: PermissionRole) => setEditing({ ...role, id: "", name: `${role.name} copy`, builtin_key: null, grant_count: 0, group_count: 0 });
  return <>
    <PageHeader title="Roles" subtitle="Roles define allowed actions. Choose recipients, environments and Agent targets when assigning them."
      actions={<Show when={allowed() && !editing()}><button class={ui.button} onClick={() => { setSaved(""); setEditing("new"); }}>Create role</button></Show>} />
    <Show when={allowed()} fallback={<Notice tone="info">Manage access permission is required to manage roles.</Notice>}>
      <Show when={query.error}><Notice tone="danger">{errorMessage(query.error)}</Notice></Show>
      <Show when={saved()}><Notice tone="success">{saved()}</Notice></Show>
      <Show when={!query.isPending} fallback={<p class={ui.muted}>Loading roles…</p>}>
        <Show when={editing()} keyed fallback={
          <Panel title="Organization roles"><p class="text-control text-console-muted">Standard roles are ready to assign. Create a custom role only when you need a different set of permissions.</p>
            <div class="divide-y divide-console-border">
              <For each={query.data?.roles}>{role => <div class="flex flex-wrap items-center gap-3 py-3 first:pt-0 last:pb-0">
                <div class="min-w-0 flex-1"><button class="text-left font-medium text-console-accent hover:underline" onClick={() => setEditing(role)}>{role.name}</button>
                  <span class="ml-2 border border-console-border px-1.5 py-0.5 font-mono text-chrome text-console-muted">{role.builtin_key ? "Standard" : "Custom"}</span>
                  <p class="mt-1 text-control text-console-muted">{role.description}</p>
                  <p class="mt-1 font-mono text-chrome text-console-subtle">{role.permissions.map(p => query.data?.catalog.find(c => c.id === p)?.name ?? p).join(" · ")}</p>
                </div>
                <button class="text-control text-console-accent hover:underline" onClick={() => setEditing(role)} aria-label={`View assignments for ${role.name}`}>{role.grant_count} assignments · View</button>
                <A href={`/settings/groups?role=${encodeURIComponent(role.id)}`} class={ui.secondaryButton}>Assign</A>
                <button class={ui.secondaryButton} onClick={() => copy(role)}>Copy</button>
                <Show when={!role.builtin_key}><button class={ui.secondaryButton} onClick={() => setDeleting(role)}>Delete</button></Show>
              </div>}</For>
            </div>
          </Panel>
        }>{current => <RoleEditor role={current} catalog={query.data?.catalog ?? []} onCancel={() => setEditing(undefined)} onCopy={copy}
          onSaved={async (role, assign) => { await refresh(); setEditing(undefined); if (assign) navigate(`/settings/groups?role=${encodeURIComponent(role.id)}`); else setSaved(`“${role.name}” saved. A role grants access only after it is assigned.`); }} />}</Show>
      </Show>
      <Show when={deleting()}>{role => <ConfirmDialog title="Delete role" confirmLabel="Delete role" onClose={() => setDeleting(undefined)} onConfirm={async () => { await deleteRole(role().id); await refresh(); setDeleting(undefined); }}>
        <p>Delete “{role().name}”? {role().grant_count > 0 ? `It has ${role().grant_count} assignments. Remove those assignments first.` : "This role has no assignments."}</p>
      </ConfirmDialog>}</Show>
    </Show>
  </>;
}

function RoleEditor(props: { role: PermissionRole | "new"; catalog: Capability[]; onCancel: () => void; onCopy: (role: PermissionRole) => void; onSaved: (role: PermissionRole, assign: boolean) => Promise<void> }) {
  const initial = props.role === "new" ? undefined : props.role;
  const [name, setName] = createSignal(initial?.name ?? ""); const [description, setDescription] = createSignal(initial?.description ?? "");
  const [permissions, setPermissions] = createSignal<string[]>(initial?.permissions ?? []);
  const [busy, setBusy] = createSignal(false); const [error, setError] = createSignal("");
  const readonly = !!initial?.builtin_key;
  const grants = createQuery(() => ({ queryKey: ["access", "grants"], queryFn: listGrants, enabled: !!initial?.id }));
  const groups = createQuery(() => ({ queryKey: ["access", "groups"], queryFn: listGroups, enabled: !!initial?.id }));
  const affected = () => grants.data?.grants.filter(g => g.role_id === initial?.id) ?? [];

  const toggle = (id: string, checked: boolean) => setPermissions(p => checked ? [...p, id] : p.filter(v => v !== id));
  const submit = async (assign: boolean) => {
    setBusy(true); setError("");
    try { const saved = await saveRole({ name: name().trim(), description: description().trim(), permissions: permissions() }, initial?.id || undefined, initial?.revision); await props.onSaved(saved, assign); }
    catch (e) { setError(errorMessage(e)); } finally { setBusy(false); }
  };
  const domains = [...new Set(props.catalog.map(c=>c.resource_type))].map(type=>({type,name:resourceLabel(type)}));
  return <div class={ui.sectionStack}>
    <Show when={initial?.id}>
      <Panel title={`Assignments · ${initial?.name}`}>
        <p class="text-control text-console-muted">Each assignment chooses its recipient and targets. Access from other roles and groups adds together.</p>
        <Show when={grants.error || groups.error} fallback={
          <Show when={!grants.isPending && !groups.isPending} fallback={<p class="text-control text-console-muted" role="status">Loading assignments…</p>}>
            <Show when={affected().length > 0} fallback={<p class="text-control text-console-muted">No assignments. This role grants no access until it is assigned.</p>}>
              <div class="overflow-x-auto">
                <table class="w-full min-w-150 text-left text-control">
                  <thead><tr class="border-b border-console-border text-console-muted"><th scope="col" class="py-2 pr-4 font-medium">Recipient</th><th scope="col" class="py-2 pr-4 font-medium">Project / environment</th><th scope="col" class="py-2 font-medium">Targets</th></tr></thead>
                  <tbody><For each={affected()}>{grant => <tr class="border-b border-console-border last:border-0">
                    <td class="py-3 pr-4 align-top"><span class="font-medium">{grant.subject_kind === "all_members" ? "All members" : grant.subject_kind === "member" ? grant.member_name || grant.member_id : groups.data?.groups.find(g => g.id === grant.group_id)?.name ?? grant.group_id}</span><span class="block text-console-muted">{grant.subject_kind === "member" ? "Person" : grant.subject_kind === "group" ? "Group" : "Current and future active members"}</span></td>
                    <td class="py-3 pr-4 align-top">{grant.target_kind === "organization" ? "Organization" : `${grant.project_name} / ${grant.environment_name}`}</td>
                    <td class="space-y-2 py-3 align-top">
                      <p>{targetSummary(grant)}</p>
                      <Show when={grant.targets.some(target => target.mode === "all")}><span class="inline-block border border-console-border px-2 py-1 text-chrome">Includes future Agents</span></Show>

                    </td>
                  </tr>}</For></tbody>
                </table>
              </div>
            </Show>
          </Show>
        }><Notice tone="danger">Could not load assignments: {errorMessage(grants.error || groups.error)}</Notice></Show>
      </Panel>
    </Show>
    <Panel title={readonly ? "Standard role" : initial?.id ? "Edit role" : "Create role"}>
      <form onSubmit={event => { event.preventDefault(); void submit(false); }} class="space-y-4">
        <Field label="Name"><TextInput value={name()} required maxlength={80} disabled={readonly || busy()} onInput={e => setName(e.currentTarget.value)} placeholder="e.g. Production release operator" /></Field>
        <Field label="Description"><TextArea value={description()} maxlength={1000} rows={2} disabled={readonly || busy()} onInput={e => setDescription(e.currentTarget.value)} placeholder="What this role is responsible for" /></Field>
        <div><h2 class="mb-1 font-medium">Permissions</h2><p class="text-control text-console-muted">Select allowed actions here. Organization permissions must stay separate from workload permissions. Choose people or Groups and their targets when assigning the role. Agent targets can be selected individually or include all current and future Agents in an environment.</p></div>
        <div class="divide-y divide-console-border border-y border-console-border">
          <For each={domains}>{domain => <fieldset class="grid gap-3 py-4 sm:grid-cols-[130px_1fr]"><legend class="sr-only">{domain.name}</legend><span class="text-control font-medium">{domain.name}</span><div class="space-y-3">
            <For each={props.catalog.filter(c => c.resource_type === domain.type)}>{cap => <label class="flex cursor-pointer items-start gap-2.5">
              <input type="checkbox" class="mt-1 accent-console-accent" checked={permissions().includes(cap.id)} disabled={readonly || busy()} onChange={e => toggle(cap.id, e.currentTarget.checked)} />
              <span><span class="text-control font-medium">{cap.name}</span><span class="mt-0.5 block text-control text-console-muted">{cap.description}</span></span>
            </label>}</For>
          </div></fieldset>}</For>
        </div>
        <div class="text-control text-console-muted">Includes: {includedPermissions(permissions(), props.catalog).map(id => props.catalog.find(c => c.id === id)?.name ?? id).join(" · ") || "No permissions selected"}.</div>
        <Show when={initial?.id && !readonly}><div class="space-y-1 text-control"><p>Added actions: {permissions().filter(p=>!initial!.permissions.includes(p)).map(p=>props.catalog.find(c=>c.id===p)?.name ?? p).join(" · ") || "None"}</p><p>Removed actions: {initial!.permissions.filter(p=>!permissions().includes(p)).map(p=>props.catalog.find(c=>c.id===p)?.name ?? p).join(" · ") || "None"}</p><Show when={permissions().includes("sessions.manage") && !initial!.permissions.includes("sessions.manage")}><Notice tone="warning">Manage all sessions includes viewing personal conversations for the assigned Agents. Sending input, answering questions or resuming also requires Use agents for the same Agent.</Notice></Show><Show when={initial!.permissions.some(p=>props.catalog.find(c=>c.id===p)?.resource_type === "agents") && !permissions().some(p=>props.catalog.find(c=>c.id===p)?.resource_type === "agents")}><Notice tone="warning">Removing the last Agent action clears stored Agent selections. Re-adding Agent actions later requires new explicit assignments.</Notice></Show></div></Show>
        <Show when={initial && !readonly && initial.grant_count > 0}><Notice tone="warning">Saving changes affects {initial!.grant_count} assignments across {initial!.group_count} groups, including other Projects. Existing Agent selections are preserved. Added environment permissions apply to each assignment’s environment. Adding Agent permissions requires targets on every assignment; copy the role if those targets are missing.</Notice></Show>
        <Show when={readonly}><Notice tone="info">Helmr maintains this role. Copy it to create a custom role with different permissions.</Notice></Show>
        <Show when={error()}><Notice tone="danger">{error()}</Notice></Show>
        <div class="flex flex-wrap justify-end gap-2">
          <button type="button" class={ui.secondaryButton} disabled={busy()} onClick={props.onCancel}>Back to roles</button>
          <Show when={readonly} fallback={<>
            <button type="submit" class={ui.secondaryButton} disabled={busy() || (!!initial?.id && (grants.isPending || groups.isPending || !!grants.error || !!groups.error)) || !name().trim() || !permissions().length}>{busy() ? "Saving…" : "Save role"}</button>
            <button type="button" class={ui.button} disabled={busy() || (!!initial?.id && (grants.isPending || groups.isPending || !!grants.error || !!groups.error)) || !name().trim() || !permissions().length} onClick={() => void submit(true)}>Save & assign</button>
          </>}><button type="button" class={ui.button} onClick={() => props.onCopy(initial!)}>Copy to custom role</button></Show>
        </div>
      </form>
    </Panel>
  </div>;
}
