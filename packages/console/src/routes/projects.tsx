import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createSignal, For, Show } from "solid-js";
import { envTone } from "../features/projects/display";
import { ApiError } from "../lib/api";
import { listProjects, updateProject, type Environment, type Project } from "../lib/projects";
import { Modal } from "../ui/Modal";
import { envDotClass, ui } from "../ui/styles";

function projectsErrorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  return "Could not load projects.";
}

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

function EnvironmentBadge(props: { env: Environment }) {
  return (
    <span class="inline-flex items-center gap-1.5 border border-console-border bg-console-bg-panel px-2 py-1 text-xs font-medium text-console-text">
      <span class={envDotClass(envTone(props.env.slug))} />
      {props.env.name}
    </span>
  );
}

export function Projects() {
  const queryClient = useQueryClient();
  const [editingProject, setEditingProject] = createSignal<Project | null>(null);
  const [name, setName] = createSignal("");
  const [slug, setSlug] = createSignal("");
  const [slugTouched, setSlugTouched] = createSignal(false);
  const [submitting, setSubmitting] = createSignal(false);
  const [formError, setFormError] = createSignal<string | null>(null);

  const projects = createQuery(() => ({
    queryKey: ["projects"],
    queryFn: listProjects,
    retry: false,
  }));

  function openProject(project: Project) {
    setEditingProject(project);
    setName(project.name);
    setSlug(project.slug);
    setSlugTouched(true);
    setFormError(null);
  }

  function closeProject() {
    if (submitting()) return;
    setEditingProject(null);
    setFormError(null);
  }

  async function submitProject(event: SubmitEvent) {
    event.preventDefault();
    const project = editingProject();
    if (!project) return;
    const nextName = name().trim();
    const nextSlug = slug().trim();
    if (!nextName || !nextSlug) {
      setFormError("Name and slug are required.");
      return;
    }
    setSubmitting(true);
    setFormError(null);
    try {
      await updateProject(project.id, { name: nextName, slug: nextSlug });
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
      setEditingProject(null);
    } catch (error) {
      setFormError(formErrorMessage(error));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Projects</h1>
          <p class={ui.pageSubtitle}>
            Projects and environments define where deployments, runs, secrets, and workers live.
          </p>
        </div>
      </div>

      <Show when={projects.isError}>
        <p class={ui.error} role="alert">{projectsErrorMessage(projects.error)}</p>
      </Show>

      <Show when={!projects.isPending} fallback={<p class={ui.muted}>Loading projects…</p>}>
        <Show
          when={(projects.data?.projects.length ?? 0) > 0}
          fallback={
            <div class={ui.emptyState}>
              <strong class="text-console-text">No projects yet.</strong>
              <span>Use the scope switcher in the top bar to create one.</span>
            </div>
          }
        >
          <div class={ui.tableWrap}>
            <table class={ui.dataTable}>
              <thead>
                <tr>
                  <th>Project</th>
                  <th>Slug</th>
                  <th>Environments</th>
                  <th>ID</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                <For each={projects.data?.projects ?? []}>
                  {(project) => (
                    <tr>
                      <td class="font-medium text-console-text">{project.name}</td>
                      <td><code>{project.slug}</code></td>
                      <td>
                        <div class="inline-flex flex-wrap gap-1.5">
                          <For each={project.environments ?? []}>
                            {(env) => <EnvironmentBadge env={env} />}
                          </For>
                          <Show when={(project.environments ?? []).length === 0}>
                            <span class={ui.muted}>None</span>
                          </Show>
                        </div>
                      </td>
                      <td><code>{project.id}</code></td>
                      <td class={ui.actionsCell}>
                        <button type="button" class={ui.secondaryButton} onClick={() => openProject(project)}>
                          Edit
                        </button>
                      </td>
                    </tr>
                  )}
                </For>
              </tbody>
            </table>
          </div>
        </Show>
      </Show>

      <Show when={editingProject()}>
        {(project) => (
          <Modal
            title={`Edit ${project().name}`}
            onClose={closeProject}
            closeDisabled={submitting()}
          >
            <form onSubmit={submitProject}>
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
                  placeholder="My app"
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
                  placeholder="my-app"
                  autocomplete="off"
                  spellcheck={false}
                />
              </label>
              <Show when={formError()}>
                <p class={ui.fieldError} role="alert">{formError()}</p>
              </Show>
              <div class={ui.modalActions}>
                <button type="button" class={ui.secondaryButton} disabled={submitting()} onClick={closeProject}>
                  Cancel
                </button>
                <button class={ui.button} type="submit" disabled={submitting() || !name().trim() || !slug().trim()}>
                  {submitting() ? "Saving..." : "Save project"}
                </button>
              </div>
            </form>
          </Modal>
        )}
      </Show>
    </>
  );
}
