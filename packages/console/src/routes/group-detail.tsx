import { useNavigate, useParams } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { ManagedMark } from "../features/access/AccessIcon";
import { AssignmentsTable } from "../features/access/AssignmentsTable";
import { GrantAccessDialog } from "../features/access/GrantAccessDialog";
import { GroupMemberRemovalImpact } from "../features/access/GrantRemovalImpact";
import { hasPermission } from "../lib/auth";
import { errorMessage, loadError } from "../lib/error";
import { useMe } from "../lib/me";
import { listMembers, type OrganizationMember } from "../lib/members";
import { ALL_MEMBERS, listGrants, listGroups, listRoles, memberHref, setGroupMember, subjectGrants } from "../lib/permissions";
import { DataTable, EmptyCell, LoadMore, PrimaryLink, RowActions } from "../ui/DataTable";
import { ConfirmDialog, FormDialog } from "../ui/Dialog";
import { SearchInput } from "../ui/Field";
import { Notice } from "../ui/Notice";
import { Toolbar } from "../ui/Page";
import { DangerZone } from "../ui/Panel";
import { PageHeader } from "../ui/PageHeader";
import { EmptyState, NotFoundState, QueryState } from "../ui/QueryState";
import { TableSkeleton } from "../ui/Skeleton";
import { ui } from "../ui/styles";
import { TabPanel, Tabs } from "../ui/Tabs";
import { ALL_MEMBERS_NOTE, DeleteGroupDialog, GroupNameDialog } from "./groups";

type Tab = "access" | "members";
const memberName = (m: OrganizationMember) => m.display_name || m.email || m.membership_id;

export function GroupDetail() {
  const params = useParams(); const me = useMe(); const cache = useQueryClient(); const navigate = useNavigate();
  const allowed = () => hasPermission(me.data, "access.manage");
  const id = () => params["group"] ?? "";
  const isAll = () => id() === ALL_MEMBERS;
  const groups = createQuery(() => ({ queryKey: ["access", "groups"], queryFn: listGroups, enabled: allowed() }));
  const grants = createQuery(() => ({ queryKey: ["access", "grants"], queryFn: listGrants, enabled: allowed() }));
  const roles = createQuery(() => ({ queryKey: ["access", "roles"], queryFn: listRoles, enabled: allowed() }));
  const members = createQuery(() => ({ queryKey: ["members"], queryFn: listMembers, enabled: allowed() }));
  const group = () => groups.data?.groups.find(g => g.id === id());
  const name = () => isAll() ? "All members" : group()?.name ?? "";
  const missing = () => !isAll() && !!groups.data && !group();
  const [tab, setTab] = createSignal<Tab>("access");
  const [granting, setGranting] = createSignal(false);
  const [renaming, setRenaming] = createSignal(false);
  const [deleting, setDeleting] = createSignal(false);
  const [adding, setAdding] = createSignal(false);
  const [removing, setRemoving] = createSignal<OrganizationMember>();
  const [filter, setFilter] = createSignal("");
  const assignments = createMemo(() => subjectGrants(grants.data?.grants ?? [], id()));
  const active = () => (members.data?.members ?? []).filter(m => m.status === "active");
  const inGroup = createMemo(() => isAll() ? active() : active().filter(m => group()?.member_ids.includes(m.membership_id)));
  const visibleMembers = createMemo(() => {
    const q = filter().trim().toLowerCase();
    return inGroup().filter(m => !q || memberName(m).toLowerCase().includes(q) || (m.email ?? "").toLowerCase().includes(q));
  });
  const otherGroups = (member: OrganizationMember) => (groups.data?.groups ?? []).filter(g => g.id !== id() && g.member_ids.includes(member.membership_id)).map(g => g.name).join(", ");

  return <>
    <PageHeader
      title={missing() ? "Group not found" : name()}
      breadcrumb={[{ href: "/settings/groups", label: "Groups" }]}
      badge={<Show when={isAll()}><ManagedMark title={ALL_MEMBERS_NOTE} /></Show>}
      subtitle={isAll() ? "Everyone in the organization, including people who join later. Only agent use on selected agents can be granted here." : undefined}
      actions={<Show when={allowed() && !missing()}>
        <Show when={!isAll()}><button type="button" class={ui.secondaryButton} onClick={() => setRenaming(true)}>Rename</button></Show>
        <button type="button" class={ui.button} disabled={!isAll() && !group()} onClick={() => setGranting(true)}>Grant access</button>
      </Show>}
    />
    <Show when={allowed()} fallback={<Show when={!me.isPending}><Notice tone="info">Manage access permission is required to manage groups.</Notice></Show>}>
      <Show when={!missing()} fallback={<NotFoundState noun="group" id={id()} back={{ href: "/settings/groups", label: "Go to groups" }} />}>
        <QueryState query={groups} pending={<TableSkeleton label="Loading group" columns={["Role", "Where", "Agents", "Permissions"]} />} error={error => loadError(error, "Could not load this group.")}>
          {() => <>
            <Tabs id="group-tabs" label="Group" items={[{ value: "access", label: "Access", count: assignments().length }, { value: "members", label: "Members", count: inGroup().length }]} selected={tab()} onSelect={setTab} />
            <TabPanel tabs="group-tabs" value="access" selected={tab()}>
              <QueryState query={grants} pending={<TableSkeleton label="Loading access" columns={["Role", "Where", "Agents", "Permissions"]} />} error={error => loadError(error, "Could not load access.")}>
                {() => <AssignmentsTable grants={assignments()} catalog={roles.data?.catalog ?? []} empty="No access yet" emptyHint={`Grant a role to give ${isAll() ? "everyone" : "this group's members"} access.`} />}
              </QueryState>
            </TabPanel>
            <TabPanel tabs="group-tabs" value="members" selected={tab()}>
              <Toolbar
                start={<SearchInput aria-label="Filter members" placeholder="Filter by name or email" value={filter()} onInput={e => setFilter(e.currentTarget.value)} />}
                end={<Show when={!isAll()}><button type="button" class={ui.secondaryButton} onClick={() => setAdding(true)}>Add members</button></Show>}
              />
              <QueryState query={members} pending={<TableSkeleton label="Loading members" columns={["Member", "Email", "Other groups"]} />} error={error => loadError(error, "Could not load members.")}>
                {() => (
                  <Show when={inGroup().length > 0} fallback={<EmptyState title="No members yet" hint="Add people to give them this group's access." />}>
                    <DataTable columns={["Member", "Email", "Other groups", { label: "Actions", srOnly: true }]} footer={<LoadMore count={visibleMembers().length} noun={{ one: "member", other: "members" }} hasMore={false} loading={false} onLoadMore={() => undefined} />}>
                      <For each={visibleMembers()}>
                        {member => (
                          <tr>
                            <td><PrimaryLink href={memberHref(member.membership_id)}>{memberName(member)}</PrimaryLink></td>
                            <td class="text-console-muted!">{member.email ?? <EmptyCell />}</td>
                            <td><Show when={otherGroups(member)} fallback={<EmptyCell />}>{text => text()}</Show></td>
                            <RowActions label={`Actions for ${memberName(member)}`} items={isAll() ? [] : [{ label: "Remove from group", tone: "danger", onSelect: () => setRemoving(member) }]} />
                          </tr>
                        )}
                      </For>
                    </DataTable>
                  </Show>
                )}
              </QueryState>
            </TabPanel>
            <Show when={group()}>{g => (
              <DangerZone title="Delete group" class="mt-6" action={<button type="button" class={ui.dangerOutlineButton} onClick={() => setDeleting(true)}>Delete group</button>}>
                Removes {g().name} with its members and access. Access from other sources stays.
              </DangerZone>
            )}</Show>
          </>}
        </QueryState>
      </Show>
    </Show>
    <Show when={granting()}>
      <GrantAccessDialog recipient={isAll() ? { kind: "all_members", id: "", name: "All members" } : { kind: "group", id: id(), name: name() }} onClose={() => setGranting(false)} />
    </Show>
    <Show when={renaming() && group()}>{g => <GroupNameDialog group={g()} onClose={() => setRenaming(false)} />}</Show>
    <Show when={deleting() && group()}>{g => <DeleteGroupDialog group={g()} onClose={() => setDeleting(false)} onDeleted={async () => { await cache.invalidateQueries({ queryKey: ["access"] }); navigate("/settings/groups"); }} />}</Show>
    <Show when={adding() && group()}>{g => <AddMembersDialog groupID={g().id} candidates={active().filter(m => !g().member_ids.includes(m.membership_id))} onClose={() => setAdding(false)} />}</Show>
    <Show when={removing()}>
      {member => (
        <ConfirmDialog title="Remove from group" confirmLabel="Remove" busyLabel="Removing…" tone="danger" target={`${memberName(member())} · ${name()}`} onClose={() => setRemoving(undefined)}
          onConfirm={async () => { await setGroupMember(id(), member().membership_id, false); await Promise.all([cache.invalidateQueries({ queryKey: ["access"] }), cache.invalidateQueries({ queryKey: ["me"] })]); }}>
          <GroupMemberRemovalImpact memberID={member().membership_id} groupID={id()} />
        </ConfirmDialog>
      )}
    </Show>
  </>;
}

function AddMembersDialog(props: { groupID: string; candidates: OrganizationMember[]; onClose: () => void }) {
  const cache = useQueryClient();
  const [chosen, setChosen] = createSignal<string[]>([]);
  const [busy, setBusy] = createSignal(false); const [error, setError] = createSignal<string | null>(null);
  return (
    <FormDialog
      title="Add members"
      submitLabel="Add members"
      busyLabel="Adding…"
      busy={busy()}
      disabled={chosen().length === 0}
      error={error()}
      onClose={props.onClose}
      onSubmit={async () => {
        setBusy(true); setError(null);
        try {
          for (const member of chosen()) await setGroupMember(props.groupID, member, true);
          await Promise.all([cache.invalidateQueries({ queryKey: ["access"] }), cache.invalidateQueries({ queryKey: ["me"] })]);
          props.onClose();
        } catch (cause) { setError(errorMessage(cause)); await cache.invalidateQueries({ queryKey: ["access"] }); } finally { setBusy(false); }
      }}
    >
      <Show when={props.candidates.length > 0} fallback={<p class={ui.muted}>Every active member is already in this group.</p>}>
        <div class="grid max-h-72 gap-1.5 overflow-y-auto">
          <For each={props.candidates}>
            {member => (
              <label class="flex cursor-pointer items-center gap-2 text-control text-console-text">
                <input type="checkbox" class="accent-console-accent" checked={chosen().includes(member.membership_id)} disabled={busy()} onChange={e => setChosen(ids => e.currentTarget.checked ? [...ids, member.membership_id] : ids.filter(v => v !== member.membership_id))} />
                <span>{memberName(member)}</span>
                <Show when={member.email && member.email !== memberName(member)}><span class="text-console-muted">{member.email}</span></Show>
              </label>
            )}
          </For>
        </div>
      </Show>
    </FormDialog>
  );
}
