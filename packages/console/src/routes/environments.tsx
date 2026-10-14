import { useMe } from "../lib/me";
import { useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { ENVIRONMENT_COLOR_PRESETS, normalizeEnvironmentColor } from "../features/projects/display";
import { NewEnvironmentDialog } from "../features/projects/forms";
import { errorMessage } from "../lib/error";
import { hasPermission } from "../lib/auth";
import { isProtectedEnvironment, updateEnvironment, type Environment } from "../lib/projects";
import { useScope } from "../lib/scope";
import { DataTable, EmptyCell, RowActions } from "../ui/DataTable";
import { FormDialog } from "../ui/Dialog";
import { EnvChip } from "../ui/EnvChip";
import { ColorField, Field, TextInput } from "../ui/Field";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { EmptyState } from "../ui/QueryState";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";
import { useClipboard } from "../ui/useClipboard";

function EditEnvironmentDialog(props: {
  env: Environment;
  onClose: () => void;
  onSaved: () => Promise<void>;
}) {
  const [name, setName] = createSignal(props.env.name);
  const [colorHex, setColorHex] = createSignal(normalizeEnvironmentColor(props.env.color_hex));
  const [submitting, setSubmitting] = createSignal(false);
  const [formError, setFormError] = createSignal<string | null>(null);

  const submit = async () => {
    setSubmitting(true);
    setFormError(null);
    try {
      await updateEnvironment(props.env.project, props.env.environment, { name: name().trim(), color_hex: colorHex() });
      await props.onSaved();
      props.onClose();
    } catch (error) {
      setFormError(errorMessage(error));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <FormDialog
      title={`Edit ${props.env.name}`}
      submitLabel="Save"
      busyLabel="Saving..."
      busy={submitting()}
      disabled={!name().trim()}
      error={formError()}
      onSubmit={() => void submit()}
      onClose={props.onClose}
    >
      <Field label="Name">
        <TextInput value={name()} onInput={(event) => setName(event.currentTarget.value)} autocomplete="off" autofocus />
      </Field>
      <Field label="Environment ID">
        <TextInput class="font-mono" value={props.env.environment} disabled aria-label="Environment ID (not editable)" />
      </Field>
      <ColorField value={colorHex()} onChange={setColorHex} presets={ENVIRONMENT_COLOR_PRESETS} ariaLabel="Environment color" />
    </FormDialog>
  );
}

function EnvironmentStatus(props: { env: Environment; selected: boolean }) {
  return (
    <Show when={props.selected} fallback={<EmptyCell />}>
      <StatusBadge resource="environment" status="current" />
    </Show>
  );
}

export function Environments() {
  const scope = useScope();
  const queryClient = useQueryClient();
  const me = useMe();
  const clipboard = useClipboard({ copied: "Environment ID copied", failed: "Could not copy the Environment ID" });
  const canManage = () => hasPermission(me.data, "projects.manage");
  const [creating, setCreating] = createSignal(false);
  const [editing, setEditing] = createSignal<Environment | null>(null);

  const project = createMemo(() => scope.selectedProject());
  const environments = createMemo(() => project()?.environments ?? []);

  return (
    <>
      <PageHeader
        title="Environments"
        subtitle="Environments of the selected project. The current one scopes every other console page."
        actions={
          <button type="button" class={ui.button} disabled={!project()} onClick={() => setCreating(true)}>
            New environment
          </button>
        }
      />

      <Show when={project()} fallback={<EmptyState title="No project selected." />}>
        <DataTable columns={["Name", "Environment ID", "Default", "Status", { label: "Actions", srOnly: true }]} minWidth="min-w-200">
          <For each={environments()}>
            {(env) => (
              <tr>
                <td><EnvChip name={env.name} colorHex={env.color_hex} protected={isProtectedEnvironment(env)} /></td>
                <td><IDText value={env.environment} /></td>
                <td><Show when={env.is_default} fallback={<EmptyCell />}><StatusBadge resource="environment" status="default" /></Show></td>
                <td><EnvironmentStatus env={env} selected={scope.selectedEnvironmentID() === env.environment} /></td>
                <RowActions
                  label={`Actions for ${env.name}`}
                  items={[
                    ...(scope.selectedEnvironmentID() !== env.environment ? [{ label: "Use", onSelect: () => scope.setSelectedEnvironmentID(env.environment) }] : []),
                    ...(canManage() ? [{ label: "Edit", onSelect: () => setEditing(env) }] : []),
                    { label: "Copy ID", onSelect: () => void clipboard.copy(env.environment) },
                  ]}
                />
              </tr>
            )}
          </For>
        </DataTable>
        <clipboard.LiveRegion />
      </Show>

      <Show when={creating() && project()}>
        {(currentProject) => (
          <NewEnvironmentDialog
            project={currentProject()}
            onClose={() => setCreating(false)}
            onCreated={async (env) => {
              await queryClient.invalidateQueries({ queryKey: ["projects"] });
              scope.setSelectedEnvironmentID(env.environment);
              setCreating(false);
            }}
          />
        )}
      </Show>

      <Show when={editing()}>
        {(env) => (
          <EditEnvironmentDialog
            env={env()}
            onClose={() => setEditing(null)}
            onSaved={async () => {
              await queryClient.invalidateQueries({ queryKey: ["projects"] });
            }}
          />
        )}
      </Show>
    </>
  );
}
