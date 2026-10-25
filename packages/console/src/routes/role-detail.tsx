import { useNavigate, useParams, useSearchParams } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, Show } from "solid-js";
import { ManagedMark } from "../features/access/AccessIcon";
import { AssignmentsTable } from "../features/access/AssignmentsTable";
import { GrantAccessDialog } from "../features/access/GrantAccessDialog";
import { hasPermission } from "../lib/auth";
import { errorMessage, loadError } from "../lib/error";
import { useMe } from "../lib/me";
import { isOrganizationRole, listGrants, listRoles, permissionResource, resourceLabel, roleHref, saveRole, type Capability, type PermissionRole } from "../lib/permissions";
import { CheckboxGroup, Field, FormActions, Segmented, TextArea, TextInput } from "../ui/Field";
import { Notice } from "../ui/Notice";
import { DangerZone } from "../ui/Panel";
import { PageHeader } from "../ui/PageHeader";
import { NotFoundState, QueryState } from "../ui/QueryState";
import { PageSkeleton, TableSkeleton } from "../ui/Skeleton";
import { ui } from "../ui/styles";
import { TabPanel, Tabs } from "../ui/Tabs";
import { DeleteRoleDialog, MANAGED_ROLE_NOTE } from "./roles";

type Level = "environment" | "organization";
type Tab = "permissions" | "assignments";

// Agents come first: they are what most roles are about.
const RESOURCE_ORDER = ["agents", "deployments", "computers", "secrets", "api_keys", "organization"];
const resourceRank = (resource: string) => { const i = RESOURCE_ORDER.indexOf(resource); return i < 0 ? RESOURCE_ORDER.length : i; };

// The permission groups a role of this level chooses from.
function permissionGroups(catalog: readonly Capability[], level: Level) {
  const resources = [...new Set(catalog.map(c => c.resource_type))].filter(r => (r === "organization") === (level === "organization")).sort((a, b) => resourceRank(a) - resourceRank(b));
  return resources.map(resource => ({
    label: resource === "agents" ? "Agents and sessions" : resourceLabel(resource),
    options: catalog.filter(c => c.resource_type === resource).map(c => ({ value: c.id, label: c.name, description: c.description })),
  }));
}

export function RoleDetail() {
  const params = useParams(); const [search] = useSearchParams(); const me = useMe();
  const navigate = useNavigate(); const cache = useQueryClient();
  const allowed = () => hasPermission(me.data, "access.manage");
  const id = () => params["role"] ?? "";
  const creating = () => id() === "new";
  const query = createQuery(() => ({ queryKey: ["access", "roles"], queryFn: listRoles, enabled: allowed() }));
  const grants = createQuery(() => ({ queryKey: ["access", "grants"], queryFn: listGrants, enabled: allowed() && !creating() }));
  const catalog = () => query.data?.catalog ?? [];
  const role = () => creating() ? undefined : query.data?.roles.find(r => r.id === id());
  const source = () => creating() ? query.data?.roles.find(r => r.id === search["from"]) : role();
  const missing = () => !creating() && !!query.data && !role();
  const readonly = () => !!role()?.builtin_key;
  const assignments = createMemo(() => grants.data?.grants.filter(g => g.role_id === id()) ?? []);
  const formKey = () => creating() ? `new:${source()?.id ?? ""}` : role() ? `${role()!.id}:${role()!.revision}` : "";
  const [tab, setTab] = createSignal<Tab>("permissions");
  const [granting, setGranting] = createSignal(false);
  const [deleting, setDeleting] = createSignal(false);

  const title = () => creating() ? (source() ? `Duplicate ${source()!.name}` : "Create role") : missing() ? "Role not found" : role()?.name ?? "";
  return <>
    <PageHeader
      title={title()}
      breadcrumb={[{ href: "/settings/roles", label: "Roles" }]}
      badge={<Show when={readonly()}><ManagedMark title={MANAGED_ROLE_NOTE} /></Show>}
      subtitle={readonly() ? role()?.description || undefined : undefined}
      actions={<Show when={allowed() && role()}>
        {r => <>
          <button type="button" class={ui.secondaryButton} onClick={() => navigate(`/settings/roles/new?from=${encodeURIComponent(r().id)}`)}>Duplicate</button>
          <button type="button" class={ui.button} onClick={() => setGranting(true)}>Grant this role</button>
        </>}
      </Show>}
    />
    <Show when={allowed()} fallback={<Show when={!me.isPending}><Notice tone="info">Manage access permission is required to manage roles.</Notice></Show>}>
      <Show when={!missing()} fallback={<NotFoundState noun="role" id={id()} back={{ href: "/settings/roles", label: "Go to roles" }} />}>
        <QueryState query={query} pending={<PageSkeleton label="Loading role" sections={["Permissions"]} />} error={error => loadError(error, "Could not load this role.")}>
          {() => <>
            <Show when={!creating()}>
              <Tabs id="role-tabs" label="Role" items={[{ value: "permissions", label: "Permissions", count: role()?.permissions.length }, { value: "assignments", label: "Assignments", count: role()?.grant_count }]} selected={tab()} onSelect={setTab} />
            </Show>
            {/* The form stays mounted while another tab shows, so unsaved edits survive; it
                starts over only for another role or a newly saved revision. */}
            <div {...(creating() ? {} : { role: "tabpanel", id: "role-tabs-panel-permissions", "aria-labelledby": "role-tabs-tab-permissions" })} hidden={!creating() && tab() !== "permissions"}>
              <Show when={formKey()} keyed>
                {_key => <RoleForm role={source()} copy={creating()} readonly={readonly()} catalog={catalog()} onSaved={saved => creating() ? navigate(roleHref(saved.id)) : undefined} />}
              </Show>
              <Show when={role() && !role()!.builtin_key}>
                <DangerZone
                  title="Delete role"
                  class="mt-6"
                  action={role()!.grant_count > 0
                    ? <button type="button" class={ui.secondaryButton} onClick={() => setTab("assignments")}>View assignments</button>
                    : <button type="button" class={ui.dangerOutlineButton} onClick={() => setDeleting(true)}>Delete role</button>}
                >
                  {role()!.grant_count > 0 ? "Remove this role's assignments before deleting it." : "Nobody has this role. Deleting it can't be undone."}
                </DangerZone>
              </Show>
            </div>
            <TabPanel tabs="role-tabs" value="assignments" selected={tab()}>
              <QueryState query={grants} pending={<TableSkeleton label="Loading assignments" columns={["Recipient", "Where", "Agents", "Permissions"]} />} error={error => loadError(error, "Could not load assignments.")}>
                {() => <AssignmentsTable grants={assignments()} catalog={catalog()} recipient role={false} empty="Nobody has this role" emptyHint="Grant it to give people, groups or API keys these permissions." />}
              </QueryState>
            </TabPanel>
          </>}
        </QueryState>
      </Show>
    </Show>
    <Show when={granting() && role()}>{r => <GrantAccessDialog roleID={r().id} onClose={() => setGranting(false)} />}</Show>
    <Show when={deleting() && role()}>{r => <DeleteRoleDialog role={r()} onClose={() => setDeleting(false)} onDeleted={async () => { await cache.invalidateQueries({ queryKey: ["access"] }); navigate("/settings/roles"); }} />}</Show>
  </>;
}

function RoleForm(props: { role: PermissionRole | undefined; copy: boolean; readonly: boolean; catalog: Capability[]; onSaved: (role: PermissionRole) => void }) {
  const cache = useQueryClient();
  const initial = () => ({
    name: props.role ? (props.copy ? `${props.role.name} copy` : props.role.name) : "",
    description: props.role?.description ?? "",
    permissions: props.role?.permissions ?? [],
  });
  const [name, setName] = createSignal(initial().name);
  const [description, setDescription] = createSignal(initial().description);
  const [permissions, setPermissions] = createSignal<string[]>(initial().permissions);
  const [level, setLevel] = createSignal<Level>(props.role && isOrganizationRole(props.role.permissions, props.catalog) ? "organization" : "environment");
  const [busy, setBusy] = createSignal(false); const [error, setError] = createSignal<string | null>(null);
  const editing = () => !!props.role && !props.copy;
  // A saved role keeps its level; a new one starts empty when its level changes.
  createEffect(() => { if (!editing()) setPermissions(p => p.filter(id => (permissionResource(id, props.catalog) === "organization") === (level() === "organization"))); });
  const changed = () => name() !== initial().name || description() !== initial().description || [...permissions()].sort().join() !== [...initial().permissions].sort().join();
  const agentActions = (ids: readonly string[]) => ids.some(id => permissionResource(id, props.catalog) === "agents");
  const addsSessionReading = () => editing() && permissions().some(p => (p === "sessions.view" || p === "sessions.manage") && !initial().permissions.includes(p));
  const dropsAgents = () => editing() && (props.role?.grant_count ?? 0) > 0 && agentActions(initial().permissions) && !agentActions(permissions());
  const reset = () => { setName(initial().name); setDescription(initial().description); setPermissions(initial().permissions); setError(null); };
  const submit = async () => {
    setBusy(true); setError(null);
    try {
      const saved = await saveRole({ name: name().trim(), description: description().trim(), permissions: permissions() }, editing() ? props.role!.id : undefined, editing() ? props.role!.revision : undefined);
      await cache.invalidateQueries({ queryKey: ["access"] });
      props.onSaved(saved);
    } catch (cause) { setError(errorMessage(cause)); } finally { setBusy(false); }
  };
  return (
    <form onSubmit={event => { event.preventDefault(); void submit(); }}>
      <Show when={!props.readonly}>
        <div class="grid max-w-180 gap-0">
          <Field label="Name"><TextInput value={name()} required maxlength={80} disabled={busy()} onInput={e => setName(e.currentTarget.value)} placeholder="Release operator" /></Field>
          <Field label="Description"><TextArea value={description()} maxlength={1000} rows={2} disabled={busy()} onInput={e => setDescription(e.currentTarget.value)} placeholder="Ships releases and rotates their secrets" /></Field>
          <Show when={!editing()}>
            <Field label="Level" hint={level() === "organization" ? "Granted across the organization." : "Granted in one environment at a time."}>
              <Segmented label="Level" value={level()} options={[{ value: "environment" as const, label: "Environment" }, { value: "organization" as const, label: "Organization" }]} onChange={setLevel} disabled={busy()} />
            </Field>
          </Show>
        </div>
      </Show>
      <CheckboxGroup label="Permissions" groups={permissionGroups(props.catalog, level())} value={permissions()} onChange={setPermissions} disabled={props.readonly || busy()} />
      <Show when={level() === "environment"}>
        <p class={`-mt-1 mb-3 ${ui.fieldHint}`}>Agent and session permissions apply to the agents chosen when the role is granted. The others apply to the whole environment.</p>
      </Show>
      <Show when={addsSessionReading()}><Notice tone="warning" class="mb-3">Viewing or managing all sessions includes other people’s personal conversations with these agents.</Notice></Show>
      <Show when={dropsAgents()}><Notice tone="warning" class="mb-3">Removing every agent permission clears the agents chosen on this role’s assignments.</Notice></Show>
      <Show when={!props.readonly}>
        <Show when={error()}><p class={ui.error} role="alert">{error()}</p></Show>
        <div class="mt-3 flex flex-wrap items-center justify-between gap-2">
          <span class={ui.muted}>{editing() && (props.role?.grant_count ?? 0) > 0 ? `Changes apply to ${props.role!.grant_count} ${props.role!.grant_count === 1 ? "assignment" : "assignments"}.` : ""}</span>
          <FormActions class="flex gap-2" submitLabel={editing() ? "Save role" : "Create role"} busyLabel="Saving…" busy={busy()} cancelLabel="Discard" onCancel={reset} disabled={!name().trim() || permissions().length === 0 || (editing() && !changed())} />
        </div>
      </Show>
    </form>
  );
}
