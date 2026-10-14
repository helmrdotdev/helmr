import { createInfiniteQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { Select, type SelectOption } from "../ui/Select";
import { errorMessage } from "../lib/error";
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
import { DataTable, LoadMore, RowActions } from "../ui/DataTable";
import { ConfirmDialog, FormDialog, RevealDialog } from "../ui/Dialog";
import { CheckboxGroup, Field, TextInput } from "../ui/Field";
import { Toolbar } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { QueryState } from "../ui/QueryState";
import { RelativeTime } from "../ui/RelativeTime";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";
import { EnvironmentTarget } from "../ui/EnvChip";

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
      { value: "sessions:read", label: "Read sessions", description: "Allow automation to read session state in the selected project and environment." },
      { value: "sessions:create", label: "Start agents", description: "Allow automation to start declared agents." },
      { value: "sessions:send", label: "Send session work and messages", description: "Allow automation to enqueue turns and send session or exact turn messages." },
      { value: "sessions:interrupt", label: "Interrupt sessions", description: "Interrupt the session subtree and hold queued work until resumed." },
      { value: "sessions:resume", label: "Resume sessions", description: "Resume queued work after an exact hold converges." },
      { value: "sessions:close", label: "Close sessions", description: "Drain accepted work and close sessions." },
      { value: "sessions:cancel", label: "Cancel sessions", description: "Cancel queued turns, stop active execution, and close sessions." },
      { value: "asks:respond", label: "Answer questions", description: "Allow this API key to answer pending questions in the selected project and environment." },
    ],
  },
  {
    label: "Computers",
    options: [
      { value: "computers:read", label: "Read computers", description: "Allow automation to read computer state and configuration." },
      { value: "computers:create", label: "Create computers", description: "Allow automation to create declared computers." },
      { value: "computers:update", label: "Update computer retention", description: "Allow automation to change computer retention policies." },
      { value: "computers:delete", label: "Delete computers", description: "Allow automation to delete computers." },
      { value: "computer-exec:create", label: "Run computer commands", description: "Allow automation to start commands on computers." },
    ],
  },
  { label: "Secrets", options: [{ value: "secrets:write", label: "Manage secrets", description: "Allow automation to list, create, rotate, and revoke secrets in the selected project and environment." }] },
  { label: "Deployments", options: [{ value: "deployments:write", label: "Deploy bundles", description: "Allow automation to upload, finalize, and promote deployment bundles in the selected project and environment." }] },
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

function permissionText(keyItem: ApiKeySummary): string {
  const scopes = (keyItem.permissions ?? []).flatMap((grant) => grant.scopes);
  if (scopes.length === 0) return "Not reported";
  return scopes.map((scope) => PERMISSION_LABELS.get(scope) ?? scope).join(", ");
}

function ApiKeyRow(props: { keyItem: ApiKeySummary; onRevoke: (keyItem: ApiKeySummary) => void }) {
  return (
    <tr>
      <td>{props.keyItem.name}</td>
      <td><StatusBadge resource="api_key" status={props.keyItem.status} /></td>
      <td><code>{props.keyItem.key_prefix}...</code></td>
      <td>{permissionText(props.keyItem)}</td>
      <td><RelativeTime value={props.keyItem.last_used_at} fallback="never" /></td>
      <td><RelativeTime value={props.keyItem.created_at} /></td>
      <td><RelativeTime value={props.keyItem.expires_at} fallback="never" /></td>
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
      intro="Create a machine credential for automation. Limit it to the permissions this workflow needs."
      submitLabel="Generate"
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
        projectID={props.projectID}
        environmentID={props.environmentID}
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

  const invalidateApiKeys = (projectID: string, environmentID: string) =>
    queryClient.invalidateQueries({ queryKey: ["api-keys", projectID, environmentID] });

  return (
    <>
      <PageHeader
        title="API keys"
        subtitle="Limited to the permissions chosen when generated."
        actions={
          <button
            class={ui.button}
            type="button"
            disabled={!edited.projectID() || !edited.environment()}
            onClick={() => setIssuing(edited.target())}
          >
            {edited.environment() ? `Generate API key in ${edited.environment()!.name}` : "Generate API key"}
          </button>
        }
      />

      <EnvironmentSelectorLayout edited={edited}>
        <Toolbar
          start={<Select<ListFilter> value={filter()} options={FILTER_OPTIONS} onChange={setFilter} ariaLabel="Filter API keys" minWidth="140px" />}
        />

        <QueryState
          query={keys}
          pending="Loading API keys..."
          error={apiKeyErrorMessage}
          empty={{ when: (data) => data.pages.every((page) => page.api_keys.length === 0), title: "No API keys found." }}
        >
          {() => (
            <DataTable
              columns={["Name", "Status", "Key prefix", "Permissions", "Last used", "Created", "Expires", { label: "Actions", srOnly: true }]}
              minWidth="min-w-270"
              footer={<LoadMore count={keyItems().length} noun={{ one: "API key", other: "API keys" }} hasMore={keys.hasNextPage} loading={keys.isFetchingNextPage} failed={keys.isFetchNextPageError} onLoadMore={() => void keys.fetchNextPage()} />}
            >
              <For each={keyItems()}>{(keyItem) => <ApiKeyRow keyItem={keyItem} onRevoke={revoke} />}</For>
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
