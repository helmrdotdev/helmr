import { IDText } from "../ui/IDText";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { ApiError } from "../lib/api";
import { errorMessage } from "../lib/error";
import { createSecret, listSecrets, revokeSecret, rotateSecret, type Secret } from "../lib/secrets";
import { useScope } from "../lib/scope";
import { ActionMenu } from "../ui/ActionMenu";
import { DataTable } from "../ui/DataTable";
import { Dialog } from "../ui/Dialog";
import { PageHeader } from "../ui/PageHeader";
import { RelativeTime } from "../ui/RelativeTime";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";
import { createRowActions } from "../ui/rowActions";
import { EnvironmentTarget } from "../ui/EnvChip";

const SECRET_ERROR_MESSAGES: Record<string, string> = {
  forbidden: "You do not have permission to manage secrets.",
  not_found: "This secret no longer exists.",
};
const INTERNAL_ERROR_MESSAGE = "Something went wrong. Please try again.";

const secretErrorMessage = (error: unknown) => errorMessage(error, INTERNAL_ERROR_MESSAGE, SECRET_ERROR_MESSAGES);

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
  const [name, setName] = createSignal(props.secret?.secret ?? "");
  const [value, setValue] = createSignal("");
  const [saving, setSaving] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const save = async (event: Event) => {
    event.preventDefault();
    setError(null);
    setSaving(true);
    try {
      if (props.secret) {
        await rotateSecret(props.secret.secret, value(), props.projectID, props.environmentID);
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
    <Dialog title={editing() ? "Update secret" : "Set secret"} onClose={props.onClose} closeDisabled={saving()}>
      <form onSubmit={save}>
        <EnvironmentTarget
          label="Secret target environment"
          environmentName={props.environmentName}
          environmentColorHex={props.environmentColorHex}
          projectName={props.projectName}
          projectID={props.projectID}
          environmentID={props.environmentID}
        />
        <label class={ui.field}>
          <span>Secret ID</span>
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
    </Dialog>
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
      <td><IDText value={props.secret.secret} mode="full" /></td>
      <td><StatusBadge resource="secret" status={props.secret.status} /></td>
      <td><RelativeTime value={props.secret.rotated_at} fallback="never" /></td>
      <td><RelativeTime value={props.secret.created_at} /></td>
      <td class={ui.actionsCell}>
        <ActionMenu
          label={`Actions for ${props.secret.secret}`}
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
  const rows = createRowActions<"revoke">();
  const secrets = createQuery(() => ({
    queryKey: ["secrets", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () => listSecrets(scope.selectedProjectID(), scope.selectedEnvironmentID()),
    enabled: !!scope.selectedProjectID() && !!scope.selectedEnvironmentID(),
    retry: false,
  }));

  const invalidateSecrets = () => queryClient.invalidateQueries({ queryKey: ["secrets"] });

  const revoke = async (secret: Secret) => {
    if (!window.confirm(`Revoke secret "${secret.secret}"?\n\nAffected running Turns may be interrupted and fail to complete. Pending inputs are retained but cannot run on affected Computers.`)) return;
    await rows.run(secret.secret, "revoke", async () => {
      await revokeSecret(secret.secret, scope.selectedProjectID(), scope.selectedEnvironmentID());
      await invalidateSecrets();
    }, secretErrorMessage);
  };

  return (
    <>
      <PageHeader
        title="Secrets"
        subtitle="Environment-scoped Secrets for Agents and Computers. Values are never displayed after saving."
        actions={<button class={ui.button} type="button" disabled={!scope.selectedEnvironmentID()} onClick={() => setModalSecret(null)}>Set secret</button>}
      />

      <Show when={secrets.isError}>
        <StatePanel error={secrets.error instanceof ApiError ? secretErrorMessage(secrets.error) : "Could not load secrets."} />
      </Show>

      <Show when={!secrets.isPending} fallback={<StatePanel loading="Loading secrets..." />}>
        <Show when={(secrets.data?.secrets.length ?? 0) > 0} fallback={<StatePanel empty="No secrets found." />}>
          <DataTable columns={["Secret ID", "Status", "Rotated", "Created", { label: "Actions", srOnly: true }]} minWidth="min-w-225">
            <For each={secrets.data?.secrets ?? []}>
              {(secret) => (
                <SecretRow
                  secret={secret}
                  revoking={rows.busy(secret.secret)}
                  error={rows.error(secret.secret)}
                  onUpdate={setModalSecret}
                  onRevoke={revoke}
                />
              )}
            </For>
          </DataTable>
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
