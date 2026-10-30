import { useNavigate, useParams } from "@solidjs/router";
import { AddMembersDialog } from "../features/access/AddMembersDialog";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { ManagedMark } from "../features/access/AccessIcon";
import { AssignmentsTable } from "../features/access/AssignmentsTable";
import { GrantAccessDialog } from "../features/access/GrantAccessDialog";
import { GroupMemberRemovalImpact } from "../features/access/GrantRemovalImpact";
import { hasPermission } from "../lib/auth";
import { loadError } from "../lib/error";
import { useMe } from "../lib/me";
import { listMembers, type OrganizationMember } from "../lib/members";
import { ALL_MEMBERS, listGrants, listGroups, listRoles, memberHref, setGroupMember, subjectGrants } from "../lib/permissions";
import { DataTable, EmptyCell, TableCount, PrimaryLink, RowActions } from "../ui/DataTable";
import { ConfirmDialog } from "../ui/Dialog";
import { SearchInput } from "../ui/Field";
import { Toolbar } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { EmptyState, NotFoundState, QueryState, DeniedState } from "../ui/QueryState";
import { TableSkeleton } from "../ui/Skeleton";
import { ui } from "../ui/styles";
import { TabPanel, Tabs } from "../ui/Tabs";
import { ALL_MEMBERS_NOTE, DeleteGroupDialog, GroupNameDialog, groupMenu } from "./groups";
import { useAuditItem } from "../features/audit/auditLink";

type Tab = "access" | "members";
const memberName = (m: OrganizationMember) => m.display_name || m.email || m.membership_id;

export function GroupDetail() {
  const params = useParams(); const me = useMe(); const cache = useQueryClient(); const navigate = useNavigate();
  const auditItem = useAuditItem();
  const allowed = () => hasPermission(me.data, "groups.read");
  const id = () => params["group"] ?? "";
  const isAll = () => id() === ALL_MEMBERS;
  const groups = createQuery(() => ({ queryKey: ["access", "groups"], queryFn: listGroups, enabled: allowed() }));
  const grants = createQuery(() => ({ queryKey: ["access", "grants"], queryFn: listGrants, enabled: hasPermission(me.data, "assignments.read") }));
  const roles = createQuery(() => ({ queryKey: ["access", "roles"], queryFn: listRoles, enabled: hasPermission(me.data, "roles.read") }));
  const members = createQuery(() => ({ queryKey: ["members"], queryFn: listMembers, enabled: hasPermission(me.data, "members.read") }));
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
      subtitle={isAll() ? "Everyone in the organization, including people who join later." : undefined}
      menu={allowed() && !missing() && (isAll() || group()) ? groupMenu({ all: isAll(), membersLoaded: !!members.data, can: permission => hasPermission(me.data, permission), grant: () => setGranting(true), add: () => setAdding(true), rename: () => setRenaming(true), remove: () => setDeleting(true) }).concat(isAll() ? [] : auditItem({ target: id(), target_kind: "permission_group" })) : undefined}
      menuLabel="Group actions"
    />
    <Show when={allowed()} fallback={<Show when={!me.isPending}><DeniedState message="You need Read groups permission to view groups." /></Show>}>
      <Show when={!missing()} fallback={<NotFoundState noun="group" id={id()} back={{ href: "/settings/groups", label: "Go to groups" }} />}>
        <QueryState query={groups} pending={<TableSkeleton label="Loading group" columns={["Role", "Where", "Agents", "Permissions"]} />} error={error => loadError(error, "Could not load this group.")}>
          {() => <>
            <Tabs id="group-tabs" label="Group" items={[{ value: "access", label: "Access", count: assignments().length }, { value: "members", label: "Members", count: inGroup().length }]} selected={tab()} onSelect={setTab} />
            <TabPanel tabs="group-tabs" value="access" selected={tab()}><Show when={hasPermission(me.data, "assignments.read")} fallback={<DeniedState message="You need Read assignments permission to view this section." />}>
              <QueryState query={grants} pending={<TableSkeleton label="Loading access" columns={["Role", "Where", "Agents", "Permissions"]} />} error={error => loadError(error, "Could not load access.")}>
                {() => <AssignmentsTable grants={assignments()} catalog={roles.data?.catalog ?? []} empty="No access yet" emptyHint={`Grant a role to give ${isAll() ? "everyone" : "this group's members"} access.`} />}
              </QueryState>
            </Show></TabPanel>
            <TabPanel tabs="group-tabs" value="members" selected={tab()}><Show when={hasPermission(me.data, "members.read")} fallback={<DeniedState message="You need Read members permission to view this section." />}>
              <Toolbar
                start={<SearchInput aria-label="Filter members" placeholder="Filter by name or email" value={filter()} onInput={e => setFilter(e.currentTarget.value)} />}
                end={<Show when={!isAll() && hasPermission(me.data, "groups.add_member")}><button type="button" class={ui.secondaryButton} onClick={() => setAdding(true)}>Add members</button></Show>}
              />
              <QueryState query={members} pending={<TableSkeleton label="Loading members" columns={["Member", "Email", "Other groups"]} />} error={error => loadError(error, "Could not load members.")}>
                {() => (
                  <Show when={inGroup().length > 0} fallback={<EmptyState title="No members yet" hint="Add people to give them this group's access." />}>
                    <DataTable columns={["Member", "Email", "Other groups", { label: "Actions", srOnly: true }]} footer={<TableCount count={visibleMembers().length} noun={{ one: "member", other: "members" }} />}>
                      <For each={visibleMembers()}>
                        {member => (
                          <tr>
                            <td><PrimaryLink href={memberHref(member.membership_id)}>{memberName(member)}</PrimaryLink></td>
                            <td class="text-console-muted!">{member.email ?? <EmptyCell />}</td>
                            <td><Show when={otherGroups(member)} fallback={<EmptyCell />}>{text => text()}</Show></td>
                            <RowActions label={`Actions for ${memberName(member)}`} items={isAll() || !hasPermission(me.data, "groups.remove_member") ? [] : [{ label: "Remove from group", tone: "danger", onSelect: () => setRemoving(member) }]} />
                          </tr>
                        )}
                      </For>
                    </DataTable>
                  </Show>
                )}
              </QueryState>
            </Show></TabPanel>
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
