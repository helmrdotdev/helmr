import { useNavigate } from "@solidjs/router";
import { GrantAccessDialog } from "../features/access/GrantAccessDialog";
import type { ActionMenuItem } from "../ui/ActionMenu";
import { AddMembersDialog } from "../features/access/AddMembersDialog";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { ManagedMark } from "../features/access/AccessIcon";
import { hasPermission } from "../lib/auth";
import { errorMessage, loadError } from "../lib/error";
import { useMe } from "../lib/me";
import { listMembers } from "../lib/members";
import { ALL_MEMBERS, deleteGroup, grantWhere, groupHref, listGrants, listGroups, saveGroup, subjectGrants, type PermissionGroup, type RoleGrant } from "../lib/permissions";
import { DataTable, EmptyCell, TableCount, PrimaryLink, RowActions } from "../ui/DataTable";
import { ConfirmDialog, FormDialog } from "../ui/Dialog";
import { Field, SearchInput, TextInput } from "../ui/Field";
import { Toolbar } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { QueryState, DeniedState } from "../ui/QueryState";
import { TableSkeleton } from "../ui/Skeleton";
import { ui } from "../ui/styles";

const COLUMNS = ["Group", "Members", "Assignments", "Where", { label: "Actions", srOnly: true }] as const;
const failure = (error: unknown) => loadError(error, "Could not load groups.");
export const ALL_MEMBERS_NOTE = "Managed by Helmr. Everyone in the organization, including people who join later.";

// Where a recipient's assignments apply, without repeats.
const grantPlaces = (grants: readonly RoleGrant[]) => [...new Set(grants.map(grantWhere))].join(", ");

export function Groups() {
  const me = useMe(); const cache = useQueryClient(); const navigate = useNavigate();
  const allowed = () => hasPermission(me.data, "access.manage");
  const groups = createQuery(() => ({ queryKey: ["access", "groups"], queryFn: listGroups, enabled: allowed() }));
  const grants = createQuery(() => ({ queryKey: ["access", "grants"], queryFn: listGrants, enabled: allowed() }));
  const members = createQuery(() => ({ queryKey: ["members"], queryFn: listMembers, enabled: allowed() }));
  const [filter, setFilter] = createSignal("");
  const [creating, setCreating] = createSignal(false);
  const [renaming, setRenaming] = createSignal<PermissionGroup>();
  const [deleting, setDeleting] = createSignal<PermissionGroup>();
  const [granting, setGranting] = createSignal<{ id: string; name: string }>();
  const [adding, setAdding] = createSignal<PermissionGroup>();
  const query = () => filter().trim().toLowerCase();
  const visible = createMemo(() => (groups.data?.groups ?? []).filter(g => !query() || g.name.toLowerCase().includes(query())));
  const showAll = () => !query() || "all members".includes(query());
  const active = createMemo(() => new Set((members.data?.members ?? []).filter(m => m.status === "active").map(m => m.membership_id)));
  // Counts only current members, as the group page does.
  const memberCount = (ids: readonly string[]) => members.data ? ids.filter(id => active().has(id)).length : ids.length;
  const assignments = (id: string) => subjectGrants(grants.data?.grants ?? [], id);
  const places = (id: string) => grantPlaces(assignments(id));
  return <>
    <PageHeader title="Groups" subtitle="Grant roles to several people at once." actions={<Show when={allowed()}><button type="button" class={ui.button} onClick={() => setCreating(true)}>Create group</button></Show>} />
    <Show when={allowed()} fallback={<Show when={!me.isPending}><DeniedState message="You need Manage access permission to manage groups." /></Show>}>
      <Toolbar start={<SearchInput aria-label="Filter groups" placeholder="Filter groups" value={filter()} onInput={e => setFilter(e.currentTarget.value)} />} />
      <QueryState query={groups} pending={<TableSkeleton label="Loading groups" columns={COLUMNS} />} error={failure}>
        {() => (
          <DataTable columns={COLUMNS} footer={<TableCount count={visible().length + (showAll() ? 1 : 0)} noun={{ one: "group", other: "groups" }} />}>
            <Show when={showAll()}>
              <tr>
                <td><span class="inline-flex items-center gap-1.5"><PrimaryLink href={groupHref(ALL_MEMBERS)}>All members</PrimaryLink><ManagedMark title={ALL_MEMBERS_NOTE} /></span></td>
                <td class="font-mono text-chrome!">{members.data ? active().size : "—"}</td>
                <td class="font-mono text-chrome!">{assignments(ALL_MEMBERS).length}</td>
                <td><Show when={places(ALL_MEMBERS)} fallback={<EmptyCell />}>{text => <span class="font-mono text-chrome">{text()}</span>}</Show></td>
                <RowActions label="Actions for All members" items={groupMenu({ all: true, membersLoaded: !!members.data, grant: () => setGranting({ id: ALL_MEMBERS, name: "All members" }), add: () => undefined, rename: () => undefined, remove: () => undefined })} />
              </tr>
            </Show>
            <For each={visible()}>
              {group => (
                <tr>
                  <td><PrimaryLink href={groupHref(group.id)}>{group.name}</PrimaryLink></td>
                  <td class="font-mono text-chrome!">{memberCount(group.member_ids)}</td>
                  <td class="font-mono text-chrome!">{assignments(group.id).length}</td>
                  <td><Show when={places(group.id)} fallback={<EmptyCell />}>{text => <span class="font-mono text-chrome">{text()}</span>}</Show></td>
                  <RowActions label={`Actions for ${group.name}`} items={groupMenu({ all: false, membersLoaded: !!members.data, grant: () => setGranting({ id: group.id, name: group.name }), add: () => setAdding(group), rename: () => setRenaming(group), remove: () => setDeleting(group) })} />
                </tr>
              )}
            </For>
          </DataTable>
        )}
      </QueryState>
    </Show>
    <Show when={creating()}><GroupNameDialog onClose={() => setCreating(false)} onSaved={g => navigate(groupHref(g.id))} /></Show>
    <Show when={renaming()} keyed>{group => <GroupNameDialog group={group} onClose={() => setRenaming(undefined)} />}</Show>
    <Show when={granting()} keyed>{target => <GrantAccessDialog recipient={target.id === ALL_MEMBERS ? { kind: "all_members", id: "", name: "All members" } : { kind: "group", id: target.id, name: target.name }} onClose={() => setGranting(undefined)} />}</Show>
    <Show when={adding()} keyed>{group => <AddMembersDialog groupID={group.id} candidates={(members.data?.members ?? []).filter(m => m.status === "active" && !group.member_ids.includes(m.membership_id))} onClose={() => setAdding(undefined)} />}</Show>
    <Show when={deleting()}>{group => <DeleteGroupDialog group={group()} onClose={() => setDeleting(undefined)} onDeleted={() => cache.invalidateQueries({ queryKey: ["access"] })} />}</Show>
  </>;
}

// The object menu for a group, the same in its list row and by its name. All
// members can only be granted access. Adding waits for the member list.
export function groupMenu(options: { all: boolean; membersLoaded: boolean; grant: () => void; add: () => void; rename: () => void; remove: () => void }): ActionMenuItem[] {
  return [
    { label: "Grant access", onSelect: options.grant },
    ...(options.all ? [] : [
      { label: "Add members", disabled: !options.membersLoaded, onSelect: options.add },
      { label: "Rename", onSelect: options.rename },
      { label: "Delete group", tone: "danger" as const, onSelect: options.remove },
    ]),
  ];
}

export function GroupNameDialog(props: { group?: PermissionGroup; onClose: () => void; onSaved?: (group: PermissionGroup) => void }) {
  const cache = useQueryClient();
  const [name, setName] = createSignal(props.group?.name ?? "");
  const [busy, setBusy] = createSignal(false); const [error, setError] = createSignal<string | null>(null);
  return (
    <FormDialog
      title={props.group ? "Rename group" : "Create group"}
      intro={props.group ? undefined : "A new group has no members or access until you add people and grant roles."}
      submitLabel={props.group ? "Rename" : "Create group"}
      busyLabel="Saving…"
      busy={busy()}
      disabled={!name().trim()}
      error={error()}
      onClose={props.onClose}
      onSubmit={async () => {
        setBusy(true); setError(null);
        try {
          const saved = await saveGroup(name().trim(), props.group?.id, props.group?.revision);
          await cache.invalidateQueries({ queryKey: ["access"] });
          props.onClose(); props.onSaved?.(saved);
        } catch (cause) { setError(errorMessage(cause)); } finally { setBusy(false); }
      }}
    >
      <Field label="Name" flush><TextInput autofocus value={name()} required maxlength={80} onInput={e => setName(e.currentTarget.value)} placeholder="Release team" /></Field>
    </FormDialog>
  );
}

export function DeleteGroupDialog(props: { group: PermissionGroup; onClose: () => void; onDeleted: () => void }) {
  return (
    <ConfirmDialog title="Delete group" confirmLabel="Delete" busyLabel="Deleting…" tone="danger" target={props.group.name} onClose={props.onClose} onConfirm={async () => { await deleteGroup(props.group.id); props.onDeleted(); }}>
      Its members lose the access this group grants. Access from their other groups, direct assignments and all members stays.
    </ConfirmDialog>
  );
}
