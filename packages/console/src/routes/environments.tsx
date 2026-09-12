import { useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { defaultEnvironmentColor, ENVIRONMENT_COLOR_PRESETS, normalizeEnvironmentColor } from "../features/projects/display";
import { ApiError } from "../lib/api";
import { createEnvironment, type Environment } from "../lib/projects";
import { useScope } from "../lib/scope";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { Modal } from "../ui/Modal";
import { PageHeader } from "../ui/PageHeader";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { envDotStyle, ui } from "../ui/styles";

const PROTECTED_ENVIRONMENT_SLUGS = new Set(["production", "staging"]);

function slugify(value: string): string {
  return value
    .toLowerCase()
    .trim()
    .replace(/[^a-z0-9\s-]/g, "")
    .replace(/\s+/g, "-")
    .replace(/-+/g, "-")
    .replace(/^-|-$/g, "")
    .slice(0, 48)
    .replace(/^-|-$/g, "");
}

function formErrorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  return "Something went wrong.";
}

function isProtectedEnvironment(env: Environment): boolean {
  return PROTECTED_ENVIRONMENT_SLUGS.has(env.slug);
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
  const [creating, setCreating] = createSignal(false);
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
      setFormError("Name and slug are required.");
      return;
    }
    setSubmitting(true);
    setFormError(null);
    try {
      const env = await createEnvironment(currentProject.id, { name: nextName, slug: nextSlug, color_hex: colorHex() });
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
      scope.setSelectedEnvironmentID(env.id);
      setCreating(false);
    } catch (error) {
      setFormError(formErrorMessage(error));
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
        <DataTable columns={["Environment", "Slug", "Status", "ID", { label: "Actions", srOnly: true }]} minWidth="min-w-200">
          <For each={environments()}>
            {(env) => (
              <tr>
                <td>
                  <span class="inline-flex items-center gap-2.5 font-medium text-console-text">
                    <span class="inline-block size-1.5 shrink-0 rounded-full" style={envDotStyle(env.color_hex)} />
                    {env.name}
                  </span>
                </td>
                <td><code>{env.slug}</code></td>
                <td><EnvironmentStatus env={env} selected={scope.selectedEnvironmentID() === env.id} /></td>
                <td><IDText value={env.id} /></td>
                <td class={ui.actionsCell}>
                  <Show when={scope.selectedEnvironmentID() !== env.id}>
                    <button type="button" class={ui.secondaryButton} onClick={() => scope.setSelectedEnvironmentID(env.id)}>
                      Use
                    </button>
                  </Show>
                </td>
              </tr>
            )}
          </For>
        </DataTable>
      </Show>

      <Show when={creating() && project()}>
        {(currentProject) => (
          <Modal title={`New environment in ${currentProject().name}`} onClose={closeCreateEnvironment} closeDisabled={submitting()}>
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
                <span>Slug</span>
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
              <label class={ui.field}>
                <span>Color</span>
                <div class="flex items-center gap-2">
                  <input
                    type="color"
                    class="size-8 cursor-pointer border border-console-border bg-white p-0.5"
                    value={colorHex()}
                    onInput={(event) => {
                      setColorTouched(true);
                      setColorHex(normalizeEnvironmentColor(event.currentTarget.value));
                    }}
                    aria-label="Environment color"
                  />
                  <div class="flex flex-wrap gap-1.5">
                    <For each={ENVIRONMENT_COLOR_PRESETS}>
                      {(preset) => (
                        <button
                          type="button"
                          class="size-6 cursor-pointer border border-console-border bg-white p-0.5"
                          aria-label={`Use ${preset}`}
                          onClick={() => {
                            setColorTouched(true);
                            setColorHex(preset);
                          }}
                        >
                          <span class="block size-full" style={{ "background-color": preset }} />
                        </button>
                      )}
                    </For>
                  </div>
                </div>
              </label>
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
          </Modal>
        )}
      </Show>
    </>
  );
}
