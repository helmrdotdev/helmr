import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createSignal, For, Show } from "solid-js";
import { formatRelative } from "../features/runs/display";
import { ApiError } from "../lib/api";
import { listSecrets, setSecret } from "../lib/secrets";
import { useScope } from "../lib/scope";
import { Modal } from "../ui/Modal";
import { ui } from "../ui/styles";

function secretErrorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  return "Could not load secrets.";
}

function SecretModal(props: {
  projectID: string;
  environmentID: string;
  onClose: () => void;
  onSaved: () => Promise<void>;
}) {
  const [name, setName] = createSignal("");
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
    <Modal title="Set secret" onClose={props.onClose} closeDisabled={saving()}>
      <form onSubmit={save}>
        <label class={ui.field}>
          <span>Name</span>
          <input
            class={ui.input}
            value={name()}
            autocomplete="off"
            autofocus
            onInput={(event) => setName(event.currentTarget.value)}
          />
        </label>
        <label class={ui.field}>
          <span>Value</span>
          <textarea class={ui.textarea} value={value()} onInput={(event) => setValue(event.currentTarget.value)} />
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

export function Secrets() {
  const scope = useScope();
  const queryClient = useQueryClient();
  const [modalOpen, setModalOpen] = createSignal(false);
  const secrets = createQuery(() => ({
    queryKey: ["secrets", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () => listSecrets(scope.selectedProjectID(), scope.selectedEnvironmentID()),
    enabled: !!scope.selectedProjectID() && !!scope.selectedEnvironmentID(),
    retry: false,
  }));

  const invalidateSecrets = () => queryClient.invalidateQueries({ queryKey: ["secrets"] });

  return (
    <>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Secrets</h1>
          <p class={ui.pageSubtitle}>Environment-scoped secret names for tasks. Values are never displayed after saving.</p>
        </div>
        <button class={ui.button} type="button" disabled={!scope.selectedEnvironmentID()} onClick={() => setModalOpen(true)}>Set secret</button>
      </div>

      <Show when={secrets.isError}>
        <p class={ui.error} role="alert">{secretErrorMessage(secrets.error)}</p>
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
                </tr>
              </thead>
              <tbody>
                <For each={secrets.data?.secrets ?? []}>
                  {(secret) => (
                    <tr>
                      <td><code>{secret.name}</code></td>
                      <td>{formatRelative(secret.updated_at)}</td>
                      <td>{formatRelative(secret.created_at)}</td>
                    </tr>
                  )}
                </For>
              </tbody>
            </table>
          </div>
        </Show>
      </Show>

      <Show when={modalOpen()}>
        <SecretModal
          projectID={scope.selectedProjectID()}
          environmentID={scope.selectedEnvironmentID()}
          onClose={() => setModalOpen(false)}
          onSaved={invalidateSecrets}
        />
      </Show>
    </>
  );
}
