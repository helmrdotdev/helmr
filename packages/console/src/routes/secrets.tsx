import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createSignal, For, Show } from "solid-js";
import { environmentChip } from "../features/projects/display";
import { errorMessage, loadError } from "../lib/error";
import { createSecret, listSecrets, revokeSecret, rotateSecret, type Secret } from "../lib/secrets";
import { EnvironmentSelectorLayout, useEditedEnvironment, type EditTarget } from "../components/EnvironmentSelector";
import { DataTable, RowActions } from "../ui/DataTable";
import { ConfirmDialog, FormDialog } from "../ui/Dialog";
import { EnvironmentTarget } from "../ui/EnvChip";
import { Field, SecretTextArea, TextInput } from "../ui/Field";
import { PageHeader } from "../ui/PageHeader";
import { QueryState } from "../ui/QueryState";
import { TableSkeleton } from "../ui/Skeleton";
import { RelativeTime } from "../ui/RelativeTime";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";
import { useClipboard } from "../ui/useClipboard";

const COLUMNS = ["Secret ID", "Status", "Rotated", "Created", { label: "Actions", srOnly: true }] as const;

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
  const edited = useEditedEnvironment();
  const queryClient = useQueryClient();
  const clipboard = useClipboard({ copied: "Secret ID copied", failed: "Could not copy the secret ID" });
  const [editing, setEditing] = createSignal<{ secret: Secret | null; target: EditTarget }>();
  const [revoking, setRevoking] = createSignal<{ secret: Secret; target: EditTarget }>();
  const edit = (secret: Secret | null) => {
    const target = edited.target();
    if (target) setEditing({ secret, target });
  };
  const revoke = (secret: Secret) => {
    const target = edited.target();
    if (target) setRevoking({ secret, target });
  };
  const secrets = createQuery(() => ({
    queryKey: ["secrets", edited.projectID(), edited.environmentID()],
    queryFn: () => listSecrets(edited.projectID(), edited.environmentID()),
    enabled: !!edited.projectID() && !!edited.environmentID(),
    retry: false,
  }));

  const invalidateSecrets = () => queryClient.invalidateQueries({ queryKey: ["secrets"] });

  return (
    <>
      <PageHeader
        title="Secrets"
        subtitle="Values are never displayed after saving."
        actions={
          <button class={ui.button} type="button" disabled={!edited.environment()} onClick={() => edit(null)}>
            {edited.environment() ? `Set secret in ${edited.environment()!.name}` : "Set secret"}
          </button>
        }
      />

      <EnvironmentSelectorLayout edited={edited}>
        <QueryState
          query={secrets}
          pending={<TableSkeleton label="Loading secrets" columns={COLUMNS} minWidth="min-w-160" />}
          error={(error) => loadError(error, "Could not load secrets.", SECRET_ERROR_MESSAGES)}
          empty={{ when: (data) => data.secrets.length === 0, title: "No secrets found." }}
        >
          {(data) => (
            <DataTable columns={COLUMNS} minWidth="min-w-160">
              <For each={data().secrets}>
                {(secret) => (
                  <SecretRow
                    secret={secret}
                    onUpdate={edit}
                    onRevoke={revoke}
                    onCopy={(current) => void clipboard.copy(current.secret)}
                  />
                )}
              </For>
            </DataTable>
          )}
        </QueryState>
      </EnvironmentSelectorLayout>
      <clipboard.LiveRegion />

      <Show when={editing()}>
        {(current) => (
          <SecretDialog
            secret={current().secret}
            projectID={current().target.projectID}
            environmentID={current().target.environment.environment}
            projectName={current().target.projectName}
            environmentName={current().target.environment.name}
            environmentColorHex={environmentChip(current().target.environment)?.colorHex ?? ""}
            onClose={() => setEditing(undefined)}
            onSaved={invalidateSecrets}
          />
        )}
      </Show>

      <Show when={revoking()}>
        {(current) => (
          <ConfirmDialog
            title="Revoke secret"
            confirmLabel="Revoke"
            busyLabel="Revoking..."
            tone="danger"
            target={current().secret.secret}
            environment={environmentChip(current().target.environment)}
            errorMessage={secretErrorMessage}
            onClose={() => setRevoking(undefined)}
            onConfirm={async () => {
              const { secret, target } = current();
              await revokeSecret(secret.secret, target.projectID, target.environment.environment);
              await invalidateSecrets();
            }}
          >
            Affected running turns may be interrupted and fail to complete. Pending inputs are retained but cannot run on affected computers.
          </ConfirmDialog>
        )}
      </Show>
    </>
  );
}
