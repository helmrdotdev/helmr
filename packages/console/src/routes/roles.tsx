import { useNavigate } from "@solidjs/router";
import { GrantAccessDialog } from "../features/access/GrantAccessDialog";
import type { ActionMenuItem } from "../ui/ActionMenu";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { ManagedMark } from "../features/access/AccessIcon";
import { hasPermission } from "../lib/auth";
import { loadError } from "../lib/error";
import { useMe } from "../lib/me";
import { deleteRole, isOrganizationRole, listRoles, permissionNames, roleHref, type PermissionRole } from "../lib/permissions";
import { DataTable, TableCount, PrimaryLink, RowActions } from "../ui/DataTable";
import { ConfirmDialog } from "../ui/Dialog";
import { SearchInput } from "../ui/Field";
import { Toolbar } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { QueryState, DeniedState } from "../ui/QueryState";
import { TableSkeleton } from "../ui/Skeleton";
import { ui } from "../ui/styles";

const COLUMNS = ["Role", "Level", "Permissions", "Assignments", { label: "Actions", srOnly: true }] as const;
export const MANAGED_ROLE_NOTE = "Managed by Helmr. Duplicate it to make a version you can edit.";
// More permissions than this show as a count, so a row stays one line.
const LISTED_PERMISSIONS = 3;

export function Roles() {
  const me = useMe(); const cache = useQueryClient(); const navigate = useNavigate();
  const allowed = () => hasPermission(me.data, "access.manage");
  const query = createQuery(() => ({ queryKey: ["access", "roles"], queryFn: listRoles, enabled: allowed() }));
  const [filter, setFilter] = createSignal("");
  const [deleting, setDeleting] = createSignal<PermissionRole>();
  const [granting, setGranting] = createSignal<PermissionRole>();
  // The organization's own roles first, then the ones Helmr maintains.
  const roles = createMemo(() => {
    const q = filter().trim().toLowerCase();
    const all = (query.data?.roles ?? []).filter(r => !q || r.name.toLowerCase().includes(q));
    return [...all.filter(r => !r.builtin_key), ...all.filter(r => r.builtin_key)];
  });
  const catalog = () => query.data?.catalog ?? [];
  return <>
    <PageHeader title="Roles" subtitle="Sets of permissions you grant to people, groups and API keys." actions={<Show when={allowed()}><button type="button" class={ui.button} onClick={() => navigate("/settings/roles/new")}>Create role</button></Show>} />
    <Show when={allowed()} fallback={<Show when={!me.isPending}><DeniedState message="You need Manage access permission to manage roles." /></Show>}>
      <Toolbar start={<SearchInput aria-label="Filter roles" placeholder="Filter roles" value={filter()} onInput={e => setFilter(e.currentTarget.value)} />} />
      <QueryState query={query} pending={<TableSkeleton label="Loading roles" columns={COLUMNS} />} error={error => loadError(error, "Could not load roles.")}>
        {() => (
          <DataTable columns={COLUMNS} minWidth="min-w-180" footer={<TableCount count={roles().length} noun={{ one: "role", other: "roles" }} />}>
            <For each={roles()}>
              {role => (
                <tr>
                  <td><span class="inline-flex items-center gap-1.5"><PrimaryLink href={roleHref(role.id)}>{role.name}</PrimaryLink><Show when={role.builtin_key}><ManagedMark title={MANAGED_ROLE_NOTE} /></Show></span></td>
                  <td>{isOrganizationRole(role.permissions, catalog()) ? "Organization" : "Environment"}</td>
                  <td>{role.permissions.length > LISTED_PERMISSIONS ? `${role.permissions.length} permissions` : permissionNames(role.permissions, catalog())}</td>
                  <td class="font-mono text-chrome!">{role.grant_count}</td>
                  <RowActions label={`Actions for ${role.name}`} items={roleMenu(role, { grant: () => setGranting(role), remove: () => setDeleting(role) })} />
                </tr>
              )}
            </For>
          </DataTable>
        )}
      </QueryState>
    </Show>
    <Show when={granting()} keyed>{role => <GrantAccessDialog roleID={role.id} onClose={() => setGranting(undefined)} />}</Show>
    <Show when={deleting()}>{role => <DeleteRoleDialog role={role()} onClose={() => setDeleting(undefined)} onDeleted={() => cache.invalidateQueries({ queryKey: ["access"] })} />}</Show>
  </>;
}

// The object menu for a role, the same in its list row and by its name. Only an
// unassigned role the organization made can be deleted.
export function roleMenu(role: PermissionRole, options: { grant: () => void; remove: () => void }): ActionMenuItem[] {
  return [
    { label: "Grant this role", onSelect: options.grant },
    { label: "Duplicate", href: `/settings/roles/new?from=${encodeURIComponent(role.id)}` },
    ...(role.builtin_key || role.grant_count > 0 ? [] : [{ label: "Delete role", tone: "danger" as const, onSelect: options.remove }]),
  ];
}

export function DeleteRoleDialog(props: { role: PermissionRole; onClose: () => void; onDeleted: () => void }) {
  return (
    <ConfirmDialog title="Delete role" confirmLabel="Delete" busyLabel="Deleting…" tone="danger" target={props.role.name} onClose={props.onClose} onConfirm={async () => { await deleteRole(props.role.id); props.onDeleted(); }}>
      Nobody has this role. Deleting it can't be undone.
    </ConfirmDialog>
  );
}
