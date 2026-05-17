import { useQueryClient } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, onCleanup, onMount, Show } from "solid-js";
import { envTone } from "../features/projects/display";
import { ApiError } from "../lib/api";
import { createEnvironment, createProject } from "../lib/projects";
import { useScope } from "../lib/scope";
import { Modal } from "../ui/Modal";
import { cx, envDotClass, ui } from "../ui/styles";

type CreateMode = "project" | "environment" | null;

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

function createErrorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  return "Something went wrong.";
}

export function ScopeSwitcher() {
  const scope = useScope();
  const queryClient = useQueryClient();
  const [open, setOpen] = createSignal(false);
  const [mobileMenuStyle, setMobileMenuStyle] = createSignal<Record<string, string> | undefined>();
  const [projectQuery, setProjectQuery] = createSignal("");
  const [envQuery, setEnvQuery] = createSignal("");
  const [hoverProjectID, setHoverProjectID] = createSignal<string | null>(null);

  const [creating, setCreating] = createSignal<CreateMode>(null);
  const [formName, setFormName] = createSignal("");
  const [formSlug, setFormSlug] = createSignal("");
  const [slugTouched, setSlugTouched] = createSignal(false);
  const [submitting, setSubmitting] = createSignal(false);
  const [formError, setFormError] = createSignal<string | null>(null);

  let wrapperRef: HTMLDivElement | undefined;
  let projectSearchRef: HTMLInputElement | undefined;
  let formNameRef: HTMLInputElement | undefined;

  const browsedProjectID = createMemo(() => hoverProjectID() ?? scope.selectedProjectID());
  const browsedProject = createMemo(() =>
    scope.projects().find((project) => project.id === browsedProjectID()),
  );

  const filteredProjects = createMemo(() => {
    const q = projectQuery().trim().toLowerCase();
    if (!q) return scope.projects();
    return scope.projects().filter((project) =>
      project.name.toLowerCase().includes(q) || project.slug.toLowerCase().includes(q),
    );
  });

  const filteredEnvs = createMemo(() => {
    const envs = browsedProject()?.environments ?? [];
    const q = envQuery().trim().toLowerCase();
    if (!q) return envs;
    return envs.filter((env) =>
      env.name.toLowerCase().includes(q) || env.slug.toLowerCase().includes(q),
    );
  });

  onMount(() => {
    const onMouseDown = (event: MouseEvent) => {
      if (creating()) return;
      if (!wrapperRef) return;
      if (!wrapperRef.contains(event.target as Node)) closeAll();
    };
    const onKey = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      if (creating()) {
        if (document.querySelector('[role="listbox"]')) return;
        cancelForm();
        event.stopPropagation();
        return;
      }
      if (open()) {
        setOpen(false);
      }
    };
    const onViewportChange = () => {
      if (open()) updateMenuPosition();
    };
    document.addEventListener("mousedown", onMouseDown);
    document.addEventListener("keydown", onKey);
    window.addEventListener("resize", onViewportChange);
    window.addEventListener("scroll", onViewportChange, true);
    onCleanup(() => {
      document.removeEventListener("mousedown", onMouseDown);
      document.removeEventListener("keydown", onKey);
      window.removeEventListener("resize", onViewportChange);
      window.removeEventListener("scroll", onViewportChange, true);
    });
  });

  createEffect(() => {
    if (open()) {
      updateMenuPosition();
    } else {
      setMobileMenuStyle(undefined);
    }
  });

  createEffect(() => {
    const name = formName();
    if (!slugTouched()) {
      setFormSlug(slugify(name));
    }
  });

  function closeAll() {
    setOpen(false);
    setCreating(null);
    setHoverProjectID(null);
  }

  function resetForm() {
    setFormName("");
    setFormSlug("");
    setSlugTouched(false);
    setFormError(null);
    setSubmitting(false);
  }

  function cancelForm() {
    setCreating(null);
    resetForm();
  }

  function updateMenuPosition() {
    if (!wrapperRef || window.innerWidth > 720) {
      setMobileMenuStyle(undefined);
      return;
    }
    const rect = wrapperRef.getBoundingClientRect();
    const top = Math.max(8, Math.min(rect.bottom + 6, window.innerHeight - 80));
    setMobileMenuStyle({
      position: "fixed",
      top: `${top}px`,
      left: "10px",
      right: "10px",
      width: "auto",
      "max-height": `calc(100dvh - ${top + 10}px)`,
      "overflow-y": "auto",
    });
  }

  function startCreateProject() {
    resetForm();
    setCreating("project");
    setOpen(false);
    queueMicrotask(() => formNameRef?.focus());
  }

  function startCreateEnvironment() {
    if (!browsedProject()) return;
    resetForm();
    setCreating("environment");
    setOpen(false);
    queueMicrotask(() => formNameRef?.focus());
  }

  const toggle = () => {
    if (scope.isLoading()) return;
    const next = !open();
    if (next) {
      setProjectQuery("");
      setEnvQuery("");
      setHoverProjectID(null);
      setCreating(null);
      resetForm();
      setOpen(true);
      queueMicrotask(() => projectSearchRef?.focus());
    } else {
      closeAll();
    }
  };

  const browseProject = (id: string) => {
    if (creating()) return;
    setHoverProjectID(id);
  };

  const pickEnv = (envID: string) => {
    const browsedID = browsedProjectID();
    if (browsedID && browsedID !== scope.selectedProjectID()) {
      scope.setSelectedProjectID(browsedID);
    }
    scope.setSelectedEnvironmentID(envID);
    closeAll();
  };

  async function submitProject(event: Event) {
    event.preventDefault();
    const name = formName().trim();
    const slug = formSlug().trim();
    if (!name || !slug) {
      setFormError("Name and slug are required.");
      return;
    }
    setFormError(null);
    setSubmitting(true);
    try {
      const project = await createProject({ name, slug });
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
      setHoverProjectID(project.id);
      resetForm();
      setCreating("environment");
      queueMicrotask(() => formNameRef?.focus());
    } catch (error) {
      setFormError(createErrorMessage(error));
    } finally {
      setSubmitting(false);
    }
  }

  async function submitEnvironment(event: Event) {
    event.preventDefault();
    const browsedID = browsedProjectID();
    if (!browsedID) {
      setFormError("Pick a project first.");
      return;
    }
    const name = formName().trim();
    const slug = formSlug().trim();
    if (!name || !slug) {
      setFormError("Name and slug are required.");
      return;
    }
    setFormError(null);
    setSubmitting(true);
    try {
      const env = await createEnvironment(browsedID, { name, slug });
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
      if (browsedID !== scope.selectedProjectID()) {
        scope.setSelectedProjectID(browsedID);
      }
      scope.setSelectedEnvironmentID(env.id);
      resetForm();
      setCreating(null);
      setOpen(false);
      setHoverProjectID(null);
    } catch (error) {
      setFormError(createErrorMessage(error));
    } finally {
      setSubmitting(false);
    }
  }

  const selectedEnvSlug = createMemo(() => scope.selectedEnvironment()?.slug);
  const triggerDisabled = createMemo(() => scope.isLoading());
  const hasNoProjects = createMemo(() => !scope.isLoading() && scope.projects().length === 0);
  const searchInputClass =
    "h-6 w-full border border-console-border bg-white px-2 py-0 font-mono text-[11px] text-console-text outline-none transition placeholder:text-console-faint focus:border-console-accent focus:shadow-[0_0_0_2px_rgb(49_95_206/0.12)]";
  const createActionClass =
    "flex h-7 w-full cursor-pointer items-center gap-1.5 border-0 bg-transparent px-2 text-left font-mono text-[11px] font-medium text-console-muted transition-colors hover:bg-white hover:text-console-text disabled:cursor-not-allowed disabled:opacity-45";
  const createIconClass =
    "grid size-4 flex-shrink-0 place-items-center border border-console-border bg-white text-[13px] font-medium leading-none text-console-muted";
  return (
    <div class={"relative min-w-0"} ref={wrapperRef}>
      <button
        type="button"
        class={ui.scopeTrigger}
        data-open={open() ? "true" : "false"}
        disabled={triggerDisabled()}
        aria-haspopup="dialog"
        aria-expanded={open()}
        onClick={toggle}
      >
        <Show
          when={!hasNoProjects()}
          fallback={<span class={"max-w-37.5 overflow-hidden text-ellipsis whitespace-nowrap font-medium text-console-text max-sm:max-w-27.5"}>No projects yet</span>}
        >
          <span class={"text-console-muted"}>Project</span>
          <span class={"max-w-32.5 overflow-hidden text-ellipsis whitespace-nowrap font-medium text-console-text max-sm:max-w-23"}>{scope.selectedProject()?.name ?? "—"}</span>
          <span class={"text-console-faint"}>/</span>
          <span class={"text-console-muted"}>Env</span>
          <span class={"flex max-w-32.5 items-center gap-1 overflow-hidden text-ellipsis whitespace-nowrap font-medium text-console-text max-sm:max-w-23"}>
            <span class={envDotClass(envTone(selectedEnvSlug()))} />
            {scope.selectedEnvironment()?.name ?? "—"}
          </span>
        </Show>
        <span
          class={cx(
            "ml-0.5 grid size-4 place-items-center transition-transform",
            open() && "rotate-180",
          )}
          aria-hidden="true"
        >
          <span class={"block size-1.5 -translate-y-px rotate-45 border-b border-r border-console-muted"} />
        </span>
        <Show when={scope.isLoading()}>
          <span class={"ml-1.5 border border-[#e5c26e] bg-[#fff7df] px-1.5 py-0.5 text-[10px] font-medium text-console-warning"}>Loading…</span>
        </Show>
      </button>

      <Show when={open()}>
        <div
          class={"absolute left-0 top-[calc(100%+7px)] z-50 w-110 overflow-hidden border border-console-border-strong bg-console-surface shadow-[2px_2px_0_rgb(15_23_42/0.12)] max-[720px]:w-auto"}
          style={mobileMenuStyle()}
          role="dialog"
          aria-label="Choose project and environment"
        >
          <div class={"flex h-5.5 items-center justify-between border-b border-console-border bg-[linear-gradient(to_bottom,#f8f8f8,#eceff2)] px-2 font-mono text-[10.5px] text-console-text"}>
            <span>Switch scope</span>
            <span class={"text-console-subtle"}>Project / Env</span>
          </div>
          <div class={"grid grid-cols-2 max-[720px]:grid-cols-1"}>
            <div class={"flex min-h-47 min-w-0 flex-col border-console-border even:border-l max-[720px]:even:border-l-0 max-[720px]:even:border-t"}>
              <div class={"flex flex-col gap-1.5 border-b border-console-border bg-console-bg-panel p-1.5"}>
                <span class={"font-mono text-[10px] font-medium uppercase tracking-[0.06em] text-console-subtle"}>Projects</span>
                <input
                  ref={projectSearchRef}
                  class={searchInputClass}
                  type="text"
                  placeholder="Find a project…"
                  value={projectQuery()}
                  onInput={(event) => setProjectQuery(event.currentTarget.value)}
                  autocomplete="off"
                  spellcheck={false}
                />
              </div>
              <div class={"flex max-h-54 flex-1 flex-col gap-0.5 overflow-y-auto p-1"}>
                <Show
                  when={filteredProjects().length > 0}
                  fallback={<div class={"grid flex-1 place-items-center px-3 py-5 text-[12px] text-console-subtle"}>No matches</div>}
                >
                  <For each={filteredProjects()}>
                    {(project) => (
                      <button
                        type="button"
                        class={cx(
                          ui.scopeItem,
                          project.id === browsedProjectID() && ui.scopeItemPreviewed,
                          project.id === scope.selectedProjectID() && ui.scopeItemSelected,
                        )}
                        onMouseEnter={() => browseProject(project.id)}
                        onFocus={() => browseProject(project.id)}
                        onClick={() => browseProject(project.id)}
                      >
                        <span class={cx(envDotClass("neutral"), "invisible")} />
                        <span class={"overflow-hidden text-ellipsis whitespace-nowrap font-medium"}>{project.name}</span>
                        <Show
                          when={project.id === scope.selectedProjectID()}
                          fallback={<span class={"font-mono text-[11px] text-console-subtle"}>{project.slug}</span>}
                        >
                          <span class={"text-[12px] font-medium leading-none text-console-accent"} aria-label="current">✓</span>
                        </Show>
                      </button>
                    )}
                  </For>
                </Show>
              </div>
              <div class={"border-t border-console-border bg-console-bg-panel p-1"}>
                <button type="button" class={createActionClass} onClick={startCreateProject}>
                  <span class={createIconClass} aria-hidden="true">+</span>
                  <span class={"truncate"}>New project</span>
                </button>
              </div>
            </div>

            <div class={"flex min-h-47 min-w-0 flex-col border-console-border even:border-l max-[720px]:even:border-l-0 max-[720px]:even:border-t"}>
              <div class={"flex flex-col gap-1.5 border-b border-console-border bg-console-bg-panel p-1.5"}>
                <span class={"font-mono text-[10px] font-medium uppercase tracking-[0.06em] text-console-subtle"}>
                  Environments
                  <Show when={browsedProject() && browsedProject()!.id !== scope.selectedProjectID()}>
                    <span class={"ml-1.5 normal-case tracking-normal text-console-subtle"}>
                      in {browsedProject()!.name}
                    </span>
                  </Show>
                </span>
                <input
                  class={searchInputClass}
                  type="text"
                  placeholder="Find an environment…"
                  value={envQuery()}
                  onInput={(event) => setEnvQuery(event.currentTarget.value)}
                  autocomplete="off"
                  spellcheck={false}
                />
              </div>
              <div class={"flex max-h-54 flex-1 flex-col gap-0.5 overflow-y-auto p-1"}>
                <Show
                  when={filteredEnvs().length > 0}
                  fallback={
                    <div class={"grid flex-1 place-items-center px-3 py-5 text-[12px] text-console-subtle"}>
                      <Show when={browsedProject()} fallback="No project selected">
                        No environments yet
                      </Show>
                    </div>
                  }
                >
                  <For each={filteredEnvs()}>
                    {(env) => (
                      <button
                        type="button"
                        class={cx(
                          ui.scopeItem,
                          browsedProjectID() === scope.selectedProjectID() &&
                            env.id === scope.selectedEnvironmentID() &&
                            ui.scopeItemSelected,
                        )}
                        onClick={() => pickEnv(env.id)}
                      >
                        <span class={envDotClass(envTone(env.slug))} />
                        <span class={"overflow-hidden text-ellipsis whitespace-nowrap font-medium"}>{env.name}</span>
                        <Show
                          when={
                            browsedProjectID() === scope.selectedProjectID() &&
                            env.id === scope.selectedEnvironmentID()
                          }
                          fallback={<span class={"font-mono text-[11px] text-console-subtle"}>{env.slug}</span>}
                        >
                          <span class={"text-[12px] font-medium leading-none text-console-accent"} aria-label="current">✓</span>
                        </Show>
                      </button>
                    )}
                  </For>
                </Show>
              </div>
              <div class={"border-t border-console-border bg-console-bg-panel p-1"}>
                <button
                  type="button"
                  class={createActionClass}
                  disabled={!browsedProject()}
                  onClick={startCreateEnvironment}
                >
                  <span class={createIconClass} aria-hidden="true">+</span>
                  <span class={"truncate"}>New environment</span>
                </button>
              </div>
            </div>
          </div>
          <div class={"flex items-center justify-between gap-3 border-t border-console-border bg-console-bg-panel px-2 py-1 font-mono text-[10px] text-console-subtle max-[720px]:flex-wrap"}>
            <span>Preview a project, then choose an environment.</span>
            <span><kbd class={"border border-console-border bg-white px-1 py-px font-mono text-[10px] text-console-muted"}>Esc</kbd> closes</span>
          </div>
        </div>
      </Show>

      <Show when={creating() === "project"}>
        <Modal title="New project" onClose={cancelForm} closeDisabled={submitting()}>
          <form onSubmit={submitProject}>
            <p class={ui.modalIntro}>Create a project, then add its first environment.</p>
            <label class={ui.field}>
              <span>Name</span>
              <input
                ref={formNameRef}
                type="text"
                class={ui.input}
                value={formName()}
                onInput={(event) => setFormName(event.currentTarget.value)}
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
                value={formSlug()}
                onInput={(event) => {
                  setSlugTouched(true);
                  setFormSlug(event.currentTarget.value);
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
              <button type="button" class={ui.secondaryButton} disabled={submitting()} onClick={cancelForm}>Cancel</button>
              <button class={ui.button} type="submit" disabled={submitting() || !formName().trim() || !formSlug().trim()}>
                {submitting() ? "Creating…" : "Create project"}
              </button>
            </div>
          </form>
        </Modal>
      </Show>

      <Show when={creating() === "environment"}>
        <Modal
          title={browsedProject() ? `New environment in ${browsedProject()!.name}` : "New environment"}
          onClose={cancelForm}
          closeDisabled={submitting()}
        >
          <form onSubmit={submitEnvironment}>
            <label class={ui.field}>
              <span>Name</span>
              <input
                ref={formNameRef}
                type="text"
                class={ui.input}
                value={formName()}
                onInput={(event) => setFormName(event.currentTarget.value)}
                placeholder="Production"
                autocomplete="off"
                autofocus
              />
            </label>
            <label class={ui.field}>
              <span>Slug</span>
              <input
                type="text"
                class={ui.input}
                value={formSlug()}
                onInput={(event) => {
                  setSlugTouched(true);
                  setFormSlug(event.currentTarget.value);
                }}
                placeholder="production"
                autocomplete="off"
                spellcheck={false}
              />
            </label>
            <Show when={formError()}>
              <p class={ui.fieldError} role="alert">{formError()}</p>
            </Show>
            <div class={ui.modalActions}>
              <button type="button" class={ui.secondaryButton} disabled={submitting()} onClick={cancelForm}>Cancel</button>
              <button
                class={ui.button}
                type="submit"
                disabled={submitting() || !formName().trim() || !formSlug().trim()}
              >
                {submitting() ? "Creating…" : "Create environment"}
              </button>
            </div>
          </form>
        </Modal>
      </Show>
    </div>
  );
}
