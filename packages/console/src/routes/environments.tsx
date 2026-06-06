import { useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { envTone } from "../features/projects/display";
import { ApiError } from "../lib/api";
import { createEnvironment, deleteEnvironment, type Environment } from "../lib/projects";
import { useScope } from "../lib/scope";
import { Modal } from "../ui/Modal";
import { envDotClass, ui } from "../ui/styles";

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
    <div class="flex flex-wrap items-center gap-1.5">
      <Show when={isProtectedEnvironment(props.env)}>
        <span class="border border-console-border bg-console-bg-panel px-1.5 py-0.5 font-mono text-[10px] font-medium uppercase tracking-[0.04em] text-console-subtle">
          Protected
        </span>
      </Show>
      <Show when={props.selected}>
        <span class="border border-console-accent bg-console-accent-soft px-1.5 py-0.5 font-mono text-[10px] font-medium uppercase tracking-[0.04em] text-console-accent">
          Current
        </span>
      </Show>
    </div>
  );
}

export function Environments() {
  const scope = useScope();
  const queryClient = useQueryClient();
  const [creating, setCreating] = createSignal(false);
  const [name, setName] = createSignal("");
  const [slug, setSlug] = createSignal("");
  const [slugTouched, setSlugTouched] = createSignal(false);
  const [submitting, setSubmitting] = createSignal(false);
  const [formError, setFormError] = createSignal<string | null>(null);

  const [environmentToDelete, setEnvironmentToDelete] = createSignal<Environment | null>(null);
  const [deleteConfirmation, setDeleteConfirmation] = createSignal("");
  const [deleteSubmitting, setDeleteSubmitting] = createSignal(false);
  const [deleteError, setDeleteError] = createSignal<string | null>(null);

  const project = createMemo(() => scope.selectedProject());
  const environments = createMemo(() => project()?.environments ?? []);

  function openCreateEnvironment() {
    setName("");
    setSlug("");
    setSlugTouched(false);
    setFormError(null);
    setSubmitting(false);
    setCreating(true);
  }

  function closeCreateEnvironment() {
    if (submitting()) return;
    setCreating(false);
    setFormError(null);
  }

  function openDeleteEnvironment(env: Environment) {
    if (isProtectedEnvironment(env)) return;
    setEnvironmentToDelete(env);
    setDeleteConfirmation("");
    setDeleteError(null);
  }

  function closeDeleteEnvironment() {
    if (deleteSubmitting()) return;
    setEnvironmentToDelete(null);
    setDeleteConfirmation("");
    setDeleteError(null);
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
      const env = await createEnvironment(currentProject.id, { name: nextName, slug: nextSlug });
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
      scope.setSelectedEnvironmentID(env.id);
      setCreating(false);
    } catch (error) {
      setFormError(formErrorMessage(error));
    } finally {
      setSubmitting(false);
    }
  }

  async function submitDeleteEnvironment(event: SubmitEvent) {
    event.preventDefault();
    const currentProject = project();
    const env = environmentToDelete();
    if (!currentProject || !env || deleteConfirmation().trim() !== env.slug) return;
    setDeleteSubmitting(true);
    setDeleteError(null);
    try {
      await deleteEnvironment(currentProject.id, env.id);
      if (scope.selectedEnvironmentID() === env.id) {
        scope.setSelectedEnvironmentID("");
      }
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
      setEnvironmentToDelete(null);
      setDeleteConfirmation("");
    } catch (error) {
      setDeleteError(formErrorMessage(error));
    } finally {
      setDeleteSubmitting(false);
    }
  }

  return (
    <>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Environments</h1>
        </div>
        <button type="button" class={ui.button} disabled={!project()} onClick={openCreateEnvironment}>
          New environment
        </button>
      </div>

      <Show
        when={project()}
        fallback={
          <div class={ui.emptyState}>
            <strong class="text-console-text">No project selected.</strong>
          </div>
        }
      >
        <div class={ui.tableWrap}>
          <table class={ui.dataTable}>
            <thead>
              <tr>
                <th>Environment</th>
                <th>Slug</th>
                <th>Status</th>
                <th>ID</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              <For each={environments()}>
                {(env) => (
                  <tr>
                    <td>
                      <div class={ui.tableCellStack}>
                        <div class="flex items-center gap-2.5 font-medium text-console-text">
                          <span class={envDotClass(envTone(env.slug))} />
                          {env.name}
                        </div>
                      </div>
                    </td>
                    <td><code>{env.slug}</code></td>
                    <td><EnvironmentStatus env={env} selected={scope.selectedEnvironmentID() === env.id} /></td>
                    <td><code>{env.id}</code></td>
                    <td class={ui.actionsCell}>
                      <div class="flex items-center justify-end gap-1.5">
                        <Show when={scope.selectedEnvironmentID() !== env.id}>
                          <button type="button" class={ui.secondaryButton} onClick={() => scope.setSelectedEnvironmentID(env.id)}>
                            Use
                          </button>
                        </Show>
                        <button
                          type="button"
                          class={ui.dangerOutlineButton}
                          disabled={isProtectedEnvironment(env)}
                          onClick={() => openDeleteEnvironment(env)}
                        >
                          Delete
                        </button>
                      </div>
                    </td>
                  </tr>
                )}
              </For>
            </tbody>
          </table>
        </div>
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
                    if (!slugTouched()) setSlug(slugify(event.currentTarget.value));
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
                    setSlug(event.currentTarget.value);
                  }}
                  placeholder="preview"
                  autocomplete="off"
                  spellcheck={false}
                />
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

      <Show when={environmentToDelete()}>
        {(env) => (
          <Modal title={`Delete ${env().name}`} onClose={closeDeleteEnvironment} closeDisabled={deleteSubmitting()}>
            <form onSubmit={submitDeleteEnvironment}>
              <p class={ui.warning}>
                This hard deletes the environment and the data scoped to it. Production and staging environments cannot be deleted.
              </p>
              <label class={ui.field}>
                <span>Type {env().slug} to confirm</span>
                <input
                  type="text"
                  class={ui.input}
                  value={deleteConfirmation()}
                  onInput={(event) => setDeleteConfirmation(event.currentTarget.value)}
                  autocomplete="off"
                  spellcheck={false}
                  autofocus
                />
              </label>
              <Show when={deleteError()}>
                <p class={ui.fieldError} role="alert">{deleteError()}</p>
              </Show>
              <div class={ui.modalActions}>
                <button type="button" class={ui.secondaryButton} disabled={deleteSubmitting()} onClick={closeDeleteEnvironment}>
                  Cancel
                </button>
                <button
                  class={ui.dangerButton}
                  type="submit"
                  disabled={deleteSubmitting() || deleteConfirmation().trim() !== env().slug}
                >
                  {deleteSubmitting() ? "Deleting..." : "Delete"}
                </button>
              </div>
            </form>
          </Modal>
        )}
      </Show>
    </>
  );
}
