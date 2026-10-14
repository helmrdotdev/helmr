import { slugify } from "../lib/slug";
import { useMe } from "../lib/me";
import { useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { defaultEnvironmentColor, ENVIRONMENT_COLOR_PRESETS, normalizeEnvironmentColor } from "../features/projects/display";
import { errorMessage } from "../lib/error";
import { hasPermission } from "../lib/auth";
import { createEnvironment, isProtectedEnvironment, updateEnvironment, type Environment } from "../lib/projects";
import { useScope } from "../lib/scope";
import { DataTable } from "../ui/DataTable";
import { EnvDot } from "../ui/EnvChip";
import { ColorField } from "../ui/Field";
import { IDText } from "../ui/IDText";
import { Dialog } from "../ui/Dialog";
import { PageHeader } from "../ui/PageHeader";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";



function EditEnvironmentModal(props: {
  env: Environment;
  onClose: () => void;
  onSaved: () => Promise<void>;
}) {
  const [name, setName] = createSignal(props.env.name);
  const [colorHex, setColorHex] = createSignal(normalizeEnvironmentColor(props.env.color_hex));
  const [submitting, setSubmitting] = createSignal(false);
  const [formError, setFormError] = createSignal<string | null>(null);

  const submit = async (event: SubmitEvent) => {
    event.preventDefault();
    const nextName = name().trim();
    if (!nextName) {
      setFormError("Name is required.");
      return;
    }
    setSubmitting(true);
    setFormError(null);
    try {
      await updateEnvironment(props.env.project, props.env.environment, { name: nextName, color_hex: colorHex() });
      await props.onSaved();
      props.onClose();
    } catch (error) {
      setFormError(errorMessage(error));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Dialog title={`Edit ${props.env.name}`} onClose={props.onClose} closeDisabled={submitting()}>
      <form onSubmit={submit}>
        <label class={ui.field}>
          <span>Name</span>
          <input
            type="text"
            class={ui.input}
            value={name()}
            onInput={(event) => setName(event.currentTarget.value)}
            autocomplete="off"
            autofocus
          />
        </label>
        <label class={ui.field}>
          <span>Environment ID</span>
          <input type="text" class={ui.input} value={props.env.environment} disabled aria-label="Environment ID (not editable)" />
        </label>
        <ColorField value={colorHex()} onChange={setColorHex} presets={ENVIRONMENT_COLOR_PRESETS} ariaLabel="Environment color" />
        <Show when={formError()}>
          <p class={ui.fieldError} role="alert">{formError()}</p>
        </Show>
        <div class={ui.modalActions}>
          <button type="button" class={ui.secondaryButton} disabled={submitting()} onClick={props.onClose}>
            Cancel
          </button>
          <button class={ui.button} type="submit" disabled={submitting() || !name().trim()}>
            {submitting() ? "Saving..." : "Save"}
          </button>
        </div>
      </form>
    </Dialog>
  );
}

function EnvironmentStatus(props: { env: Environment; selected: boolean }) {
  return (
    <span class="inline-flex flex-wrap items-center gap-1.5">
      <Show when={isProtectedEnvironment(props.env)}>
        <StatusBadge resource="environment" status="protected" />
      </Show>
      <Show when={props.selected}>
        <StatusBadge resource="environment" status="current" />
      </Show>
      <Show when={!isProtectedEnvironment(props.env) && !props.selected}>
        <span class="text-console-faint">—</span>
      </Show>
    </span>
  );
}

export function Environments() {
  const scope = useScope();
  const queryClient = useQueryClient();
  const me = useMe();
  const canManage = () => hasPermission(me.data, "projects.manage");
  const [creating, setCreating] = createSignal(false);
  const [editing, setEditing] = createSignal<Environment | null>(null);
  const [name, setName] = createSignal("");
  const [slug, setSlug] = createSignal("");
  const [slugTouched, setSlugTouched] = createSignal(false);
  const [colorHex, setColorHex] = createSignal(defaultEnvironmentColor(""));
  const [colorTouched, setColorTouched] = createSignal(false);
  const [submitting, setSubmitting] = createSignal(false);
  const [formError, setFormError] = createSignal<string | null>(null);

  const project = createMemo(() => scope.selectedProject());
  const environments = createMemo(() => project()?.environments ?? []);

  function openCreateEnvironment() {
    setName("");
    setSlug("");
    setSlugTouched(false);
    setColorHex(defaultEnvironmentColor(""));
    setColorTouched(false);
    setFormError(null);
    setSubmitting(false);
    setCreating(true);
  }

  function closeCreateEnvironment() {
    if (submitting()) return;
    setCreating(false);
    setFormError(null);
  }

  async function submitCreateEnvironment(event: SubmitEvent) {
    event.preventDefault();
    const currentProject = project();
    if (!currentProject) return;
    const nextName = name().trim();
    const nextSlug = slug().trim();
    if (!nextName || !nextSlug) {
      setFormError("Name and Environment ID are required.");
      return;
    }
    setSubmitting(true);
    setFormError(null);
    try {
      const env = await createEnvironment(currentProject.project, { name: nextName, environment: nextSlug, color_hex: colorHex() });
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
      scope.setSelectedEnvironmentID(env.environment);
      setCreating(false);
    } catch (error) {
      setFormError(errorMessage(error));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <>
      <PageHeader
        title="Environments"
        subtitle="Environments of the selected project. The current one scopes every other console page."
        actions={
          <button type="button" class={ui.button} disabled={!project()} onClick={openCreateEnvironment}>
            New environment
          </button>
        }
      />

      <Show when={project()} fallback={<StatePanel empty="No project selected." />}>
        <DataTable columns={["Name", "Environment ID", "Status", { label: "Actions", srOnly: true }]} minWidth="min-w-200">
          <For each={environments()}>
            {(env) => (
              <tr>
                <td>
                  <span class="inline-flex items-center gap-2.5 font-medium text-console-text">
                    <EnvDot colorHex={env.color_hex} />
                    {env.name}
                  </span>
                </td>
                <td><IDText value={env.environment} mode="full" /></td>
                <td><EnvironmentStatus env={env} selected={scope.selectedEnvironmentID() === env.environment} /></td>
                <td class={ui.actionsCell}>
                  <div class="flex justify-end gap-1.5">
                    <Show when={scope.selectedEnvironmentID() !== env.environment}>
                      <button type="button" class={ui.secondaryButton} onClick={() => scope.setSelectedEnvironmentID(env.environment)}>
                        Use
                      </button>
                    </Show>
                    <Show when={canManage()}>
                      <button type="button" class={ui.secondaryButton} onClick={() => setEditing(env)}>
                        Edit
                      </button>
                    </Show>
                  </div>
                </td>
              </tr>
            )}
          </For>
        </DataTable>
      </Show>

      <Show when={creating() && project()}>
        {(currentProject) => (
          <Dialog title={`New environment in ${currentProject().name}`} onClose={closeCreateEnvironment} closeDisabled={submitting()}>
            <form onSubmit={submitCreateEnvironment}>
              <label class={ui.field}>
                <span>Name</span>
                <input
                  type="text"
                  class={ui.input}
                  value={name()}
                  onInput={(event) => {
                    setName(event.currentTarget.value);
                    if (!slugTouched()) {
                      const nextSlug = slugify(event.currentTarget.value);
                      setSlug(nextSlug);
                      if (!colorTouched()) setColorHex(defaultEnvironmentColor(nextSlug));
                    }
                  }}
                  placeholder="Preview"
                  autocomplete="off"
                  autofocus
                />
              </label>
              <label class={ui.field}>
                <span>Environment ID</span>
                <input
                  type="text"
                  class={ui.input}
                  value={slug()}
                  onInput={(event) => {
                    setSlugTouched(true);
                    const nextSlug = event.currentTarget.value;
                    setSlug(nextSlug);
                    if (!colorTouched()) setColorHex(defaultEnvironmentColor(nextSlug));
                  }}
                  placeholder="preview"
                  autocomplete="off"
                  spellcheck={false}
                />
              </label>
              <ColorField
                presets={ENVIRONMENT_COLOR_PRESETS}
                ariaLabel="Environment color"
                value={colorHex()}
                onChange={(next) => {
                  setColorTouched(true);
                  setColorHex(next);
                }}
              />
              <Show when={formError()}>
                <p class={ui.fieldError} role="alert">{formError()}</p>
              </Show>
              <div class={ui.modalActions}>
                <button type="button" class={ui.secondaryButton} disabled={submitting()} onClick={closeCreateEnvironment}>
                  Cancel
                </button>
                <button class={ui.button} type="submit" disabled={submitting() || !name().trim() || !slug().trim()}>
                  {submitting() ? "Creating..." : "Create"}
                </button>
              </div>
            </form>
          </Dialog>
        )}
      </Show>

      <Show when={editing()}>
        {(env) => (
          <EditEnvironmentModal
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
