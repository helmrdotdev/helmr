import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { formatRelative } from "../features/runs/display";
import { ApiError } from "../lib/api";
import { deleteSecret, listSecrets, setSecret, type Secret } from "../lib/secrets";
import { useScope } from "../lib/scope";
import { ActionMenu } from "../ui/ActionMenu";
import { Modal } from "../ui/Modal";
import { ui } from "../ui/styles";

const SECRET_ERROR_MESSAGES: Record<string, string> = {
  forbidden: "You do not have permission to manage secrets.",
  not_found: "This secret no longer exists.",
  internal: "Something went wrong. Please try again.",
};
const INTERNAL_ERROR_MESSAGE = "Something went wrong. Please try again.";

function secretErrorMessage(error: unknown): string {
  if (error instanceof ApiError) return SECRET_ERROR_MESSAGES[error.errorKind] ?? error.message ?? INTERNAL_ERROR_MESSAGE;
  return INTERNAL_ERROR_MESSAGE;
}

function SecretModal(props: {
  secretName: string | null;
  projectID: string;
  environmentID: string;
  onClose: () => void;
  onSaved: () => Promise<void>;
}) {
  const editing = createMemo(() => props.secretName !== null);
  const [name, setName] = createSignal(props.secretName ?? "");
  const [value, setValue] = createSignal("");
  const [saving, setSaving] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const save = async (event: Event) => {
    event.preventDefault();
    setError(null);
    setSaving(true);
    try {
      await setSecret(name().trim(), value(), props.projectID, props.environmentID);
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
  deleting: boolean;
  error: string | null;
  onUpdate: (secret: Secret) => void;
  onDelete: (secret: Secret) => void;
}) {
  return (
    <tr>
      <td><code>{props.secret.name}</code></td>
      <td>{formatRelative(props.secret.updated_at)}</td>
      <td>{formatRelative(props.secret.created_at)}</td>
      <td class={ui.actionsCell}>
        <ActionMenu
          label={`Actions for ${props.secret.name}`}
          items={[
            {
              label: "Update",
              disabled: props.deleting,
              onSelect: () => props.onUpdate(props.secret),
            },
            {
              label: "Delete",
              busyLabel: props.deleting ? "Deleting..." : undefined,
              disabled: props.deleting,
              tone: "danger",
              onSelect: () => props.onDelete(props.secret),
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
  const [modalSecretName, setModalSecretName] = createSignal<string | null | undefined>(undefined);
  const [deletingName, setDeletingName] = createSignal<string | null>(null);
  const [deleteError, setDeleteError] = createSignal<{ name: string; message: string } | null>(null);
  const secrets = createQuery(() => ({
    queryKey: ["secrets", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () => listSecrets(scope.selectedProjectID(), scope.selectedEnvironmentID()),
    enabled: !!scope.selectedProjectID() && !!scope.selectedEnvironmentID(),
    retry: false,
  }));

  const invalidateSecrets = () => queryClient.invalidateQueries({ queryKey: ["secrets"] });

  const remove = async (secret: Secret) => {
    if (!window.confirm(`Delete secret "${secret.name}"?`)) return;
    setDeleteError(null);
    setDeletingName(secret.name);
    try {
      await deleteSecret(secret.name, scope.selectedProjectID(), scope.selectedEnvironmentID());
      await invalidateSecrets();
    } catch (removeError) {
      setDeleteError({ name: secret.name, message: secretErrorMessage(removeError) });
    } finally {
      setDeletingName(null);
    }
  };

  return (
    <>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Secrets</h1>
          <p class={ui.pageSubtitle}>Environment-scoped secret names for tasks. Values are never displayed after saving.</p>
        </div>
        <button class={ui.button} type="button" disabled={!scope.selectedEnvironmentID()} onClick={() => setModalSecretName(null)}>Set secret</button>
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
                  <th>Updated</th>
                  <th>Created</th>
                  <th><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                <For each={secrets.data?.secrets ?? []}>
                  {(secret) => (
                    <SecretRow
                      secret={secret}
                      deleting={deletingName() === secret.name}
                      error={deleteError()?.name === secret.name ? deleteError()?.message ?? null : null}
                      onUpdate={(secret) => setModalSecretName(secret.name)}
                      onDelete={remove}
                    />
                  )}
                </For>
              </tbody>
            </table>
          </div>
        </Show>
      </Show>

      <Show when={modalSecretName() !== undefined}>
        <SecretModal
          secretName={modalSecretName() ?? null}
          projectID={scope.selectedProjectID()}
          environmentID={scope.selectedEnvironmentID()}
          onClose={() => setModalSecretName(undefined)}
          onSaved={invalidateSecrets}
        />
      </Show>
    </>
  );
}
