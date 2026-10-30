import { useMe } from "../lib/me";
import { Icon } from "../ui/Icon";
import { hasPermission } from "../lib/auth";
import { APIKeyAccess } from "../features/access/APIKeyAccess";
import { Notice } from "../ui/Notice";
import { listMembers } from "../lib/members";
import { listRoles, listAccessTargets, resourceLabel, permissionHasScope, type PermissionDefinition, type PermissionRole } from "../lib/permissions";
import { createQuery, createInfiniteQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { Select, type SelectOption } from "../ui/Select";
import { errorMessage, loadError } from "../lib/error";
import {
  issueApiKey,
  listApiKeys,
  revokeApiKey,
  rotateApiKey,
  updateApiKey,

  type ApiKeyIssued,
  type ApiKeySummary,
  type ListFilter,
} from "../lib/api-keys";
import { environmentChip } from "../features/projects/display";
import { EnvironmentSelectorLayout, useEditedEnvironment, type EditTarget } from "../components/EnvironmentSelector";
import { DataTable, LoadMore, RowActions } from "../ui/DataTable";
import { ConfirmDialog, FormDialog, RevealDialog } from "../ui/Dialog";
import { Field, TextInput } from "../ui/Field";
import { PageHeader } from "../ui/PageHeader";
import { QueryState } from "../ui/QueryState";
import { LinesSkeleton, TableSkeleton } from "../ui/Skeleton";
import { RelativeTime } from "../ui/RelativeTime";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";
import { EnvironmentTarget } from "../ui/EnvChip";
import { useAuditItem } from "../features/audit/auditLink";

const COLUMNS = ["Name", "Key", "Created by", "Roles", "Last used", "Expires", { label: "Actions", srOnly: true }] as const;
// "All" mixes active, expired and revoked keys, so it adds their status.
const ALL_COLUMNS = ["Name", "Status", "Key", "Created by", "Roles", "Last used", "Expires", { label: "Actions", srOnly: true }] as const;

const FILTER_OPTIONS: SelectOption<ListFilter>[] = [
  { value: "active", label: "Active" },
  { value: "expired", label: "Expired" },
  { value: "revoked", label: "Revoked" },
  { value: "all", label: "All" },
];

type ExpiryValue = "never" | "30" | "90" | "365";

const EXPIRY_OPTIONS: SelectOption<ExpiryValue>[] = [
  { value: "never", label: "Never" },
  { value: "30", label: "30 days" },
  { value: "90", label: "90 days" },
  { value: "365", label: "1 year" },
];

const API_KEY_ERROR_MESSAGES: Record<string, string> = {
  forbidden: "You do not have permission to manage API keys.",
  invalid_label: "Name must be 1-64 characters and contain no control characters.",

  not_found: "This key was already revoked or removed.",
};
const INTERNAL_ERROR_MESSAGE = "Something went wrong. Please try again.";

const apiKeyErrorMessage = (error: unknown) => errorMessage(error, INTERNAL_ERROR_MESSAGE, API_KEY_ERROR_MESSAGES);

function validateLabel(value: string): string | null {
  const label = value.trim();
  if (label.length < 1 || label.length > 64 || /[\u0000-\u001F\u007F]/.test(label)) {
    return API_KEY_ERROR_MESSAGES["invalid_label"] ?? INTERNAL_ERROR_MESSAGE;
  }
  return null;
}

function ApiKeyRow(props: { metadata: boolean; creator: string; keyItem: ApiKeySummary; canUpdate: boolean; canRotate: boolean; canRevoke: boolean; canAssign: boolean; status: boolean; onAccess: (keyItem: ApiKeySummary) => void; onRevoke: (keyItem: ApiKeySummary) => void; onRotate: (keyItem: ApiKeySummary) => void; onEdit: (keyItem: ApiKeySummary) => void }) {
  const auditItem = useAuditItem();
  const lifecycleActions = () => [
    ...(props.keyItem.status === "active" && props.canUpdate ? [{label:"Edit",onSelect:()=>props.onEdit(props.keyItem)}] : []),
    ...(props.keyItem.status === "active" && props.canRotate ? [{label:"Rotate token",onSelect:()=>props.onRotate(props.keyItem)}] : []),
    ...(["active", "expired"].includes(props.keyItem.status) && props.canRevoke ? [{label:"Revoke",tone:"danger" as const,onSelect:()=>props.onRevoke(props.keyItem)}] : []),
  ];
  return <tr>
    <td><span class="font-mono text-chrome">{props.keyItem.name}</span></td>
    <Show when={props.status}><td><StatusBadge resource="api_key" status={props.keyItem.status} /></td></Show>
    <Show when={props.metadata}><td><code>{props.keyItem.key_prefix}…</code></td>
    <td><span class="text-control">{props.creator}</span></td></Show>
    <td><Show when={props.keyItem.role_names.length} fallback={<span class="inline-flex items-center gap-1.5 text-console-warning"><Icon name="warning" />No access</span>}>{props.keyItem.role_names.join(", ")}</Show></td>
    <Show when={props.metadata}><td><RelativeTime value={props.keyItem.last_used_at} fallback="—" /></td>
    <td><RelativeTime value={props.keyItem.expires_at} fallback="Never" /></td></Show>
    <RowActions label={`Actions for ${props.keyItem.name}`} items={[...(props.canAssign ? [{label:"Manage access",onSelect:()=>props.onAccess(props.keyItem)}] : []),...lifecycleActions(), ...auditItem({ actor: props.keyItem.id })]} />
  </tr>;
}

function IssueApiKeyDialog(props: {
 catalog: PermissionDefinition[];
 roles: PermissionRole[];
 canAssign: boolean;
 rolesLoading: boolean;
 rolesError: unknown;
  projectID: string;
  environmentID: string;
  projectName: string;
  environmentName: string;
  environmentColorHex: string;
  onClose: () => void;
  onIssued: (issued: ApiKeyIssued) => Promise<void>;
}) {
  const [label, setLabel] = createSignal("");
  const [expiry, setExpiry] = createSignal<ExpiryValue>("never");
  const [roleID, setRoleID] = createSignal("");
  const [mode, setMode] = createSignal<"selected"|"all">("selected");
  const [agents, setAgents] = createSignal<string[]>([]);
  const targets = createQuery(()=>({queryKey:["access","targets",props.projectID],queryFn:()=>listAccessTargets(props.projectID),enabled:props.canAssign}));
  const environment = () => targets.data?.targets.find(t=>t.environment === props.environmentID);
  const role = () => props.roles.find(r=>r.id === roleID());
  const resources = () => [...new Set((role()?.permissions ?? []).map(p=>props.catalog.find(c=>c.id===p)?.resource_type).filter((r):r is string=>!!r))];
  const hasAgents = () => (role()?.permissions ?? []).some(permission => permissionHasScope(permission, "agent", props.catalog));
  const roleReady = () => !props.rolesLoading && !props.rolesError && !!role() && !!environment() && !targets.error && (!hasAgents() || mode()==="all" || agents().length>0);
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const submit = async () => {
    setError(null);
    const validation = validateLabel(label());
    if (validation) {
      setError(validation);
      return;
    }
    if (props.canAssign && roleID() && !roleReady()) {
      setError("Choose a role and its targets after access options load.");
      return;
    }
    setSubmitting(true);
    try {
      const value = expiry();
      const result = await issueApiKey(props.projectID, props.environmentID, {
        name: label().trim(),
        expires_in_days: value === "never" ? null : Number(value),
        ...(props.canAssign && role() ? {role:{role_id:role()!.id,expected_role_revision:role()!.revision,agents_mode:hasAgents() ? mode() : "none" as const,agent_ids:hasAgents() && mode()==="selected" ? agents() : []}} : {}),
      });
      await props.onIssued(result);
    } catch (issueError) {
      setError(apiKeyErrorMessage(issueError));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <FormDialog
      title="Generate API key"
      intro="Create a machine identity for automation. Its role assignments determine what it can do."
      submitLabel="Generate key"
      busyLabel="Generating…"
      busy={submitting()}
      error={error()}
      onSubmit={() => void submit()}
      onClose={props.onClose}
    >
      <EnvironmentTarget
        label="API key target environment"
        environmentName={props.environmentName}
        environmentColorHex={props.environmentColorHex}
        projectName={props.projectName}
      />
      <Field label="Name">
        <TextInput value={label()} maxLength={64} autocomplete="off" onInput={(event) => setLabel(event.currentTarget.value)} autofocus />
      </Field>
      <Field label="Expiry">
        <Select<ExpiryValue> value={expiry()} options={EXPIRY_OPTIONS} onChange={setExpiry} minWidth="100%" />
      </Field>
      <Show when={props.canAssign} fallback={<Notice tone="info">This key will have no workload access. An access administrator must grant a role before it can be used.</Notice>}>
        <Show when={props.rolesLoading || targets.isPending}><LinesSkeleton label="Loading roles and agent targets" /></Show>
        <Show when={props.rolesError || targets.error}><Notice tone="danger">{errorMessage(props.rolesError || targets.error)}</Notice></Show>
        <Field label="Role"><Select value={roleID()} options={[{value:"",label:"No access — grant a role later"},...props.roles.filter(r=>r.api_key_assignable).map(r=>({value:r.id,label:r.name}))]} onChange={id=>{setRoleID(id);setMode("selected");setAgents([]);}} /></Field>
        <Show when={!roleID()}><Notice tone="info">This key will have no workload access until a role is assigned.</Notice></Show>
        <Show when={hasAgents()}><Field label="Agent targets"><Select value={mode()} options={[{value:"selected" as const,label:"Selected agents"},{value:"all" as const,label:"All current and future agents"}]} onChange={value=>{setMode(value);setAgents([]);}} /></Field>
          <Show when={mode()==="selected"}><fieldset class="max-h-44 space-y-2 overflow-y-auto border border-console-border p-3"><legend class="px-1 text-control">Select agents</legend><For each={environment()?.agents} fallback={<p class="text-control text-console-muted">No agents available in this environment.</p>}>{agent=><label class="flex items-center gap-2 text-control"><input type="checkbox" class="accent-console-accent" checked={agents().includes(agent.id)} onChange={e=>setAgents(ids=>e.currentTarget.checked ? [...ids,agent.id] : ids.filter(id=>id!==agent.id))}/>{agent.key}</label>}</For></fieldset></Show>
        </Show>
        <Show when={role()}><div class="space-y-1 text-control"><p class="font-medium">Access summary</p><For each={resources()}>{resource=><p>{resourceLabel(resource)}: {role()!.permissions.some(permission => props.catalog.find(c => c.id === permission)?.resource_type === resource && permissionHasScope(permission, "agent", props.catalog)) ? mode()==="all" ? "All current and future agents" : environment()?.agents.filter(a=>agents().includes(a.id)).map(a=>a.key).join(", ") || "No agents selected" : "Entire environment"} — {role()!.permissions.filter(p=>props.catalog.find(c=>c.id===p)?.resource_type===resource).map(p=>props.catalog.find(c=>c.id===p)?.name ?? p).join(" · ")}</p>}</For></div></Show>
      </Show>
      <p class="text-control text-console-muted">Everyone holding this secret acts as the same identity and shares ownership of its sessions. Role changes apply to future operations.</p>
    </FormDialog>
  );
}

function EditApiKeyDialog(props: {keyItem:ApiKeySummary;target:EditTarget;onClose:()=>void;onSaved:()=>Promise<void>}) {
 const [name,setName]=createSignal(props.keyItem.name);
 const [expiry,setExpiry]=createSignal<"keep"|ExpiryValue>("keep");
 const [busy,setBusy]=createSignal(false);
 const [error,setError]=createSignal<string|null>(null);
 const save=async()=>{
  const validation=validateLabel(name()); if(validation){setError(validation);return}
  setBusy(true);setError(null);
  try {
   const choice=expiry();
   const expires=choice==="keep"?(props.keyItem.expires_at ?? null):choice==="never"?null:new Date(Date.now()+Number(choice)*86400000).toISOString();
   await updateApiKey(props.target.projectID,props.target.environment.environment,props.keyItem.id,{name:name().trim(),expires_at:expires});
   await props.onSaved();
  }catch(e){setError(apiKeyErrorMessage(e))}finally{setBusy(false)}
 };
 return <FormDialog title="Edit API key" submitLabel="Save changes" busyLabel="Saving…" busy={busy()} error={error()} onClose={props.onClose} onSubmit={()=>void save()}>
  <EnvironmentTarget label="API key target environment" environmentName={props.target.environment.name} environmentColorHex={environmentChip(props.target.environment)?.colorHex??""} projectName={props.target.projectName}/>
  <Field label="Name"><TextInput value={name()} maxLength={64} onInput={e=>setName(e.currentTarget.value)}/></Field>
  <Field label="Expiry"><Select<"keep"|ExpiryValue> value={expiry()} options={[{value:"keep",label:"Keep current expiry"},...EXPIRY_OPTIONS]} onChange={setExpiry}/></Field>
  <p class="text-control text-console-muted">Role assignments determine this key’s access. Use Manage access to change them. Anyone who can rotate this key can act with its assigned roles.</p>
 </FormDialog>;
}

export function ApiKeys() {
 const me = useMe();
 const canAssign = () => hasPermission(me.data,"assignments.create");
 const can = (action: string) => hasPermission(me.data, `api_keys.${action}`, {projectID:edited.projectID(),environmentID:edited.environmentID()});
 const catalog = createQuery(()=>({queryKey:["access","roles"],queryFn:listRoles,enabled:hasPermission(me.data, "roles.read")}));
  const edited = useEditedEnvironment();
  const queryClient = useQueryClient();
  const [accessing,setAccessing] = createSignal<{keyItem:ApiKeySummary;target:EditTarget}>();
  const [filter, setFilter] = createSignal<ListFilter>("active");
  const [issuing, setIssuing] = createSignal<EditTarget>();
  const [issued, setIssued] = createSignal<ApiKeyIssued | null>(null);
  const [editing, setEditing] = createSignal<{keyItem:ApiKeySummary;target:EditTarget}>();
  const [rotating, setRotating] = createSignal<{keyItem:ApiKeySummary;target:EditTarget}>();
  const [revoking, setRevoking] = createSignal<{ keyItem: ApiKeySummary; target: EditTarget }>();
  const revoke = (keyItem: ApiKeySummary) => {
    const target = edited.target();
    if (target) setRevoking({ keyItem, target });
  };

  const keys = createInfiniteQuery(() => ({
    queryKey: ["api-keys", edited.projectID(), edited.environmentID(), filter(), can("read")],
    queryFn: ({ pageParam }) => listApiKeys(
      edited.projectID(),
      edited.environmentID(),
      filter(),
      pageParam || undefined,
    ),
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor,
    enabled: !!edited.projectID() && !!edited.environmentID(),
    retry: false,
  }));
  const members = createQuery(() => ({ queryKey: ["members"], queryFn: listMembers, enabled: !!keys.data && hasPermission(me.data, "members.read") }));
  const creator = (key: ApiKeySummary) => { const member = members.data?.members.find(m => m.membership_id === key.created_by_member_id); return member?.display_name || member?.email || key.created_by_member_id || "Unknown"; };
  const keyItems = createMemo(() => keys.data?.pages.flatMap((page) => page.api_keys) ?? []);
  const columns = () => can("read") ? (filter() === "all" ? ALL_COLUMNS : COLUMNS) : ["Name", ...(filter() === "all" ? ["Status"] : []), "Roles", {label: "Actions", srOnly: true}];
  // While the active list is empty its empty state offers the one action.

  const generate = () => setIssuing(edited.target());

  const invalidateApiKeys = (projectID: string, environmentID: string) =>
    queryClient.invalidateQueries({ queryKey: ["api-keys", projectID, environmentID] });

  return (
    <>
      <PageHeader
        title="API keys"
        subtitle="Machine identities with role assignments for one environment."
        actions={
          <Show when={can("create") && !(keys.isSuccess && filter() === "active" && keyItems().length === 0)}>
            <button
              class={ui.button}
              type="button"
              disabled={!edited.projectID() || !edited.environment()}
              onClick={generate}
            >
              {edited.environment() ? `Generate API key in ${edited.environment()!.name}` : "Generate API key"}
            </button>
          </Show>
        }
      />

      <EnvironmentSelectorLayout edited={edited} tools={<Select<ListFilter> value={filter()} options={FILTER_OPTIONS} onChange={setFilter} ariaLabel="Filter API keys" minWidth="140px" />}>

        <QueryState
          query={keys}
          pending={<TableSkeleton label="Loading API keys" columns={columns()} minWidth="min-w-200" />}
          error={(error) => loadError(error, "Could not load API keys.", API_KEY_ERROR_MESSAGES)}
          empty={{
            when: (data) => data.pages.every((page) => page.api_keys.length === 0),
            title: filter() === "active" ? `No API keys in ${edited.environment()?.name ?? "this environment"} yet.` : "No API keys match this filter.",
            hint: filter() === "active" ? "API keys let CI and scripts deploy and start sessions without signing in." : undefined,
            action: filter() === "active" && can("create") ? <button type="button" class={ui.button} disabled={!edited.environment()} onClick={generate}>Generate API key</button> : undefined,
          }}
        >
          {() => (
            <DataTable
              columns={columns()}
              minWidth="min-w-200"
              footer={<LoadMore count={keyItems().length} noun={{ one: "API key", other: "API keys" }} hasMore={keys.hasNextPage} loading={keys.isFetchingNextPage} failed={keys.isFetchNextPageError} onLoadMore={() => void keys.fetchNextPage()} />}
            >
              <For each={keyItems()}>{(keyItem) => <ApiKeyRow metadata={can("read")} creator={creator(keyItem)} canAssign={canAssign()} canUpdate={can("update") && can("read") && keyItem.created_at !== undefined} canRotate={can("rotate")} canRevoke={can("revoke")} onAccess={keyItem=>{const target=edited.target();if(target)setAccessing({keyItem,target});}} keyItem={keyItem} status={filter() === "all"} onRevoke={revoke} onEdit={keyItem=>{const target=edited.target();if(target)setEditing({keyItem,target})}} onRotate={keyItem=>{const target=edited.target();if(target)setRotating({keyItem,target})}} />}</For>
            </DataTable>
          )}
        </QueryState>
      </EnvironmentSelectorLayout>

      <Show when={issuing()}>
        {(target) => (
          <IssueApiKeyDialog
 catalog={catalog.data?.catalog ?? []} roles={catalog.data?.roles ?? []} canAssign={canAssign()} rolesLoading={catalog.isPending} rolesError={catalog.error}
            projectID={target().projectID}
            environmentID={target().environment.environment}
            projectName={target().projectName}
            environmentName={target().environment.name}
            environmentColorHex={environmentChip(target().environment)?.colorHex ?? ""}
            onClose={() => setIssuing(undefined)}
            onIssued={async (result) => {
              const { projectID, environment } = target();
              setIssuing(undefined);
              setIssued(result);
              await invalidateApiKeys(projectID, environment.environment);
            }}
          />
        )}
      </Show>

      <Show when={issued()}>
        {(key) => (
          <RevealDialog
            title="API key generated"
            value={key().raw_key}
            warning="This machine credential will not be shown again. Save it to a secret store now."
            onClose={() => setIssued(null)}
          />
        )}
      </Show>

      <Show when={accessing()} keyed>{current=><APIKeyAccess keyItem={current.keyItem} project={current.target.projectID} environment={current.target.environment.environment} onClose={()=>setAccessing(undefined)}/>}</Show>
      <Show when={editing()} keyed>{current=><EditApiKeyDialog keyItem={current.keyItem} target={current.target} onClose={()=>setEditing(undefined)} onSaved={async()=>{await invalidateApiKeys(current.target.projectID,current.target.environment.environment);setEditing(undefined)}}/>}</Show>
      <Show when={rotating()}>{current=><ConfirmDialog title="Rotate API key token" confirmLabel="Rotate token" busyLabel="Rotating…" target={current().keyItem.name} environment={environmentChip(current().target.environment)} errorMessage={apiKeyErrorMessage} onClose={()=>setRotating(undefined)} onConfirm={async()=>{
       const {keyItem,target}=current();const result=await rotateApiKey(target.projectID,target.environment.environment,keyItem.id);setIssued(result);await invalidateApiKeys(target.projectID,target.environment.environment);
      }}>The previous token stops working immediately. This key keeps its role assignments and ownership of existing sessions. Update your automation with the new token.</ConfirmDialog>}</Show>
      <Show when={revoking()}>
        {(current) => (
          <ConfirmDialog
            title="Revoke API key"
            confirmLabel="Revoke"
            busyLabel="Revoking…"
            tone="danger"
            target={current().keyItem.name}
            environment={environmentChip(current().target.environment)}
            errorMessage={apiKeyErrorMessage}
            onClose={() => setRevoking(undefined)}
            onConfirm={async () => {
              const { keyItem, target } = current();
              const projectID = target.projectID;
              const environmentID = target.environment.environment;
              await revokeApiKey(projectID, environmentID, keyItem.id);
              await invalidateApiKeys(projectID, environmentID);
            }}
          >
            Automation using this key loses access. Its existing sessions cannot admit further work under this key; an environment operator may need to stop or clean them up. Creating another key does not transfer ownership.
          </ConfirmDialog>
        )}
      </Show>
    </>
  );
}
