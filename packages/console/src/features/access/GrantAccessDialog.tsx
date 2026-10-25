import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { listAllApiKeys } from "../../lib/api-keys";
import { errorMessage } from "../../lib/error";
import { listMembers } from "../../lib/members";
import {
  assignRole, grantRecipientName, isOrganizationRole, listAccessTargets, listGrants, listGroups, listRoles, permissionName, permissionResources,
  type Capability, type PermissionRole, type RoleGrant,
} from "../../lib/permissions";
import { useScope } from "../../lib/scope";
import { FormDialog } from "../../ui/Dialog";
import { Field, Segmented } from "../../ui/Field";
import { Notice } from "../../ui/Notice";
import { Select } from "../../ui/Select";
import { ui } from "../../ui/styles";
import { AccessLabel, type AccessKind } from "./AccessIcon";

export type GrantRecipient = { kind: AccessKind; id: string; name: string };

const KINDS: { value: AccessKind; label: string }[] = [
  { value: "member", label: "Person" },
  { value: "group", label: "Group" },
  { value: "api_key", label: "API key" },
  { value: "all_members", label: "All members" },
];

const lowerFirst = (text: string) => text.charAt(0).toLowerCase() + text.slice(1);
const joinAnd = (parts: string[]) => parts.length < 2 ? parts.join("") : `${parts.slice(0, -1).join(", ")} and ${parts[parts.length - 1]}`;

// Whether a role may be granted here: API keys take only workload roles, all
// members only agent use on selected agents, an agent's page only roles with
// agent permissions, and a fixed environment no organization role.
function eligible(role: PermissionRole, kind: AccessKind, catalog: readonly Capability[], place: { agent: boolean; environment: boolean }): boolean {
  if (kind === "api_key" && !role.api_key_assignable) return false;
  if (kind === "all_members" && !(role.permissions.length === 1 && role.permissions[0] === "agents.use")) return false;
  if (place.agent && !permissionResources(role.permissions, catalog).includes("agents")) return false;
  return !((place.agent || place.environment) && isOrganizationRole(role.permissions, catalog));
}

// The one place access is granted or its targets edited: who, which role, where,
// and which agents, with the result in one sentence. Every page opens it with
// what it already knows (a fixed recipient, environment or agent); editing an
// assignment changes only its agent targets.
export function GrantAccessDialog(props: {
  recipient?: GrantRecipient;
  roleID?: string | undefined;
  environment?: { project: string; environment: string };
  agentID?: string;
  grant?: RoleGrant;
  onClose: () => void;
  onSaved?: () => Promise<void> | void;
}) {
  const scope = useScope();
  const cache = useQueryClient();
  const existing = props.grant;
  const roles = createQuery(() => ({ queryKey: ["access", "roles"], queryFn: listRoles }));
  const groups = createQuery(() => ({ queryKey: ["access", "groups"], queryFn: listGroups }));
  const members = createQuery(() => ({ queryKey: ["members"], queryFn: listMembers }));
  const grants = createQuery(() => ({ queryKey: ["access", "grants"], queryFn: listGrants, enabled: !props.grant }));

  const [kind, setKind] = createSignal<AccessKind>(existing?.subject_kind ?? props.recipient?.kind ?? "member");
  const [recipient, setRecipient] = createSignal(existing ? existing.api_key_id ?? existing.member_id ?? existing.group_id ?? "" : props.recipient?.id ?? "");
  const [roleID, setRoleID] = createSignal(existing?.role_id ?? props.roleID ?? "");
  const [project, setProject] = createSignal(existing?.project ?? props.environment?.project ?? scope.selectedProjectID());
  const [environmentSlug, setEnvironmentSlug] = createSignal(existing ? "" : props.environment?.environment ?? scope.selectedEnvironmentID());
  const [mode, setMode] = createSignal<"selected" | "all">(existing?.agents_mode === "all" ? "all" : "selected");
  const [agents, setAgents] = createSignal<string[]>(existing?.agent_ids ?? (props.agentID ? [props.agentID] : []));
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const fixedRecipient = () => !!existing || !!props.recipient;
  const fixedWhere = () => !!existing || !!props.environment;
  const catalog = (): Capability[] => roles.data?.catalog ?? [];
  const targets = createQuery(() => ({ queryKey: ["access", "targets", project()], queryFn: () => listAccessTargets(project()), enabled: !!project() }));
  const target = () => targets.data?.targets.find(t => existing ? t.environment_id === existing.environment_id : t.environment === environmentSlug());
  const keys = createQuery(() => ({
    queryKey: ["access", "api-key-subjects", project(), target()?.environment],
    queryFn: () => listAllApiKeys(project(), target()!.environment),
    enabled: kind() === "api_key" && !fixedRecipient() && !!target(),
  }));

  // The organization's own roles first, then the ones Helmr maintains.
  const options = createMemo(() => {
    const all = (roles.data?.roles ?? []).filter(r => existing ? r.id === existing.role_id : eligible(r, kind(), catalog(), { agent: !!props.agentID, environment: !!props.environment }));
    return [...all.filter(r => !r.builtin_key), ...all.filter(r => r.builtin_key)];
  });
  const hidden = () => existing ? 0 : (roles.data?.roles.length ?? 0) - options().length;
  const role = () => options().find(r => r.id === roleID());
  const organizationRole = () => !!role() && isOrganizationRole(role()!.permissions, catalog());
  const resources = () => permissionResources(role()?.permissions ?? [], catalog());
  const hasAgents = () => resources().includes("agents");
  const allMembers = () => kind() === "all_members";
  const agentMode = () => allMembers() ? "selected" : mode();

  const activeMembers = () => (members.data?.members ?? []).filter(m => m.status === "active");
  const recipientName = () => {
    if (allMembers()) return "All members";
    if (props.recipient) return props.recipient.name;
    if (existing) return grantRecipientName(existing);
    if (kind() === "member") return activeMembers().find(m => m.membership_id === recipient())?.display_name ?? "";
    if (kind() === "group") return groups.data?.groups.find(g => g.id === recipient())?.name ?? "";
    return keys.data?.find(k => k.id === recipient())?.name ?? "";
  };
  const recipientOptions = () => kind() === "member"
    ? activeMembers().map(m => ({ value: m.membership_id, label: m.display_name || m.email || m.membership_id }))
    : kind() === "group" ? (groups.data?.groups ?? []).map(g => ({ value: g.id, label: g.name }))
    : (keys.data ?? []).map(k => ({ value: k.id, label: k.name }));
  const reach = () => {
    if (allMembers()) return `${activeMembers().length} members now, and everyone who joins later.`;
    if (kind() === "group") { const ids = groups.data?.groups.find(g => g.id === recipient())?.member_ids; const count = ids?.filter(id => activeMembers().some(m => m.membership_id === id)).length; return count === undefined ? undefined : `${count} ${count === 1 ? "member gets" : "members get"} this access.`; }
    if (kind() === "api_key") return "API keys get only the roles granted to them, in their own environment.";
    return undefined;
  };

  // The recipient's assignment of this role in this place, if any: the server
  // keeps one per recipient, role and environment, so a new grant adds to it.
  const current = createMemo(() => {
    const chosen = role();
    if (existing || !chosen || (allMembers() ? false : !recipient())) return undefined;
    return grants.data?.grants.find(g => g.role_id === chosen.id && g.subject_kind === kind()
      && (allMembers() || (g.member_id ?? g.group_id ?? g.api_key_id) === recipient())
      && (organizationRole() ? g.target_kind === "organization" : g.environment_id === target()?.environment_id));
  });
  const held = () => current()?.agent_ids ?? [];
  // Nothing to add: the role has no agent targets, or already covers every agent.
  const alreadyGranted = () => !!current() && (!hasAgents() || current()!.agents_mode === "all");
  const heldNote = () => {
    const grant = current();
    if (!grant) return "";
    const name = recipientName();
    if (alreadyGranted()) return grant.target_kind === "organization" ? `${name} already has this role.` : `${name} already has this role in ${grant.environment_name}${grant.agents_mode === "all" ? " on every agent" : ""}.`;
    return grant.agent_keys.length
      ? `${name} already has this role on ${grant.agent_keys.join(", ")}. Granting adds the agents you select.`
      : `${name} already has this role in ${grant.environment_name} with no agents selected. Granting adds the agents you select.`;
  };
  const chosenAgents = () => (target()?.agents ?? []).filter(a => agents().includes(a.id) || held().includes(a.id)).map(a => a.key);
  const actions = (resource: string) => joinAnd((role()?.permissions ?? []).filter(p => catalog().find(c => c.id === p)?.resource_type === resource).map(p => lowerFirst(permissionName(p, catalog()))));
  const summary = () => {
    const name = recipientName();
    if (!role() || !name) return "";
    if (organizationRole()) return `${name} can ${actions("organization")} across the organization.`;
    const where = target()?.name ?? "this environment";
    const parts = resources().map(resource => resource === "agents"
      ? `${actions(resource)} ${agentMode() === "all" ? `on every agent in ${where}, including new ones` : chosenAgents().length ? `on ${chosenAgents().join(", ")} in ${where}` : "on the agents you select"}`
      : `${actions(resource)} across ${where}`);
    return `${name} can ${joinAnd(parts)}.`;
  };
  const readsSessions = () => role()?.permissions.some(p => p === "sessions.view" || p === "sessions.manage");
  // Waits for current assignments, so a grant never misses one it should add to.
  const valid = () => (!!existing || !!grants.data) && !!role() && !alreadyGranted() && (allMembers() || !!recipient()) && (organizationRole() || !!target()) && (!hasAgents() || agentMode() === "all" || agents().some(id => !held().includes(id)) || (!current() && resources().some(r => r !== "agents")));

  const chooseKind = (value: AccessKind) => { setKind(value); setRecipient(""); setMode("selected"); };
  const chooseEnvironment = (slug: string) => { setEnvironmentSlug(slug); setAgents([]); if (kind() === "api_key") setRecipient(""); };
  const chooseProject = (value: string) => { setProject(value); setEnvironmentSlug(""); setAgents([]); if (kind() === "api_key") setRecipient(""); };

  const submit = async () => {
    const chosen = role();
    if (!valid() || !chosen) return;
    setBusy(true); setError(null);
    try {
      await assignRole(project(), {
        subject_kind: kind(),
        member_id: kind() === "member" ? recipient() : undefined,
        api_key_id: kind() === "api_key" ? recipient() : undefined,
        group_id: kind() === "group" ? recipient() : null,
        role_id: chosen.id,
        environment_id: target()?.environment_id ?? "",
        target_kind: organizationRole() ? "organization" : "environment",
        agents_mode: hasAgents() ? agentMode() : "none",
        agent_ids: hasAgents() && agentMode() === "selected" ? [...new Set([...held(), ...agents()])] : [],
        expected_role_revision: chosen.revision,
      }, existing ?? current());
      await Promise.all([cache.invalidateQueries({ queryKey: ["access"] }), cache.invalidateQueries({ queryKey: ["me"] }), cache.invalidateQueries({ queryKey: ["api-keys"] })]);
      await props.onSaved?.();
      props.onClose();
    } catch (cause) {
      setError(`${errorMessage(cause)} Review the current access and try again.`);
      await cache.invalidateQueries({ queryKey: ["access"] });
    } finally {
      setBusy(false);
    }
  };

  const loadError = () => roles.error || groups.error || members.error || grants.error || targets.error || keys.error;

  return (
    <FormDialog
      title={existing ? "Edit access" : "Grant access"}
      submitLabel={existing ? "Save" : "Grant access"}
      busyLabel="Saving…"
      busy={busy()}
      disabled={!valid()}
      error={error() ?? (loadError() ? errorMessage(loadError()) : null)}
      onClose={props.onClose}
      onSubmit={() => void submit()}
    >
      <Show when={roles.data} fallback={<p class={ui.muted} role="status">Loading roles…</p>}>
        <Show when={fixedRecipient()} fallback={
          <Field label="Who" hint={reach()}>
            <div class="grid justify-items-start gap-2">
              <Segmented label="Recipient type" value={kind()} options={KINDS} onChange={chooseKind} disabled={busy()} />
              <Show when={!allMembers()}>
                <Select
                  value={recipient()}
                  options={recipientOptions()}
                  onChange={setRecipient}
                  placeholder={kind() === "api_key" && !target() ? "Choose an environment first" : kind() === "member" ? "Choose a person" : kind() === "group" ? "Choose a group" : "Choose an API key"}
                  disabled={busy() || (kind() === "api_key" && !target())}
                  minWidth="100%"
                />
              </Show>
            </div>
          </Field>
        }>
          <Field label="Who" hint={reach()}>
            <span class="text-body"><AccessLabel kind={kind()}>{recipientName()}</AccessLabel></span>
          </Field>
        </Show>

        <Show when={!existing} fallback={<Field label="Role"><span class="text-body">{role()?.name}</span></Field>}>
          <Field label="Role" hint={role() ? role()!.permissions.map(p => permissionName(p, catalog())).join(" · ") : hidden() > 0 ? `${hidden()} ${hidden() === 1 ? "role can’t" : "roles can’t"} be granted ${allMembers() ? "to all members" : kind() === "api_key" ? "to API keys" : props.agentID ? "for a single agent" : "in a single environment"}.` : undefined}>
            <Select
              value={role()?.id ?? ""}
              options={options().map(r => ({ value: r.id, label: r.name }))}
              onChange={value => { setRoleID(value); setMode("selected"); setAgents(props.agentID ? [props.agentID] : []); }}
              placeholder="Choose a role"
              disabled={busy()}
              minWidth="100%"
            />
          </Field>
        </Show>

        <Show when={!organizationRole()}>
          <div class="grid grid-cols-2 gap-3 max-sm:grid-cols-1">
            <Field label="Project">
              <Select value={project()} options={scope.projects().map(p => ({ value: p.project, label: p.name }))} onChange={chooseProject} disabled={busy() || fixedWhere()} minWidth="100%" />
            </Field>
            <Field label="Environment">
              <Select
                value={target()?.environment ?? environmentSlug()}
                options={(targets.data?.targets ?? []).map(t => ({ value: t.environment, label: t.name }))}
                onChange={chooseEnvironment}
                placeholder="Choose an environment"
                disabled={busy() || fixedWhere()}
                minWidth="100%"
              />
            </Field>
          </div>
        </Show>

        <Show when={!organizationRole() && hasAgents()}>
          <fieldset class={ui.fieldSet}>
            <legend class={ui.fieldLabel}>Agents</legend>
            <div class="mt-1 grid justify-items-start gap-2">
              <Show when={!allMembers()}>
                <Segmented label="Agent targets" value={mode()} options={[{ value: "selected" as const, label: "Selected" }, { value: "all" as const, label: "All, including new ones" }]} onChange={setMode} disabled={busy()} />
              </Show>
              <Show when={agentMode() === "selected"}>
                <div class="flex max-h-40 w-full flex-wrap gap-x-5 gap-y-1.5 overflow-y-auto border border-console-border px-2.5 py-2">
                  <For each={target()?.agents ?? []} fallback={<span class={ui.fieldHint}>{target() ? "This environment has no agents." : "Choose an environment to see its agents."}</span>}>
                    {agent => (
                      <label class="flex cursor-pointer items-center gap-1.5 font-mono text-chrome text-console-text">
                        <input
                          type="checkbox"
                          class="accent-console-accent"
                          checked={agents().includes(agent.id) || held().includes(agent.id)}
                          disabled={busy() || held().includes(agent.id)}
                          onChange={event => setAgents(ids => event.currentTarget.checked ? [...ids, agent.id] : ids.filter(id => id !== agent.id))}
                        />
                        {agent.key}
                      </label>
                    )}
                  </For>
                </div>
              </Show>
            </div>
          </fieldset>
        </Show>

        <Show when={summary()}>
          <p class="m-0 border border-console-border bg-console-bg-panel px-3 py-2 text-body text-console-text">{summary()}</p>
        </Show>
        <Show when={heldNote()}>
          <Notice tone="info" class="mt-3">{heldNote()}</Notice>
        </Show>
        <Show when={readsSessions()}>
          <Notice tone="warning" class="mt-3">This includes other people’s personal conversations with these agents.</Notice>
        </Show>
      </Show>
    </FormDialog>
  );
}
