import { createQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { errorMessage } from "../../lib/error";
import { listMembers } from "../../lib/members";
import { getMemberPermissions, listGroups, listGrants, targetSummary, type RoleGrant } from "../../lib/permissions";
import { Field } from "../../ui/Field";
import { Notice } from "../../ui/Notice";
import { Select } from "../../ui/Select";

// Preview remaining sources using the server's current effective projection.
export function GrantRemovalImpact(props: { grant: RoleGrant }) {
  return <Show when={props.grant.subject_kind === "api_key"} fallback={<MemberGrantRemovalImpact grant={props.grant} />}><APIKeyGrantRemovalImpact grant={props.grant} /></Show>;
}

function APIKeyGrantRemovalImpact(props: { grant: RoleGrant }) {
  const grants = createQuery(() => ({ queryKey: ["access", "grants"], queryFn: listGrants }));
  const remaining = () => grants.data?.grants.filter(g => g.api_key_id === props.grant.api_key_id && g.id !== props.grant.id) ?? [];
  return <div class="mt-4 space-y-2 border-t border-console-border pt-3">
    <Show when={grants.error}><Notice tone="danger">{errorMessage(grants.error)}</Notice></Show>
    <Show when={grants.data}><p>Remaining Role assignments for this API key:</p><For each={remaining()} fallback={<p>None. This key will have no workload access.</p>}>{grant => <p>{grant.role_name}: {targetSummary(grant)}</p>}</For></Show>
    <p class="text-control text-console-muted">API keys do not inherit member or Group assignments. Permissions are checked again when an operation runs.</p>
  </div>;
}

function MemberGrantRemovalImpact(props: { grant: RoleGrant }) {
  const groups = createQuery(() => ({ queryKey: ["access", "groups"], queryFn: listGroups }));
  const members = createQuery(() => ({ queryKey: ["members"], queryFn: listMembers }));
  const affected = createMemo(() => members.data?.members.filter(m => m.status === "active" && (props.grant.subject_kind === "all_members" || (props.grant.subject_kind === "member" && props.grant.member_id === m.membership_id) || groups.data?.groups.find(g => g.id === props.grant.group_id)?.member_ids.includes(m.membership_id))) ?? []);
  const [selected, setSelected] = createSignal("");
  const user = () => selected() || affected()[0]?.membership_id || "";
  const effective = createQuery(() => ({ queryKey: ["access", "member", user(), "permissions"], queryFn: () => getMemberPermissions(user()), enabled: !!user() }));
  const scopes = createMemo(() => effective.data?.scopes.filter(s => s.sources.some(source => source.grant_id === props.grant.id)).map(s => {
    const sources = s.sources.filter(source => source.grant_id !== props.grant.id);
    return { ...s, sources, permissions: [...new Set(sources.flatMap(source => source.permissions))].sort() };
  }) ?? []);
  const error = () => groups.error || members.error || effective.error;
  return <div class="mt-4 space-y-3 border-t border-console-border pt-3">
    <Show when={error()}><Notice tone="danger">{errorMessage(error())}</Notice></Show>
    <Show when={groups.isPending || members.isPending}><p class="text-control">Loading affected members…</p></Show>
    <Show when={affected().length} fallback={<Show when={!groups.isPending && !members.isPending && !error()}><p class="text-control">No active members currently receive this assignment.</p></Show>}>
      <Field label="Preview remaining access for"><Select value={user()} options={affected().map(m => ({ value: m.membership_id, label: m.display_name || m.email || m.membership_id }))} onChange={setSelected} /></Field>
      <Show when={effective.isPending}><p class="text-control">Loading access…</p></Show>
      <For each={scopes()}>{scope => <div class="text-control">
        <p class="font-medium">{scope.project} / {scope.environment} / {scope.agent || "Entire environment"}</p>
        <p>Remaining permissions: {scope.permissions.join(" · ") || "None"}</p>
        <For each={scope.sources}>{source => <p class="text-console-muted">{source.subject_kind === "member" ? `Direct assignment → ${source.role_name}` : source.subject_kind === "all_members" ? `All members → ${source.role_name}` : `${source.group_name} → ${source.role_name} (${source.agent || "Entire environment"})`}</p>}</For>
      </div>}</For>
    </Show>
    <p class="text-chrome text-console-muted">Preview of current sources after removing this assignment. Permissions are checked again when an operation runs.</p>
  </div>;
}

export function GroupMemberRemovalImpact(props: { memberID: string; groupID: string }) {
 const effective = createQuery(() => ({queryKey:["access","member",props.memberID,"permissions"],queryFn:()=>getMemberPermissions(props.memberID)}));
 const affected = () => effective.data?.scopes.filter(scope=>scope.sources.some(source=>source.group_id===props.groupID)) ?? [];
 return <div class="mt-3 space-y-3">
  <Show when={effective.isPending}><p>Loading remaining access…</p></Show>
  <Show when={effective.error}><Notice tone="danger">{errorMessage(effective.error)}</Notice></Show>
  <For each={affected()}>{scope=>{const remaining=()=>scope.sources.filter(source=>source.group_id!==props.groupID);return <div class="text-control"><p class="font-medium">{scope.project} / {scope.environment} / {scope.agent || "Entire environment"}</p><p>Remaining permissions: {[...new Set(remaining().flatMap(source=>source.permissions))].join(" · ") || "None"}</p><For each={remaining()}>{source=><p>{source.subject_kind === "member" ? "Direct assignment" : source.subject_kind === "all_members" ? "All members" : source.group_name} → {source.role_name}</p>}</For></div>;}}</For>
  <Show when={!effective.isPending && !effective.error && affected().length===0}><p>This Group currently supplies no permissions to this member.</p></Show>
  <p class="text-control text-console-muted">Other direct, Group and All members assignments remain. Effects already admitted by running workloads are not stopped.</p>
 </div>;
}
