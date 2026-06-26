import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { Select, type SelectOption } from "../ui/Select";
import { ApiError } from "../lib/api";
import {
  issueApiKey,
  listApiKeys,
  revokeApiKey,
  type ApiKeyScope,
  type ApiKeyIssued,
  type ApiKeyStatus,
  type ApiKeySummary,
  type ListFilter,
} from "../lib/api-keys";
import { useScope } from "../lib/scope";
import { ActionMenu } from "../ui/ActionMenu";
import { Modal } from "../ui/Modal";
import { envDotStyle, statusBadgeClass, ui } from "../ui/styles";

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

const STATUS_LABELS: Record<ApiKeyStatus, string> = {
  active: "Active",
  expired: "Expired",
  revoked: "Revoked",
};

const API_KEY_ERROR_MESSAGES: Record<string, string> = {
  forbidden: "You do not have permission to manage API keys.",
  invalid_label: "Name must be 1-64 characters and contain no control characters.",
  invalid_expiry: "Choose a valid expiry from the dropdown.",
  invalid_permissions: "Select at least one API key permission.",
  invalid_filter: "Invalid filter.",
  not_found: "This key was already revoked or removed.",
  internal: "Something went wrong. Please try again.",
};
const INTERNAL_ERROR_MESSAGE = "Something went wrong. Please try again.";

const API_KEY_SCOPE_OPTIONS: {
  value: ApiKeyScope;
  label: string;
  description: string;
}[] = [
  {
    value: "runs:create",
    label: "Start tasks",
    description: "Allow automation to start deployed tasks in the selected project and environment.",
  },
  {
    value: "runs:read",
    label: "Read runs",
    description: "Allow automation to read run status, metadata, and logs.",
  },
  {
    value: "runs:manage",
    label: "Manage runs",
    description: "Allow automation to cancel runs and sessions.",
  },
  {
    value: "session-streams:read",
    label: "Read streams",
    description: "Allow automation to list session streams and read stream records.",
  },
  {
    value: "session-input:send",
    label: "Send input",
    description: "Allow automation to append input stream records in the selected environment.",
  },
  {
    value: "session-output:append",
    label: "Append output",
    description: "Allow automation to append output stream records in the selected environment.",
  },
  {
    value: "secrets:write",
    label: "Manage secrets",
    description: "Allow automation to list, create, update, and delete secrets in the selected project and environment.",
  },
  {
    value: "tokens:create",
    label: "Create tokens",
    description: "Allow automation to create externally completable tokens.",
  },
  {
    value: "tokens:read",
    label: "Read tokens",
    description: "Allow automation to inspect token status and metadata.",
  },
  {
    value: "tokens:complete",
    label: "Complete tokens",
    description: "Allow automation to complete tokens through the token API.",
  },
  {
    value: "tokens:cancel",
    label: "Cancel tokens",
    description: "Allow automation to cancel pending tokens.",
  },
  {
    value: "workspace-lifecycle:manage",
    label: "Manage workspaces",
    description: "Allow automation to create, update, materialize, stop, and delete workspaces.",
  },
  {
    value: "workspace-exec:create",
    label: "Start execs",
    description: "Allow automation to start workspace execs.",
  },
  {
    value: "workspace-exec:read",
    label: "Read execs",
    description: "Allow automation to read workspace exec metadata and output.",
  },
  {
    value: "workspace-exec:manage",
    label: "Manage execs",
    description: "Allow automation to manage workspace exec lifecycle.",
  },
  {
    value: "workspace-pty:create",
    label: "Create PTYs",
    description: "Allow automation to create workspace PTY sessions.",
  },
  {
    value: "workspace-pty:read",
    label: "Read PTYs",
    description: "Allow automation to read workspace PTY metadata and output.",
  },
  {
    value: "workspace-pty:manage",
    label: "Manage PTYs",
    description: "Allow automation to manage workspace PTY lifecycle.",
  },
  {
    value: "tasks:deploy",
    label: "Deploy tasks",
    description: "Allow automation to upload deployment source archives in the selected project and environment.",
  },
];

const DEFAULT_SCOPES: ApiKeyScope[] = ["runs:create", "runs:read"];

function apiKeyErrorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    return API_KEY_ERROR_MESSAGES[error.errorKind] ?? error.message ?? INTERNAL_ERROR_MESSAGE;
  }
  return INTERNAL_ERROR_MESSAGE;
}

function relativeTime(iso: string | null): string {
  if (!iso) return "—";
  const time = new Date(iso).getTime();
  if (Number.isNaN(time)) return "—";

  const diff = time - Date.now();
  const abs = Math.abs(diff);
  const past = diff < 0;
  if (abs < 45_000) return past ? "just now" : "in a few seconds";

  const minute = 60_000;
  const hour = 60 * minute;
  const day = 24 * hour;
  const units = [
    { name: "year", value: 365 * day },
    { name: "month", value: 30 * day },
    { name: "day", value: day },
    { name: "hour", value: hour },
    { name: "minute", value: minute },
  ];
  const unit = units.find((candidate) => abs >= candidate.value) ?? units[units.length - 1];
  if (!unit) return "—";
  const count = Math.max(1, Math.round(abs / unit.value));

  if (unit.name === "day" && count === 1) {
    return past ? "yesterday" : "tomorrow";
  }
  return past
    ? `${count} ${unit.name}${count === 1 ? "" : "s"} ago`
    : `in ${count} ${unit.name}${count === 1 ? "" : "s"}`;
}

function expiryText(iso: string | null): string {
  return iso ? relativeTime(iso) : "never";
}

function lastUsedText(iso: string | null): string {
  return iso ? relativeTime(iso) : "never";
}

function validateLabel(value: string): string | null {
  const label = value.trim();
  if (label.length < 1 || label.length > 64 || /[\u0000-\u001F\u007F]/.test(label)) {
    return API_KEY_ERROR_MESSAGES["invalid_label"] ?? INTERNAL_ERROR_MESSAGE;
  }
  return null;
}

function permissionText(keyItem: ApiKeySummary): string {
  const grants = keyItem.permissions ?? [];
  if (grants.length === 0) return "Not reported";
  const scope = `${shortScopeID(keyItem.project_id)} / ${shortScopeID(keyItem.environment_id)}`;
  return grants.map((grant) => {
    const labels = API_KEY_SCOPE_OPTIONS
      .filter((option) => grant.scopes.includes(option.value))
      .map((option) => option.label);
    return `${scope}: ${labels.length > 0 ? labels.join(", ") : "Custom permissions"}`;
  }).join("; ");
}

function shortScopeID(id: string): string {
  return id.slice(0, 8);
}

function ApiKeyStatusBadge(props: { status: ApiKeyStatus }) {
  const tone = (): "succeeded" | "expired" | "revoked" => {
    if (props.status === "active") return "succeeded";
    if (props.status === "expired") return "expired";
    return "revoked";
  };
  return (
    <span class={statusBadgeClass(tone())}>
      {STATUS_LABELS[props.status]}
    </span>
  );
}

function ApiKeyRow(props: {
  keyItem: ApiKeySummary;
  revoking: boolean;
  error: string | null;
  onRevoke: (keyItem: ApiKeySummary) => void;
}) {
  return (
    <tr>
      <td>{props.keyItem.name}</td>
      <td><code>{props.keyItem.key_prefix}...</code></td>
      <td>{permissionText(props.keyItem)}</td>
      <td><ApiKeyStatusBadge status={props.keyItem.status} /></td>
      <td>{lastUsedText(props.keyItem.last_used_at)}</td>
      <td>{relativeTime(props.keyItem.created_at)}</td>
      <td>{expiryText(props.keyItem.expires_at)}</td>
      <td class={ui.actionsCell}>
        <Show when={props.keyItem.status === "active"} fallback={<span class={ui.muted}>No actions</span>}>
          <ActionMenu
            label={`Actions for ${props.keyItem.name}`}
            items={[{
              label: "Revoke",
              busyLabel: props.revoking ? "Revoking..." : undefined,
              disabled: props.revoking,
              tone: "danger",
              onSelect: () => props.onRevoke(props.keyItem),
            }]}
          />
        </Show>
        <Show when={props.error}>
          <p class={ui.rowError} role="alert">{props.error}</p>
        </Show>
      </td>
    </tr>
  );
}

function IssueApiKeyModal(props: {
  projectID: string;
  environmentID: string;
  projectName: string;
  environmentName: string;
  environmentColorHex: string;
  onClose: () => void;
  onIssued: () => Promise<void>;
}) {
  const [label, setLabel] = createSignal("");
  const [expiry, setExpiry] = createSignal<ExpiryValue>("never");
  const [selectedScopes, setSelectedScopes] = createSignal<ApiKeyScope[]>([...DEFAULT_SCOPES]);
  const [issued, setIssued] = createSignal<ApiKeyIssued | null>(null);
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const [copyState, setCopyState] = createSignal<string | null>(null);

  const expiryDays = createMemo(() => {
    const value = expiry();
    return value === "never" ? null : Number(value);
  });

  const closeAndClear = () => {
    setIssued(null);
    setLabel("");
    setSelectedScopes([...DEFAULT_SCOPES]);
    setError(null);
    setCopyState(null);
    props.onClose();
  };

  const toggleScope = (scope: ApiKeyScope, checked: boolean) => {
    setSelectedScopes((current) => {
      if (checked) return current.includes(scope) ? current : [...current, scope];
      return current.filter((candidate) => candidate !== scope);
    });
  };

  const submit = async (event: Event) => {
    event.preventDefault();
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
      const result = await issueApiKey(props.projectID, props.environmentID, {
        name: label().trim(),
        expires_in_days: expiryDays(),
        permissions: [{
          scopes: selectedScopes(),
        }],
      });
      setIssued(result);
      setCopyState(null);
      await props.onIssued();
    } catch (issueError) {
      setError(apiKeyErrorMessage(issueError));
    } finally {
      setSubmitting(false);
    }
  };

  const copyIssuedKey = async () => {
    const current = issued();
    if (!current) return;
    try {
      await navigator.clipboard.writeText(current.raw_key);
      setCopyState("Copied");
    } catch {
      setCopyState("Copy failed");
    }
  };

  return (
    <Modal
      title={issued() ? "API key generated" : "Generate API key"}
      onClose={closeAndClear}
      closeDisabled={submitting()}
    >
      <Show when={issued()} keyed fallback={
        <form onSubmit={submit}>
          <p class={ui.modalIntro}>Create a machine credential for automation. Limit it to the permissions this workflow needs.</p>
          <div class={ui.scopeTarget} aria-label="API key target environment">
            <span>Target environment</span>
            <strong>{props.environmentName}</strong>
            <div>
              <Show when={props.environmentColorHex}>
                <span class={ui.scopeTargetDot} style={envDotStyle(props.environmentColorHex)} aria-hidden="true" />
              </Show>
              <span>{props.projectName}</span>
              <code>{shortScopeID(props.projectID)} / {shortScopeID(props.environmentID)}</code>
            </div>
          </div>
          <label class={ui.field}>
            <span>Name</span>
            <input
              class={ui.input}
              type="text"
              value={label()}
              maxLength={64}
              autocomplete="off"
              onInput={(event) => setLabel(event.currentTarget.value)}
              autofocus
            />
          </label>
          <label class={ui.field}>
            <span>Expiry</span>
            <Select<ExpiryValue>
              value={expiry()}
              options={EXPIRY_OPTIONS}
              onChange={setExpiry}
              ariaLabel="Expiry"
              minWidth="100%"
            />
          </label>
          <fieldset class={ui.fieldSet}>
            <legend class={ui.fieldLegend}>Permissions for this environment</legend>
            <div class={"grid gap-1.5"}>
              <For each={API_KEY_SCOPE_OPTIONS}>
                {(option) => (
                  <label class={ui.permissionOption}>
                    <input
                      type="checkbox"
                      checked={selectedScopes().includes(option.value)}
                      onChange={(event) => toggleScope(option.value, event.currentTarget.checked)}
                    />
                    <span>
                      <strong>{option.label}</strong>
                      <span>{option.description}</span>
                    </span>
                  </label>
                )}
              </For>
            </div>
          </fieldset>
          <Show when={error()}>
            <p class={ui.error} role="alert">{error()}</p>
          </Show>
          <div class={ui.modalActions}>
            <button type="button" class={ui.secondaryButton} disabled={submitting()} onClick={closeAndClear}>
              Cancel
            </button>
            <button class={ui.button} type="submit" disabled={submitting()}>
              {submitting() ? "Generating..." : "Generate"}
            </button>
          </div>
        </form>
      }>
        {(current) => (
          <>
            <p class={ui.warning}>This machine credential will not be shown again. Save it to a secret store now.</p>
            <code class={ui.rawKey}>{current.raw_key}</code>
            <div class={ui.modalActions}>
              <button type="button" class={ui.secondaryButton} onClick={closeAndClear}>Done</button>
              <button class={ui.button} type="button" onClick={copyIssuedKey}>Copy</button>
            </div>
            <Show when={copyState()}>
              <p class={ui.inlineState} role="status">{copyState()}</p>
            </Show>
          </>
        )}
      </Show>
    </Modal>
  );
}

export function ApiKeys() {
  const scope = useScope();
  const queryClient = useQueryClient();
  const [filter, setFilter] = createSignal<ListFilter>("active");
  const [modalOpen, setModalOpen] = createSignal(false);
  const [revokingId, setRevokingId] = createSignal<string | null>(null);
  const [revokeError, setRevokeError] = createSignal<{ id: string; message: string } | null>(null);

  const keys = createQuery(() => ({
    queryKey: ["api-keys", scope.selectedProjectID(), scope.selectedEnvironmentID(), filter()],
    queryFn: () => listApiKeys(scope.selectedProjectID(), scope.selectedEnvironmentID(), filter()),
    retry: false,
  }));

  const invalidateApiKeys = () => queryClient.invalidateQueries({ queryKey: ["api-keys", scope.selectedProjectID(), scope.selectedEnvironmentID()] });

  const revoke = async (keyItem: ApiKeySummary) => {
    if (!window.confirm(`Revoke API key "${keyItem.name}"?`)) return;
    setRevokeError(null);
    setRevokingId(keyItem.id);
    try {
      await revokeApiKey(scope.selectedProjectID(), scope.selectedEnvironmentID(), keyItem.id);
      await invalidateApiKeys();
    } catch (error) {
      setRevokeError({ id: keyItem.id, message: apiKeyErrorMessage(error) });
    } finally {
      setRevokingId(null);
    }
  };

  return (
    <>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>API keys</h1>
          <p class={ui.pageSubtitle}>Machine credentials for automation, limited by the permissions selected when each key is generated.</p>
        </div>
        <button
          class={ui.button}
          type="button"
          disabled={!scope.selectedProjectID() || !scope.selectedEnvironmentID()}
          onClick={() => setModalOpen(true)}
        >
          Generate new API key
        </button>
      </div>

      <div class={ui.toolbar}>
        <div class={ui.toolbarSide} />
        <div class={ui.toolbarSide}>
          <span class={ui.filterField}>
            <span>Filter</span>
            <Select<ListFilter>
              value={filter()}
              options={FILTER_OPTIONS}
              onChange={setFilter}
              ariaLabel="Filter API keys"
              minWidth="140px"
            />
          </span>
        </div>
      </div>

      <Show when={keys.isError}>
        <p class={ui.error} role="alert">{apiKeyErrorMessage(keys.error)}</p>
      </Show>

      <Show when={keys.data?.has_more}>
        <div class={ui.hasMoreBanner} role="status">
          Showing the most recent 200 keys. Narrow the filter to find older keys.
        </div>
      </Show>

      <Show when={!keys.isPending} fallback={<p class={ui.muted}>Loading API keys...</p>}>
        <Show when={(keys.data?.items.length ?? 0) > 0} fallback={<p class={ui.emptyState}>No API keys found.</p>}>
          <div class={ui.tableWrap}>
            <table class={ui.apiKeyTable}>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Key prefix</th>
                  <th>Permissions</th>
                  <th>Status</th>
                  <th>Last used</th>
                  <th>Created</th>
                  <th>Expires</th>
                  <th><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                <For each={keys.data?.items ?? []}>
                  {(keyItem) => (
                    <ApiKeyRow
                      keyItem={keyItem}
                      revoking={revokingId() === keyItem.id}
                      error={revokeError()?.id === keyItem.id ? revokeError()?.message ?? null : null}
                      onRevoke={revoke}
                    />
                  )}
                </For>
              </tbody>
            </table>
          </div>
        </Show>
      </Show>

      <Show when={modalOpen()}>
        <IssueApiKeyModal
          projectID={scope.selectedProjectID()}
          environmentID={scope.selectedEnvironmentID()}
          projectName={scope.selectedProject()?.name ?? "Project"}
          environmentName={scope.selectedEnvironment()?.name ?? "Environment"}
          environmentColorHex={scope.selectedEnvironment()?.color_hex ?? ""}
          onClose={() => setModalOpen(false)}
          onIssued={invalidateApiKeys}
        />
      </Show>
    </>
  );
}
