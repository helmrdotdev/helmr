import { usePermission } from "../../lib/me";
import { createQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { errorDetails, loadError } from "../../lib/error";
import { listMembers } from "../../lib/members";
import {
  grantCoversAgent, listAccessTargets, listGrants, listGroups, listRoles, memberHref, permissionName, permissionHasScope,
  type RoleGrant,
} from "../../lib/permissions";
import { useScopeIDs } from "../../lib/scope";
import { DataTable, EmptyCell, TableCount, PrimaryLink } from "../../ui/DataTable";
import { Segmented } from "../../ui/Field";
import { Toolbar } from "../../ui/Page";
import { EmptyState, ErrorState } from "../../ui/QueryState";
import { TableSkeleton } from "../../ui/Skeleton";
import { ui } from "../../ui/styles";
import { AccessLabel, type AccessKind } from "./AccessIcon";
import { AssignmentsTable } from "./AssignmentsTable";
import { GrantAccessDialog } from "./GrantAccessDialog";

type View = "people" | "groups" | "api_keys";
type Person = { id: string; name: string; email: string; permissions: Set<string>; sources: Map<string, { kind: AccessKind; label: string }> };
const PEOPLE_COLUMNS = ["Person", "Email", "Can", "Granted through"] as const;

// Who can use and manage one agent: everyone it is shared with, the people,
// groups and API keys it is granted to, and where each person's access comes from.
export function AgentAccess(props: { agent: string }) {
  const permission = usePermission();
  const scope = useScopeIDs();
  const targets = createQuery(() => ({ queryKey: ["access", "targets", scope().projectID], queryFn: () => listAccessTargets(scope().projectID), enabled: !!scope().projectID }));
  const grants = createQuery(() => ({ queryKey: ["access", "grants"], queryFn: listGrants }));
  const groups = createQuery(() => ({ queryKey: ["access", "groups"], queryFn: listGroups, enabled: permission.has("groups.read") }));
  const roles = createQuery(() => ({ queryKey: ["access", "roles"], queryFn: listRoles, enabled: permission.has("roles.read") }));
  const members = createQuery(() => ({ queryKey: ["members"], queryFn: listMembers, enabled: permission.has("members.read") }));
  const environment = () => targets.data?.targets.find(t => t.environment === scope().environmentID);
  const agent = () => environment()?.agents.find(a => a.key === props.agent);
  const catalog = () => roles.data?.catalog ?? [];
  const covering = createMemo(() => {
    const env = environment(); const target = agent();
    return env && target ? (grants.data?.grants ?? []).filter(g => grantCoversAgent(g, env.environment_id, target.id)) : [];
  });
  const [view, setView] = createSignal<View>("people");
  const [granting, setGranting] = createSignal(false);
  // Each person with access, from direct assignments, their groups and all members.
  const people = createMemo(() => {
    const active = (members.data?.members ?? []).filter(m => m.status === "active");
    const byID = new Map<string, Person>();
    const add = (memberID: string, grant: RoleGrant, source: { kind: AccessKind; label: string }) => {
      const member = active.find(m => m.membership_id === memberID);
      if (!member) return;
      const person = byID.get(memberID) ?? { id: memberID, name: member.display_name || member.email || memberID, email: member.email ?? "", permissions: new Set<string>(), sources: new Map() };
      grant.permissions.filter(p => permissionHasScope(p, "agent", catalog())).forEach(p => person.permissions.add(p));
      person.sources.set(`${source.kind}:${source.label}`, source);
      byID.set(memberID, person);
    };
    for (const grant of covering()) {
      if (grant.subject_kind === "member" && grant.member_id) add(grant.member_id, grant, { kind: "member", label: "Direct" });
      else if (grant.subject_kind === "all_members") active.forEach(m => add(m.membership_id, grant, { kind: "all_members", label: "All members" }));
      else if (grant.subject_kind === "group") {
        const group = groups.data?.groups.find(g => g.id === grant.group_id);
        group?.member_ids.forEach(id => add(id, grant, { kind: "group", label: group.name }));
      }
    }
    return [...byID.values()].sort((a, b) => a.name.localeCompare(b.name));
  });
  const groupGrants = () => covering().filter(g => g.subject_kind === "group" || g.subject_kind === "all_members");
  const keyGrants = () => covering().filter(g => g.subject_kind === "api_key");
  const loading = () => targets.isPending || grants.isPending || groups.isLoading || roles.isLoading || members.isLoading;
  const failure = () => targets.error || grants.error || groups.error || roles.error || members.error;
  const retry = () => { for (const query of [targets, grants, groups, roles, members]) if (query.isError) void query.refetch(); };

  return <>
    <Show when={!failure()} fallback={<ErrorState message={loadError(failure(), "Could not load access.")} details={errorDetails(failure())} onRetry={retry} />}>
    <Show when={!loading()} fallback={<TableSkeleton label="Loading access" columns={PEOPLE_COLUMNS} />}>
      <Show when={agent()} fallback={<EmptyState title="This agent isn't registered in this environment." />}>
        <Toolbar
          start={<Segmented label="Who has access" value={view()} options={[{ value: "people" as const, label: "People", count: people().length }, { value: "groups" as const, label: "Groups", count: groupGrants().length }, { value: "api_keys" as const, label: "API keys", count: keyGrants().length }]} onChange={setView} />}
          end={<button type="button" class={ui.button} disabled={!permission.has("assignments.create")} onClick={() => setGranting(true)}>Grant access</button>}
        />
        <Show when={view() === "people"}>
          <Show when={people().length > 0} fallback={<EmptyState title="No access assignments for this agent" hint="Grant a role to a person, group, All members or API key." />}>
            <DataTable columns={PEOPLE_COLUMNS} minWidth="min-w-180" footer={<TableCount count={people().length} noun={{ one: "person", other: "people" }} />}>
              <For each={people()}>
                {person => (
                  <tr>
                    <td><PrimaryLink href={memberHref(person.id)}>{person.name}</PrimaryLink></td>
                    <td class="text-console-muted!">{person.email || <EmptyCell />}</td>
                    <td>{[...person.permissions].map(p => permissionName(p, catalog())).join(" · ") || <EmptyCell />}</td>
                    <td><span class="flex items-center gap-3.5"><For each={[...person.sources.values()]}>{source => <AccessLabel kind={source.kind}>{source.label}</AccessLabel>}</For></span></td>
                  </tr>
                )}
              </For>
            </DataTable>
          </Show>
        </Show>
        <Show when={view() === "groups"}>
          <AssignmentsTable grants={groupGrants()} catalog={catalog()} recipient empty="No groups have this agent" emptyHint="Grant a role to a group to give its members access." />
        </Show>
        <Show when={view() === "api_keys"}>
          <AssignmentsTable grants={keyGrants()} catalog={catalog()} recipient empty="No API keys have this agent" />
        </Show>
      </Show>
    </Show>
    </Show>
    <Show when={granting() && environment() && agent()}>
      <GrantAccessDialog environment={{ project: scope().projectID, environment: environment()!.environment }} agentID={agent()!.id} onClose={() => setGranting(false)} />
    </Show>
  </>;
}
