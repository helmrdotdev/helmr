import { useParams } from "@solidjs/router";
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
import { getMemberPermissions, groupHref, listGrants, listGroups, listRoles, permissionName, setGroupMember, type MemberPermissions, type MemberPermissionSource, type PermissionGroup } from "../lib/permissions";
import { DataTable, EmptyCell, TableCount, PrimaryLink, RowActions } from "../ui/DataTable";
import { ConfirmDialog } from "../ui/Dialog";
import { PageHeader } from "../ui/PageHeader";
import { EmptyState, NotFoundState, QueryState, DeniedState } from "../ui/QueryState";
import { TableSkeleton } from "../ui/Skeleton";
import { ui } from "../ui/styles";
import { TabPanel, Tabs } from "../ui/Tabs";

type Tab = "effective" | "assignments" | "groups";
const EFFECTIVE_COLUMNS = ["Environment", "Agent", "Permissions", "Granted through"] as const;

type Source = { kind: MemberPermissionSource["subject_kind"]; label: string };
const sourceOf = (source: MemberPermissionSource): Source => ({
  kind: source.subject_kind,
  label: source.subject_kind === "member" ? "Direct" : source.subject_kind === "all_members" ? "All members" : source.group_name || "Group",
});

// Where each permission comes from, one row per scope.
function EffectiveAccessTable(props: { access: MemberPermissions; names: (id: string) => string; empty: string }) {
  const scopeState = useScope();
  const projects = () => new Set(props.access.scopes.filter(s => s.type !== "organization").map(s => s.project));
  // Scopes name projects and environments by ID; show their names where the tab knows them.
  const environmentName = (project: string, environment: string) => [scopeState.selectedProject(), ...scopeState.projects()].find(p => p?.project === project && p.environments)?.environments?.find(e => e.environment === environment)?.name ?? environment;
  const projectName = (project: string) => scopeState.projects().find(p => p.project === project)?.name ?? project;
  const where = (scope: MemberPermissions["scopes"][number]) => scope.type === "organization" ? "Organization" : projects().size > 1 ? `${projectName(scope.project)} / ${environmentName(scope.project, scope.environment)}` : environmentName(scope.project, scope.environment);
  const sources = (scope: MemberPermissions["scopes"][number]) => {
    const seen = new Map<string, Source>();
    for (const source of scope.sources) { const s = sourceOf(source); seen.set(`${s.kind}:${s.label}`, s); }
    return [...seen.values()];
  };
  return (
    <Show when={props.access.scopes.length > 0} fallback={<EmptyState title={props.empty} />}>
      <DataTable columns={EFFECTIVE_COLUMNS} minWidth="min-w-180" footer={<TableCount count={props.access.scopes.length} noun={{ one: "scope", other: "scopes" }} />}>
        <For each={props.access.scopes}>
          {scope => (
            <tr>
              <td class={scope.type === "organization" ? "text-console-muted!" : "font-mono text-chrome!"}>{where(scope)}</td>
              <td><Show when={scope.agent} fallback={<EmptyCell />}>{agent => <span class="font-mono text-chrome">{agent()}</span>}</Show></td>
              <td>{scope.permissions.map(props.names).join(" · ")}</td>
              <td><span class="flex items-center gap-3.5"><For each={sources(scope)}>{source => <AccessLabel kind={source.kind}>{source.label}</AccessLabel>}</For></span></td>
            </tr>
          )}
        </For>
      </DataTable>
    </Show>
  );
}

export function MemberDetail() {
  const params = useParams(); const me = useMe(); const cache = useQueryClient();
  const id = () => params["member"] ?? "";
  const canManage = () => hasPermission(me.data, "access.manage");
  const self = () => me.data?.membership_id === id();
  const members = createQuery(() => ({ queryKey: ["members"], queryFn: listMembers, enabled: canManage() }));
  const access = createQuery(() => ({ queryKey: ["access", "member", id(), "permissions"], queryFn: () => getMemberPermissions(id()), enabled: !!id() && (canManage() || self()) }));
  const roles = createQuery(() => ({ queryKey: ["access", "roles"], queryFn: listRoles, enabled: canManage() }));
  const grants = createQuery(() => ({ queryKey: ["access", "grants"], queryFn: listGrants, enabled: canManage() }));
  const groups = createQuery(() => ({ queryKey: ["access", "groups"], queryFn: listGroups, enabled: canManage() }));
  const member = () => members.data?.members.find(m => m.membership_id === id());
  const name = () => member()?.display_name || member()?.email || (self() ? me.data?.display_name : "") || "";
  const missing = () => canManage() && !!members.data && !member();
  const [tab, setTab] = createSignal<Tab>("effective");
  const [granting, setGranting] = createSignal(false);
  const [leaving, setLeaving] = createSignal<PermissionGroup>();
  const direct = createMemo(() => grants.data?.grants.filter(g => g.subject_kind === "member" && g.member_id === id()) ?? []);
  const memberGroups = createMemo(() => groups.data?.groups.filter(g => g.member_ids.includes(id())) ?? []);
  const names = (permission: string) => permissionName(permission, roles.data?.catalog ?? []);
  const tabs = () => [
    { value: "effective" as const, label: "Effective access", count: access.data?.scopes.length },
    ...(canManage() ? [{ value: "assignments" as const, label: "Assignments", count: direct().length }, { value: "groups" as const, label: "Groups", count: memberGroups().length }] : []),
  ];

  return <>
    <PageHeader
      title={missing() ? "Member not found" : name()}
      breadcrumb={canManage() ? [{ href: "/settings/members", label: "Members" }] : [{ href: "/settings/account", label: "My account" }]}
      badge={<Show when={member()?.is_owner || (self() && me.data?.is_owner)}><Tag title="Can appoint owners and recover access administration">owner</Tag></Show>}
      subtitle={member()?.email && member()?.email !== name() ? member()?.email : undefined}
      actions={<Show when={canManage() && member()?.status === "active"}><button type="button" class={ui.button} onClick={() => setGranting(true)}>Grant access</button></Show>}
    />
    <Show when={canManage() || self()} fallback={<Show when={!me.isPending}><DeniedState message="You need Manage access permission to see other members’ access." /></Show>}>
      <Show when={!missing()} fallback={<NotFoundState noun="member" id={id()} back={{ href: "/settings/members", label: "Go to members" }} />}>
        <Tabs id="member-tabs" label="Member" items={tabs()} selected={tab()} onSelect={setTab} />
        <TabPanel tabs="member-tabs" value="effective" selected={tab()}>
          <QueryState query={access} pending={<TableSkeleton label="Loading access" columns={EFFECTIVE_COLUMNS} />} error={error => loadError(error, "Could not load access.")}>
            {data => <EffectiveAccessTable access={data()} names={names} empty="No access yet" />}
          </QueryState>
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
                        <RowActions label={`Actions for ${group.name}`} items={[{ label: "Remove from group", tone: "danger", onSelect: () => setLeaving(group) }]} />
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
