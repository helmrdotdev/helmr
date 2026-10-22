import { createInfiniteQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { Select, type SelectOption } from "../ui/Select";
import { errorMessage, loadError } from "../lib/error";
import {
  issueApiKey,
  listApiKeys,
  revokeApiKey,
  type ApiKeyScope,
  type ApiKeyIssued,
  type ApiKeySummary,
  type ListFilter,
} from "../lib/api-keys";
import { environmentChip } from "../features/projects/display";
import { EnvironmentSelectorLayout, useEditedEnvironment, type EditTarget } from "../components/EnvironmentSelector";
import { DataTable, EmptyCell, LoadMore, RowActions } from "../ui/DataTable";
import { ConfirmDialog, FormDialog, RevealDialog } from "../ui/Dialog";
import { CheckboxGroup, Field, TextInput } from "../ui/Field";
import { Toolbar } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { isDenied, QueryState } from "../ui/QueryState";
import { TableSkeleton } from "../ui/Skeleton";
import { RelativeTime } from "../ui/RelativeTime";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";
import { EnvironmentTarget } from "../ui/EnvChip";

const COLUMNS = ["Name", "Key", "Permissions", "Last used", "Expires", { label: "Actions", srOnly: true }] as const;
// "All" mixes active, expired and revoked keys, so it adds their status.
const ALL_COLUMNS = ["Name", "Status", "Key", "Permissions", "Last used", "Expires", { label: "Actions", srOnly: true }] as const;

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
  invalid_permissions: "Select at least one API key permission.",
  not_found: "This key was already revoked or removed.",
};
const INTERNAL_ERROR_MESSAGE = "Something went wrong. Please try again.";

const API_KEY_PERMISSION_GROUPS: { label: string; options: { value: ApiKeyScope; label: string; description: string }[] }[] = [
  {
    label: "Sessions",
    options: [
      { value: "sessions:read", label: "Read sessions", description: "Read sessions and their history." },
      { value: "sessions:create", label: "Start agents", description: "Start sessions of the agents in the deployment." },
      { value: "sessions:send", label: "Send work to sessions", description: "Send new work and messages to running sessions." },
      { value: "sessions:interrupt", label: "Interrupt sessions", description: "Stop running work and hold what is queued." },
      { value: "sessions:resume", label: "Resume held work", description: "Release held work so it runs again." },
      { value: "sessions:close", label: "Close sessions", description: "Finish accepted work, then close the session." },
      { value: "sessions:cancel", label: "Cancel sessions", description: "Stop all work now and close the session." },
      { value: "asks:answer", label: "Answer questions", description: "Answer questions that agents ask." },
    ],
  },
  {
    label: "Computers",
    options: [
      { value: "computers:read", label: "Read computers", description: "Read computers and their settings." },
      { value: "computers:create", label: "Create computers", description: "Create computers from the deployment's templates." },
      { value: "computers:update", label: "Hibernate, wake, and change retention", description: "Pause or resume execution for the whole computer, and change how long it is kept." },
      { value: "computers:delete", label: "Delete computers", description: "Delete computers and their disks." },
      { value: "computer-exec:create", label: "Run commands on computers", description: "Start commands on computers." },
    ],
  },
  { label: "Secrets", options: [{ value: "secrets:write", label: "Manage secrets", description: "List, set, rotate and revoke secrets." }] },
  { label: "Deployments", options: [{ value: "deployments:write", label: "Deploy", description: "Upload deployments and promote one to current." }] },
];

const PERMISSION_LABELS = new Map<string, string>(API_KEY_PERMISSION_GROUPS.flatMap((group) => group.options.map((option) => [option.value, option.label] as const)));

const DEFAULT_SCOPES: ApiKeyScope[] = ["sessions:create", "sessions:read"];

const apiKeyErrorMessage = (error: unknown) => errorMessage(error, INTERNAL_ERROR_MESSAGE, API_KEY_ERROR_MESSAGES);

function validateLabel(value: string): string | null {
  const label = value.trim();
  if (label.length < 1 || label.length > 64 || /[\u0000-\u001F\u007F]/.test(label)) {
    return API_KEY_ERROR_MESSAGES["invalid_label"] ?? INTERNAL_ERROR_MESSAGE;
  }
  return null;
}

// A key's permissions as a count; their names are its tooltip and label.
function Permissions(props: { keyItem: ApiKeySummary }) {
  const labels = () => (props.keyItem.permissions ?? []).flatMap((grant) => grant.scopes).map((scope) => PERMISSION_LABELS.get(scope) ?? scope);
  return (
    <Show when={labels().length > 0} fallback={<EmptyCell />}>
      <span class="cursor-help underline decoration-dotted underline-offset-4" title={labels().join("\n")}>
        {labels().length} {labels().length === 1 ? "permission" : "permissions"}
        <span class="sr-only">: {labels().join(", ")}</span>
      </span>
    </Show>
  );
}

function ApiKeyRow(props: { keyItem: ApiKeySummary; status: boolean; onRevoke: (keyItem: ApiKeySummary) => void }) {
  return (
    <tr>
      <td><span class="font-mono text-chrome">{props.keyItem.name}</span></td>
      <Show when={props.status}><td><StatusBadge resource="api_key" status={props.keyItem.status} /></td></Show>
      <td><code>{props.keyItem.key_prefix}…</code></td>
      <td><Permissions keyItem={props.keyItem} /></td>
      <td><RelativeTime value={props.keyItem.last_used_at} fallback="—" /></td>
      <td><RelativeTime value={props.keyItem.expires_at} fallback="Never" /></td>
      <RowActions
        label={`Actions for ${props.keyItem.name}`}
        items={props.keyItem.status === "active" ? [{ label: "Revoke", tone: "danger", onSelect: () => props.onRevoke(props.keyItem) }] : []}
      />
    </tr>
  );
}

function IssueApiKeyDialog(props: {
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
  const [selectedScopes, setSelectedScopes] = createSignal<ApiKeyScope[]>([...DEFAULT_SCOPES]);
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const submit = async () => {
    setError(null);
    const validation = validateLabel(label());
    if (validation) {
      setError(validation);
      return;
    }
    if (selectedScopes().length === 0) {
      setError(API_KEY_ERROR_MESSAGES["invalid_permissions"] ?? INTERNAL_ERROR_MESSAGE);
      return;
    }
    setSubmitting(true);
    try {
      const value = expiry();
      const result = await issueApiKey(props.projectID, props.environmentID, {
        name: label().trim(),
        expires_in_days: value === "never" ? null : Number(value),
        permissions: [{ scopes: selectedScopes() }],
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
      intro="A key for automation such as CI. It can only do what you allow below."
      submitLabel="Generate key"
      busyLabel="Generating..."
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
      <CheckboxGroup label="Permissions for this environment" groups={API_KEY_PERMISSION_GROUPS} value={selectedScopes()} onChange={setSelectedScopes} />
    </FormDialog>
  );
}

export function ApiKeys() {
  const edited = useEditedEnvironment();
  const queryClient = useQueryClient();
  const [filter, setFilter] = createSignal<ListFilter>("active");
  const [issuing, setIssuing] = createSignal<EditTarget>();
  const [issued, setIssued] = createSignal<ApiKeyIssued | null>(null);
  const [revoking, setRevoking] = createSignal<{ keyItem: ApiKeySummary; target: EditTarget }>();
  const revoke = (keyItem: ApiKeySummary) => {
    const target = edited.target();
    if (target) setRevoking({ keyItem, target });
  };

  const keys = createInfiniteQuery(() => ({
    queryKey: ["api-keys", edited.projectID(), edited.environmentID(), filter()],
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
  const keyItems = createMemo(() => keys.data?.pages.flatMap((page) => page.api_keys) ?? []);
  const columns = () => (filter() === "all" ? ALL_COLUMNS : COLUMNS);
  // While the active list is empty its empty state offers the one action.
  const noKeys = () => filter() === "active" && keys.data !== undefined && keyItems().length === 0;
  const generate = () => setIssuing(edited.target());

  const invalidateApiKeys = (projectID: string, environmentID: string) =>
    queryClient.invalidateQueries({ queryKey: ["api-keys", projectID, environmentID] });

  return (
    <>
      <PageHeader
        title="API keys"
        subtitle="Limited to the permissions chosen when generated."
        actions={
          <Show when={!isDenied(keys.error) && !keys.isPending && !noKeys()}>
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

      <EnvironmentSelectorLayout edited={edited}>
        <Toolbar
          start={<Select<ListFilter> value={filter()} options={FILTER_OPTIONS} onChange={setFilter} ariaLabel="Filter API keys" minWidth="140px" />}
        />

        <QueryState
          query={keys}
          pending={<TableSkeleton label="Loading API keys" columns={columns()} minWidth="min-w-200" />}
          error={(error) => loadError(error, "Could not load API keys.", API_KEY_ERROR_MESSAGES)}
          empty={{
            when: (data) => data.pages.every((page) => page.api_keys.length === 0),
            title: filter() === "active" ? `No API keys in ${edited.environment()?.name ?? "this environment"} yet.` : "No API keys match this filter.",
            hint: filter() === "active" ? "API keys let CI and scripts deploy and start sessions without signing in." : undefined,
            action: filter() === "active" ? <button type="button" class={ui.button} disabled={!edited.environment()} onClick={generate}>Generate API key</button> : undefined,
          }}
        >
          {() => (
            <DataTable
              columns={columns()}
              minWidth="min-w-200"
              footer={<LoadMore count={keyItems().length} noun={{ one: "API key", other: "API keys" }} hasMore={keys.hasNextPage} loading={keys.isFetchingNextPage} failed={keys.isFetchNextPageError} onLoadMore={() => void keys.fetchNextPage()} />}
            >
              <For each={keyItems()}>{(keyItem) => <ApiKeyRow keyItem={keyItem} status={filter() === "all"} onRevoke={revoke} />}</For>
            </DataTable>
          )}
        </QueryState>
      </EnvironmentSelectorLayout>

      <Show when={issuing()}>
        {(target) => (
          <IssueApiKeyDialog
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

      <Show when={revoking()}>
        {(current) => (
          <ConfirmDialog
            title="Revoke API key"
            confirmLabel="Revoke"
            busyLabel="Revoking..."
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
            Automation using this key stops working immediately.
          </ConfirmDialog>
        )}
      </Show>
    </>
  );
}
