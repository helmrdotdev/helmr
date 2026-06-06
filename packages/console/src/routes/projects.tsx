import { useQueryClient } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, Show } from "solid-js";
import { ApiError } from "../lib/api";
import { deleteProject, updateProject } from "../lib/projects";
import { useScope } from "../lib/scope";
import { Modal } from "../ui/Modal";
import { ui } from "../ui/styles";

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

export function Projects() {
  const scope = useScope();
  const queryClient = useQueryClient();
  const [name, setName] = createSignal("");
  const [slug, setSlug] = createSignal("");
  const [slugTouched, setSlugTouched] = createSignal(false);
  const [saving, setSaving] = createSignal(false);
  const [saveError, setSaveError] = createSignal<string | null>(null);

  const [deleting, setDeleting] = createSignal(false);
  const [deleteConfirmation, setDeleteConfirmation] = createSignal("");
  const [deleteSubmitting, setDeleteSubmitting] = createSignal(false);
  const [deleteError, setDeleteError] = createSignal<string | null>(null);

  const project = createMemo(() => scope.selectedProject());
  const hasProject = createMemo(() => !!project());
  const dirty = createMemo(() => {
    const current = project();
    if (!current) return false;
    return name().trim() !== current.name || slug().trim() !== current.slug;
  });

  createEffect(() => {
    const current = project();
    setName(current?.name ?? "");
    setSlug(current?.slug ?? "");
    setSlugTouched(true);
    setSaveError(null);
  });

  async function submitProject(event: SubmitEvent) {
    event.preventDefault();
    const current = project();
    if (!current) return;
    const nextName = name().trim();
    const nextSlug = slug().trim();
    if (!nextName || !nextSlug) {
      setSaveError("Name and slug are required.");
      return;
    }
    setSaving(true);
    setSaveError(null);
    try {
      await updateProject(current.id, { name: nextName, slug: nextSlug });
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
    } catch (error) {
      setSaveError(formErrorMessage(error));
    } finally {
      setSaving(false);
    }
  }

  function openDeleteProject() {
    setDeleteConfirmation("");
    setDeleteError(null);
    setDeleting(true);
  }

  function closeDeleteProject() {
    if (deleteSubmitting()) return;
    setDeleting(false);
    setDeleteConfirmation("");
    setDeleteError(null);
  }

  async function submitDeleteProject(event: SubmitEvent) {
    event.preventDefault();
    const current = project();
    if (!current || deleteConfirmation().trim() !== current.slug) return;
    setDeleteSubmitting(true);
    setDeleteError(null);
    try {
      await deleteProject(current.id);
      scope.setSelectedProjectID("");
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
      setDeleting(false);
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
          <h1 class={ui.h1}>Project</h1>
        </div>
      </div>

      <Show
        when={hasProject()}
        fallback={
          <div class={ui.emptyState}>
            <strong class="text-console-text">No project selected.</strong>
          </div>
        }
      >
        <form class="max-w-220 border border-console-border-strong bg-console-surface px-4 py-4" onSubmit={submitProject}>
          <h2 class={ui.h2}>Profile</h2>
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
              autocomplete="off"
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
              autocomplete="off"
              spellcheck={false}
            />
          </label>
          <Show when={saveError()}>
            <p class={ui.fieldError} role="alert">{saveError()}</p>
          </Show>
          <div class={ui.actionRow}>
            <button
              class={ui.button}
              type="submit"
              disabled={saving() || !dirty() || !name().trim() || !slug().trim()}
            >
              {saving() ? "Saving..." : "Save changes"}
            </button>
          </div>
        </form>

        <section class="mt-5 max-w-220 border border-[#e6aaa4] bg-[#fffafa] px-4 py-4">
          <h2 class={ui.h2}>Danger zone</h2>
          <p class="mt-1.5 text-[12.5px] leading-normal text-console-muted">
            Delete the current project and all environments, deployments, runs, secrets, schedules, and scoped grants.
          </p>
          <div class={ui.actionRow}>
            <button type="button" class={ui.dangerOutlineButton} onClick={openDeleteProject}>
              Delete
            </button>
          </div>
        </section>
      </Show>

      <Show when={deleting() && project()}>
        {(current) => (
          <Modal title={`Delete ${current().name}`} onClose={closeDeleteProject} closeDisabled={deleteSubmitting()}>
            <form onSubmit={submitDeleteProject}>
              <p class={ui.warning}>
                This hard deletes the project. If another project exists, Helmr will select the next available project.
              </p>
              <label class={ui.field}>
                <span>Type {current().slug} to confirm</span>
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
                <button type="button" class={ui.secondaryButton} disabled={deleteSubmitting()} onClick={closeDeleteProject}>
                  Cancel
                </button>
                <button
                  class={ui.dangerButton}
                  type="submit"
                  disabled={deleteSubmitting() || deleteConfirmation().trim() !== current().slug}
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
