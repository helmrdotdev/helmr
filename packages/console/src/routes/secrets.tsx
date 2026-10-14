import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createSignal, For, Show } from "solid-js";
import { environmentChip } from "../features/projects/display";
import { errorMessage } from "../lib/error";
import { createSecret, listSecrets, revokeSecret, rotateSecret, type Secret } from "../lib/secrets";
import { useScope } from "../lib/scope";
import { DataTable, RowActions } from "../ui/DataTable";
import { ConfirmDialog, FormDialog } from "../ui/Dialog";
import { EnvironmentTarget } from "../ui/EnvChip";
import { Field, SecretTextArea, TextInput } from "../ui/Field";
import { PageHeader } from "../ui/PageHeader";
import { QueryState } from "../ui/QueryState";
import { RelativeTime } from "../ui/RelativeTime";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";
import { useClipboard } from "../ui/useClipboard";

const SECRET_ERROR_MESSAGES: Record<string, string> = {
  forbidden: "You do not have permission to manage secrets.",
  not_found: "This secret no longer exists.",
};
const INTERNAL_ERROR_MESSAGE = "Something went wrong. Please try again.";

const secretErrorMessage = (error: unknown) => errorMessage(error, INTERNAL_ERROR_MESSAGE, SECRET_ERROR_MESSAGES);

function SecretDialog(props: {
  secret: Secret | null;
  projectID: string;
  environmentID: string;
  projectName: string;
  environmentName: string;
  environmentColorHex: string;
  onClose: () => void;
  onSaved: () => Promise<void>;
}) {
  const [name, setName] = createSignal(props.secret?.secret ?? "");
  const [value, setValue] = createSignal("");
  const [saving, setSaving] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const save = async () => {
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
    <FormDialog
      title={props.secret ? "Update secret" : "Set secret"}
      submitLabel="Save"
      busyLabel="Saving..."
      busy={saving()}
      disabled={name().trim() === ""}
      error={error()}
      onSubmit={() => void save()}
      onClose={props.onClose}
    >
      <EnvironmentTarget
        label="Secret target environment"
        environmentName={props.environmentName}
        environmentColorHex={props.environmentColorHex}
        projectName={props.projectName}
        projectID={props.projectID}
        environmentID={props.environmentID}
      />
      <Field label="Secret ID">
        <TextInput class="font-mono" value={name()} disabled={!!props.secret} autocomplete="off" spellcheck={false} autofocus={!props.secret} onInput={(event) => setName(event.currentTarget.value)} />
      </Field>
      <Field label="Value">
        <SecretTextArea value={value()} autofocus={!!props.secret} onInput={(event) => setValue(event.currentTarget.value)} />
      </Field>
    </FormDialog>
  );
}

function SecretRow(props: {
  secret: Secret;
  onUpdate: (secret: Secret) => void;
  onRevoke: (secret: Secret) => void;
  onCopy: (secret: Secret) => void;
}) {
  const active = () => props.secret.status !== "revoked";
  return (
    <tr>
      <td><code>{props.secret.secret}</code></td>
      <td><StatusBadge resource="secret" status={props.secret.status} /></td>
      <td><RelativeTime value={props.secret.rotated_at} fallback="never" /></td>
      <td><RelativeTime value={props.secret.created_at} /></td>
      <RowActions
        label={`Actions for ${props.secret.secret}`}
        items={[
          ...(active() ? [{ label: "Update", onSelect: () => props.onUpdate(props.secret) }] : []),
          { label: "Copy ID", onSelect: () => props.onCopy(props.secret) },
          ...(active() ? [{ label: "Revoke", tone: "danger" as const, onSelect: () => props.onRevoke(props.secret) }] : []),
        ]}
      />
    </tr>
  );
}

export function Secrets() {
  const scope = useScope();
  const queryClient = useQueryClient();
  const clipboard = useClipboard({ copied: "Secret ID copied", failed: "Could not copy the Secret ID" });
  const [modalSecret, setModalSecret] = createSignal<Secret | null | undefined>(undefined);
  const [revoking, setRevoking] = createSignal<Secret | null>(null);
  const secrets = createQuery(() => ({
    queryKey: ["secrets", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () => listSecrets(scope.selectedProjectID(), scope.selectedEnvironmentID()),
    enabled: !!scope.selectedProjectID() && !!scope.selectedEnvironmentID(),
    retry: false,
  }));

  const invalidateSecrets = () => queryClient.invalidateQueries({ queryKey: ["secrets"] });

  return (
    <>
      <PageHeader
        title="Secrets"
        subtitle="Environment-scoped Secrets for Agents and Computers. Values are never displayed after saving."
        actions={<button class={ui.button} type="button" disabled={!scope.selectedEnvironmentID()} onClick={() => setModalSecret(null)}>Set secret</button>}
      />

      <QueryState
        query={secrets}
        pending="Loading secrets..."
        error={(error) => errorMessage(error, "Could not load secrets.", SECRET_ERROR_MESSAGES)}
        empty={{ when: (data) => data.secrets.length === 0, title: "No secrets found." }}
      >
        {(data) => (
          <DataTable columns={["Secret ID", "Status", "Rotated", "Created", { label: "Actions", srOnly: true }]} minWidth="min-w-225">
            <For each={data().secrets}>
              {(secret) => (
                <SecretRow
                  secret={secret}
                  onUpdate={setModalSecret}
                  onRevoke={setRevoking}
                  onCopy={(current) => void clipboard.copy(current.secret)}
                />
              )}
            </For>
          </DataTable>
        )}
      </QueryState>
      <clipboard.LiveRegion />

      <Show when={modalSecret() !== undefined}>
        <SecretDialog
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

      <Show when={revoking()}>
        {(secret) => (
          <ConfirmDialog
            title="Revoke secret"
            confirmLabel="Revoke"
            busyLabel="Revoking..."
            tone="danger"
            target={secret().secret}
            environment={environmentChip(scope.selectedEnvironment())}
            errorMessage={secretErrorMessage}
            onClose={() => setRevoking(null)}
            onConfirm={async () => {
              await revokeSecret(secret().secret, scope.selectedProjectID(), scope.selectedEnvironmentID());
              await invalidateSecrets();
            }}
          >
            Affected running Turns may be interrupted and fail to complete. Pending inputs are retained but cannot run on affected Computers.
          </ConfirmDialog>
        )}
      </Show>
    </>
  );
}
