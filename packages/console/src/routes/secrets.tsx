import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { formatRelative } from "../features/runs/display";
import { ApiError } from "../lib/api";
import { createSecret, listSecrets, revokeSecret, rotateSecret, type Secret } from "../lib/secrets";
import { useScope } from "../lib/scope";
import { ActionMenu } from "../ui/ActionMenu";
import { Modal } from "../ui/Modal";
import { envDotStyle, ui } from "../ui/styles";

const SECRET_ERROR_MESSAGES: Record<string, string> = {
  forbidden: "You do not have permission to manage secrets.",
  not_found: "This secret no longer exists.",
  internal: "Something went wrong. Please try again.",
};
const INTERNAL_ERROR_MESSAGE = "Something went wrong. Please try again.";

function secretErrorMessage(error: unknown): string {
  if (error instanceof ApiError) return SECRET_ERROR_MESSAGES[error.code] ?? error.message ?? INTERNAL_ERROR_MESSAGE;
  return INTERNAL_ERROR_MESSAGE;
}

function shortScopeID(id: string): string {
  return id.slice(0, 8);
}

function SecretModal(props: {
  secret: Secret | null;
  projectID: string;
  environmentID: string;
  projectName: string;
  environmentName: string;
  environmentColorHex: string;
  onClose: () => void;
  onSaved: () => Promise<void>;
}) {
  const editing = createMemo(() => props.secret !== null);
  const [name, setName] = createSignal(props.secret?.name ?? "");
  const [value, setValue] = createSignal("");
  const [saving, setSaving] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const save = async (event: Event) => {
    event.preventDefault();
    setError(null);
    setSaving(true);
    try {
      if (props.secret) {
        await rotateSecret(props.secret.id, value(), props.projectID, props.environmentID);
      } else {
        await createSecret(name().trim(), value(), props.projectID, props.environmentID);
      }
      await props.onSaved();
      props.onClose();
    } catch (saveError) {
      setError(secretErrorMessage(saveError));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal title={editing() ? "Update secret" : "Set secret"} onClose={props.onClose} closeDisabled={saving()}>
      <form onSubmit={save}>
        <div class={ui.scopeTarget} aria-label="Secret target environment">
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
            value={name()}
            disabled={editing()}
            autocomplete="off"
            autofocus={!editing()}
            onInput={(event) => setName(event.currentTarget.value)}
          />
        </label>
        <label class={ui.field}>
          <span>Value</span>
          <textarea
            class={ui.textarea}
            value={value()}
            autofocus={editing()}
            onInput={(event) => setValue(event.currentTarget.value)}
          />
        </label>
        <Show when={error()}>
          <p class={ui.error} role="alert">{error()}</p>
        </Show>
        <div class={ui.modalActions}>
          <button type="button" class={ui.secondaryButton} disabled={saving()} onClick={props.onClose}>
            Cancel
          </button>
          <button class={ui.button} type="submit" disabled={saving() || name().trim() === ""}>
            {saving() ? "Saving..." : "Save"}
          </button>
        </div>
      </form>
    </Modal>
  );
}

function SecretRow(props: {
  secret: Secret;
  revoking: boolean;
  error: string | null;
  onUpdate: (secret: Secret) => void;
  onRevoke: (secret: Secret) => void;
}) {
  return (
    <tr>
      <td><code>{props.secret.name}</code></td>
      <td>{props.secret.status}</td>
      <td>{props.secret.rotated_at ? formatRelative(props.secret.rotated_at) : "Never"}</td>
      <td>{formatRelative(props.secret.created_at)}</td>
      <td class={ui.actionsCell}>
        <ActionMenu
          label={`Actions for ${props.secret.name}`}
          items={[
            {
              label: "Update",
              disabled: props.revoking || props.secret.status === "revoked",
              onSelect: () => props.onUpdate(props.secret),
            },
            {
              label: "Revoke",
              busyLabel: props.revoking ? "Revoking..." : undefined,
              disabled: props.revoking || props.secret.status === "revoked",
              tone: "danger",
              onSelect: () => props.onRevoke(props.secret),
            },
          ]}
        />
        <Show when={props.error}>
          <p class={ui.rowError} role="alert">{props.error}</p>
        </Show>
      </td>
    </tr>
  );
}

export function Secrets() {
  const scope = useScope();
  const queryClient = useQueryClient();
  const [modalSecret, setModalSecret] = createSignal<Secret | null | undefined>(undefined);
  const [revokingID, setRevokingID] = createSignal<string | null>(null);
  const [revokeError, setRevokeError] = createSignal<{ id: string; message: string } | null>(null);
  const secrets = createQuery(() => ({
    queryKey: ["secrets", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () => listSecrets(scope.selectedProjectID(), scope.selectedEnvironmentID()),
    enabled: !!scope.selectedProjectID() && !!scope.selectedEnvironmentID(),
    retry: false,
  }));

  const invalidateSecrets = () => queryClient.invalidateQueries({ queryKey: ["secrets"] });

  const revoke = async (secret: Secret) => {
    if (!window.confirm(`Revoke secret "${secret.name}"?`)) return;
    setRevokeError(null);
    setRevokingID(secret.id);
    try {
      await revokeSecret(secret.id, scope.selectedProjectID(), scope.selectedEnvironmentID());
      await invalidateSecrets();
    } catch (error) {
      setRevokeError({ id: secret.id, message: secretErrorMessage(error) });
    } finally {
      setRevokingID(null);
    }
  };

  return (
    <>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Secrets</h1>
          <p class={ui.pageSubtitle}>Environment-scoped secret names for tasks. Values are never displayed after saving.</p>
        </div>
        <button class={ui.button} type="button" disabled={!scope.selectedEnvironmentID()} onClick={() => setModalSecret(null)}>Set secret</button>
      </div>

      <Show when={secrets.isError}>
        <p class={ui.error} role="alert">{secrets.error instanceof ApiError ? secretErrorMessage(secrets.error) : "Could not load secrets."}</p>
      </Show>

      <Show when={!secrets.isPending} fallback={<p class={ui.muted}>Loading secrets...</p>}>
        <Show when={(secrets.data?.secrets.length ?? 0) > 0} fallback={<p class={ui.emptyState}>No secrets found.</p>}>
          <div class={ui.tableWrap}>
            <table class={ui.dataTable}>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>State</th>
                  <th>Rotated</th>
                  <th>Created</th>
                  <th><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                <For each={secrets.data?.secrets ?? []}>
                  {(secret) => (
                    <SecretRow
                      secret={secret}
                      revoking={revokingID() === secret.id}
                      error={revokeError()?.id === secret.id ? revokeError()?.message ?? null : null}
                      onUpdate={setModalSecret}
                      onRevoke={revoke}
                    />
                  )}
                </For>
              </tbody>
            </table>
          </div>
        </Show>
      </Show>

      <Show when={modalSecret() !== undefined}>
        <SecretModal
          secret={modalSecret() ?? null}
          projectID={scope.selectedProjectID()}
          environmentID={scope.selectedEnvironmentID()}
          projectName={scope.selectedProject()?.name ?? "Project"}
          environmentName={scope.selectedEnvironment()?.name ?? "Environment"}
          environmentColorHex={scope.selectedEnvironment()?.color_hex ?? ""}
          onClose={() => setModalSecret(undefined)}
          onSaved={invalidateSecrets}
        />
      </Show>
    </>
  );
}
