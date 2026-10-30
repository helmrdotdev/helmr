import { A, useNavigate, useParams } from "@solidjs/router";
import { memberMenu, RemoveMemberDialog } from "./members";
import { Tag } from "../ui/Tag";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { AccessLabel } from "../features/access/AccessIcon";
import { AssignmentsTable } from "../features/access/AssignmentsTable";
import { GrantAccessDialog } from "../features/access/GrantAccessDialog";
import { GroupMemberRemovalImpact } from "../features/access/GrantRemovalImpact";
import { hasPermission } from "../lib/auth";
import { loadError } from "../lib/error";
import { useMe } from "../lib/me";
import { listMembers } from "../lib/members";
import { useScope } from "../lib/scope";
import { getMemberPermissions, groupHref, listGrants, listGroups, listRoles, permissionName, resourceLabel, resourceRank, roleHref, setGroupMember, type MemberPermissions, type MemberPermissionSource, type PermissionDefinition, type PermissionGroup } from "../lib/permissions";
import { Icon } from "../ui/Icon";
import { cx, ui } from "../ui/styles";
import { DataTable, EmptyCell, TableCount, PrimaryLink, RowActions } from "../ui/DataTable";
import { ConfirmDialog } from "../ui/Dialog";
import { PageHeader } from "../ui/PageHeader";
import { EmptyState, NotFoundState, QueryState, DeniedState } from "../ui/QueryState";
import { TableSkeleton } from "../ui/Skeleton";
import { TabPanel, Tabs } from "../ui/Tabs";
import { useAuditItem } from "../features/audit/auditLink";

type Tab = "effective" | "assignments" | "groups";
const EFFECTIVE_COLUMNS = [{ label: "Details", srOnly: true }, "Environment", "Agent", "Granted through"] as const;

type Source = { kind: MemberPermissionSource["subject_kind"]; subject: string; groupID: string | undefined; role: string | undefined };
// A source names who received the role and the role itself, so a row says how access arrived.
const sourceOf = (source: MemberPermissionSource): Source => ({
  kind: source.subject_kind,
  subject: source.subject_kind === "member" ? "Direct" : source.subject_kind === "all_members" ? "All members" : source.group_name || "Group",
  groupID: source.subject_kind === "group" ? source.group_id : undefined,
  role: source.role_name,
});

// A source's group and role open their own pages, where the access is changed.
// Links stop the click so it does not also open or close the row.
function SourceLabel(props: { source: Source; roleID: (name: string) => string | undefined }) {
  const stop = (event: MouseEvent) => event.stopPropagation();
  return (
    <AccessLabel kind={props.source.kind}>
      <Show when={props.source.groupID} fallback={props.source.subject}>{id => <A href={groupHref(id())} class={ui.monoLink} onClick={stop}>{props.source.subject}</A>}</Show>
      <Show when={props.source.role}>
        {role => <>
          <span class="text-console-subtle"> › </span>
          <Show when={props.roleID(role())} fallback={role()}>{id => <A href={roleHref(id())} class={ui.monoLink} onClick={stop}>{role()}</A>}</Show>
        </>}
      </Show>
    </AccessLabel>
  );
}

type AreaPermission = { id: string; name: string; granted: boolean };
type Area = { label: string; permissions: AreaPermission[] };

// Every permission that applies at this scope, by area in the role editor's order
// and with the role editor's names; areas without any granted permission come apart.
function permissionAreas(scope: MemberPermissions["scopes"][number], catalog: readonly PermissionDefinition[], names: (id: string) => string) {
  const granted = new Set(scope.permissions);
  // Agent rows also carry the environment's own permissions, so applicability follows the assignment level.
  const level = scope.type === "organization" ? "organization" : "environment";
  const applicable = catalog.filter(c => c.assignment_levels.includes(level));
  const byResource = new Map<string, AreaPermission[]>();
  for (const permission of applicable) byResource.set(permission.resource_type, [...(byResource.get(permission.resource_type) ?? []), { id: permission.id, name: permission.name, granted: granted.has(permission.id) }]);
  // Without the catalog (no Read roles permission) only the granted names are known.
  const unknown = scope.permissions.filter(id => !applicable.some(c => c.id === id)).map(id => ({ id, name: names(id), granted: true }));
  const areas: Area[] = [...byResource].sort(([a], [b]) => resourceRank(a) - resourceRank(b)).map(([resource, permissions]) => ({ label: resourceLabel(resource), permissions }));
  if (unknown.length) areas.push({ label: "Permissions", permissions: unknown });
  return { withAccess: areas.filter(a => a.permissions.some(p => p.granted)), withoutAccess: areas.filter(a => !a.permissions.some(p => p.granted)) };
}

function AreaRows(props: { areas: Area[] }) {
  return (
    <For each={props.areas}>
      {area => (
        <tr class="group/area h-auto! hover:bg-transparent!">
          <th scope="row" class="h-auto! w-36 border-b border-console-border-soft px-2.5! group-last/area:border-b-0 py-1.5! text-left align-top text-body font-medium leading-5.5 text-console-text">{area.label}</th>
          <td class="h-auto! whitespace-normal! px-2.5! py-1.5! align-top">
            <ul class="m-0 flex list-none flex-wrap gap-x-1.5 gap-y-0.5 p-0">
              <For each={area.permissions}>
                {permission => (
                  <li class={cx("inline-flex items-center gap-1 rounded-xs border px-1.5 text-body leading-5", permission.granted ? "border-transparent bg-console-success-bg font-medium text-console-text" : "border-dashed border-console-border-strong text-console-muted")}>
                    <span class={permission.granted ? "text-console-success" : "text-console-subtle"} aria-hidden="true"><Icon name={permission.granted ? "check" : "cross"} /></span>
                    {permission.name}
                    <Show when={!permission.granted}><span class="sr-only"> (not granted)</span></Show>
                  </li>
                )}
              </For>
            </ul>
          </td>
        </tr>
      )}
    </For>
  );
}

// The opened row: its own component, so a data refresh keeps the "show all" choice.
function ExpandedAccess(props: { id: string; scope: MemberPermissions["scopes"][number]; catalog: readonly PermissionDefinition[]; names: (id: string) => string }) {
  const areas = createMemo(() => permissionAreas(props.scope, props.catalog, props.names));
  const [showAll, setShowAll] = createSignal(false);
  return (
    <tr id={props.id} class="hover:bg-transparent">
      <td colSpan={EFFECTIVE_COLUMNS.length} class="h-auto! whitespace-normal bg-console-accent-soft/40 py-2.5! pl-9!">
        <table class="w-full border-collapse overflow-hidden rounded-xs border border-console-border-soft bg-console-surface">
          <tbody>
            <AreaRows areas={areas().withAccess} />
            <Show when={showAll()}><AreaRows areas={areas().withoutAccess} /></Show>
          </tbody>
        </table>
        <Show when={areas().withoutAccess.length > 0}>
          <button type="button" class={cx(ui.monoLink, "mt-2 border-0 bg-transparent p-0")} onClick={() => setShowAll(!showAll())}>
            {showAll() ? "Hide areas with no access" : `Show ${areas().withoutAccess.length} ${areas().withoutAccess.length === 1 ? "area" : "areas"} with no access`}
          </button>
        </Show>
      </td>
    </tr>
  );
}

// Where access comes from, one row per scope. A row summarizes; opening it lists
// the permissions by area, so long permission sets stay readable.
function EffectiveAccessTable(props: { access: MemberPermissions; catalog: readonly PermissionDefinition[]; names: (id: string) => string; roleID: (name: string) => string | undefined; empty: string }) {
  const scopeState = useScope();
  const [open, setOpen] = createSignal<number>();
  const projects = () => new Set(props.access.scopes.filter(s => s.type !== "organization").map(s => s.project));
  // Scopes name projects and environments by ID; show their names where the tab knows them.
  const environmentName = (project: string, environment: string) => [scopeState.selectedProject(), ...scopeState.projects()].find(p => p?.project === project && p.environments)?.environments?.find(e => e.environment === environment)?.name ?? environment;
  const projectName = (project: string) => scopeState.projects().find(p => p.project === project)?.name ?? project;
  const where = (scope: MemberPermissions["scopes"][number]) => scope.type === "organization" ? "Organization" : projects().size > 1 ? `${projectName(scope.project)} / ${environmentName(scope.project, scope.environment)}` : environmentName(scope.project, scope.environment);
  const sources = (scope: MemberPermissions["scopes"][number]) => {
    const seen = new Map<string, Source>();
    for (const source of scope.sources) { const s = sourceOf(source); seen.set(`${s.kind}:${s.subject}:${s.role ?? ""}`, s); }
    return [...seen.values()];
  };
  return (
    <Show when={props.access.scopes.length > 0} fallback={<EmptyState title={props.empty} />}>
      <DataTable columns={EFFECTIVE_COLUMNS} minWidth="min-w-180" footer={<TableCount count={props.access.scopes.length} noun={{ one: "scope", other: "scopes" }} />}>
        <For each={props.access.scopes}>
          {(scope, index) => {
            const expanded = () => open() === index();
            const panel = `effective-access-${index()}`;
            return <>
              <tr class={cx("cursor-pointer", expanded() && "bg-console-accent-soft/40 [&>td]:border-b-transparent")} onClick={() => setOpen(expanded() ? undefined : index())}>
                <td class="w-6 pr-0!">
                  <button type="button" class="flex cursor-pointer items-center border-0 bg-transparent p-0 text-console-subtle hover:text-console-text" aria-expanded={expanded()} aria-controls={expanded() ? panel : undefined} aria-label={`${expanded() ? "Hide" : "Show"} permissions in ${where(scope)}`} onClick={event => { event.stopPropagation(); setOpen(expanded() ? undefined : index()); }}>
                    <Icon name={expanded() ? "chevron-down" : "chevron-right"} />
                  </button>
                </td>
                <td class={scope.type === "organization" ? "text-console-muted!" : "font-mono text-chrome!"}>{where(scope)}</td>
                <td><Show when={scope.agent} fallback={<EmptyCell />}>{agent => <span class="font-mono text-chrome">{agent()}</span>}</Show></td>
                <td><span class="flex flex-wrap items-center gap-x-3.5 gap-y-1"><For each={sources(scope)}>{source => <SourceLabel source={source} roleID={props.roleID} />}</For></span></td>
              </tr>
              <Show when={expanded()}>
                <ExpandedAccess id={panel} scope={scope} catalog={props.catalog} names={props.names} />
              </Show>
            </>;
          }}
        </For>
      </DataTable>
    </Show>
  );
}

export function MemberDetail() {
  const auditItem = useAuditItem();
  const params = useParams(); const me = useMe(); const cache = useQueryClient();
  const id = () => params["member"] ?? "";
  const canManage = () => hasPermission(me.data, "members.read");
  const self = () => me.data?.membership_id === id();
  const canReadAccess = () => hasPermission(me.data, "assignments.read") || self();
  const members = createQuery(() => ({ queryKey: ["members"], queryFn: listMembers, enabled: canManage() }));
  const access = createQuery(() => ({ queryKey: ["access", "member", id(), "permissions"], queryFn: () => getMemberPermissions(id()), enabled: !!id() && canReadAccess() }));
  const roles = createQuery(() => ({ queryKey: ["access", "roles"], queryFn: listRoles, enabled: hasPermission(me.data, "roles.read") }));
  const grants = createQuery(() => ({ queryKey: ["access", "grants"], queryFn: listGrants, enabled: hasPermission(me.data, "assignments.read") }));
  const groups = createQuery(() => ({ queryKey: ["access", "groups"], queryFn: listGroups, enabled: hasPermission(me.data, "groups.read") }));
  const member = () => members.data?.members.find(m => m.membership_id === id());
  const name = () => member()?.display_name || member()?.email || (self() ? me.data?.display_name : "") || "";
  const missing = () => canManage() && !!members.data && !member();
  const [tab, setTab] = createSignal<Tab>("effective");
  const [granting, setGranting] = createSignal(false);
  const [removing, setRemoving] = createSignal(false);
  const navigate = useNavigate();
  const [leaving, setLeaving] = createSignal<PermissionGroup>();
  const direct = createMemo(() => grants.data?.grants.filter(g => g.subject_kind === "member" && g.member_id === id()) ?? []);
  const memberGroups = createMemo(() => groups.data?.groups.filter(g => g.member_ids.includes(id())) ?? []);
  const names = (permission: string) => permissionName(permission, roles.data?.catalog ?? []);
  const tabs = () => [
    { value: "effective" as const, label: "Effective access", count: access.data?.scopes.length },
    ...(hasPermission(me.data, "assignments.read") ? [{ value: "assignments" as const, label: "Assignments", count: direct().length }] : []),
    ...(hasPermission(me.data, "groups.read") ? [{ value: "groups" as const, label: "Groups", count: memberGroups().length }] : []),
  ];

  return <>
    <PageHeader
      title={missing() ? "Member not found" : name()}
      breadcrumb={canManage() ? [{ href: "/settings/members", label: "Members" }] : [{ href: "/settings/account", label: "Profile" }]}
      badge={<Show when={member()?.is_owner || (self() && me.data?.is_owner)}><Tag title="Can appoint owners and recover access administration">owner</Tag></Show>}
      subtitle={member()?.email && member()?.email !== name() ? member()?.email : undefined}
      menu={member()?.status === "active" ? memberMenu({
        grant: hasPermission(me.data, "assignments.create") ? () => setGranting(true) : undefined,
        ownership: me.data?.is_owner === true,
        remove: !member()?.is_owner && hasPermission(me.data, "members.remove") ? () => setRemoving(true) : undefined,
      }).concat(auditItem({ actor: member()!.user_id })) : undefined}
      menuLabel="Member actions"
    />
    <Show when={canManage() || canReadAccess()} fallback={<Show when={!me.isPending}><DeniedState message="You need Read assignments permission to see other members’ access." /></Show>}>
      <Show when={!missing()} fallback={<NotFoundState noun="member" id={id()} back={{ href: "/settings/members", label: "Go to members" }} />}>
        <Tabs id="member-tabs" label="Member" items={tabs()} selected={tab()} onSelect={setTab} />
        <TabPanel tabs="member-tabs" value="effective" selected={tab()}>
          <Show when={canReadAccess()} fallback={<DeniedState message="You need Read assignments permission to view effective access." />}><QueryState query={access} pending={<TableSkeleton label="Loading access" columns={EFFECTIVE_COLUMNS} />} error={error => loadError(error, "Could not load access.")}>
            {data => <EffectiveAccessTable access={data()} catalog={roles.data?.catalog ?? []} names={names} roleID={name => roles.data?.roles.find(role => role.name === name)?.id} empty="No access yet" />}
          </QueryState></Show>
        </TabPanel>
        <TabPanel tabs="member-tabs" value="assignments" selected={tab()}>
          <QueryState query={grants} pending={<TableSkeleton label="Loading assignments" columns={["Role", "Where", "Agents", "Permissions"]} />} error={error => loadError(error, "Could not load assignments.")}>
            {() => <AssignmentsTable grants={direct()} catalog={roles.data?.catalog ?? []} empty="No direct assignments" emptyHint="Access from groups and all members shows under Effective access." />}
          </QueryState>
        </TabPanel>
        <TabPanel tabs="member-tabs" value="groups" selected={tab()}>
          <QueryState query={groups} pending={<TableSkeleton label="Loading groups" columns={["Group", "Members"]} />} error={error => loadError(error, "Could not load groups.")}>
            {() => (
              <Show when={memberGroups().length > 0} fallback={<EmptyState title="Not in any group" hint="Add this person from a group's members tab." />}>
                <DataTable columns={["Group", "Members", { label: "Actions", srOnly: true }]} footer={<TableCount count={memberGroups().length} noun={{ one: "group", other: "groups" }} />}>
                  <For each={memberGroups()}>
                    {group => (
                      <tr>
                        <td><PrimaryLink href={groupHref(group.id)}>{group.name}</PrimaryLink></td>
                        <td class="font-mono text-chrome!">{group.member_ids.length}</td>
                        <RowActions label={`Actions for ${group.name}`} items={hasPermission(me.data, "groups.remove_member") ? [{ label: "Remove from group", tone: "danger", onSelect: () => setLeaving(group) }] : []} />
                      </tr>
                    )}
                  </For>
                </DataTable>
              </Show>
            )}
          </QueryState>
        </TabPanel>
      </Show>
    </Show>
    <Show when={removing() && member()}>{m => <RemoveMemberDialog member={m()} onClose={() => setRemoving(false)} onRemoved={async () => { await cache.invalidateQueries({ queryKey: ["members"] }); navigate("/settings/members"); }} />}</Show>
    <Show when={granting()}><GrantAccessDialog recipient={{ kind: "member", id: id(), name: name() }} onClose={() => setGranting(false)} /></Show>
    <Show when={leaving()}>
      {group => (
        <ConfirmDialog title="Remove from group" confirmLabel="Remove" busyLabel="Removing…" tone="danger" target={`${name()} · ${group().name}`} onClose={() => setLeaving(undefined)}
          onConfirm={async () => { await setGroupMember(group().id, id(), false); await Promise.all([cache.invalidateQueries({ queryKey: ["access"] }), cache.invalidateQueries({ queryKey: ["me"] })]); }}>
          <GroupMemberRemovalImpact memberID={id()} groupID={group().id} />
        </ConfirmDialog>
      )}
    </Show>
  </>;
}
