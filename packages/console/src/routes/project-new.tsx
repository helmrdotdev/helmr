import { useNavigate } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, Show, type JSX } from "solid-js";
import { ApiError } from "../lib/api";
import { getMe } from "../lib/auth";
import { createProject, listProjects } from "../lib/projects";
import { rememberProjectScope } from "../lib/scope";
import { AuthCopy, AuthLoading, AuthScreen, AuthTitle } from "../ui/AuthScreen";
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

function createErrorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  return "Something went wrong.";
}

export function ProjectNew() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [name, setName] = createSignal("");
  const [slug, setSlug] = createSignal("");
  const [slugTouched, setSlugTouched] = createSignal(false);
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const me = createQuery(() => ({
    queryKey: ["me"],
    queryFn: getMe,
    retry: false,
    staleTime: 60_000,
  }));
  const projects = createQuery(() => ({
    queryKey: ["projects"],
    queryFn: listProjects,
    enabled: !!me.data?.org_id,
    retry: false,
    staleTime: 30_000,
  }));
  const firstProject = createMemo(() =>
    !projects.isPending && !projects.isError && (projects.data?.projects.length ?? 0) === 0,
  );

  createEffect(() => {
    if (me.data?.access_required) {
      navigate("/access-required", { replace: true });
      return;
    }
    if (me.data?.organization_required) {
      navigate("/organizations/new", { replace: true });
    }
  });

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    const nextName = name().trim();
    const nextSlug = slug().trim();
    if (!nextName || !nextSlug) {
      setError("Name and slug are required.");
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      const project = await createProject({ name: nextName, slug: nextSlug });
      rememberProjectScope(project);
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
      await queryClient.invalidateQueries({ queryKey: ["me"] });
      navigate("/tasks", { replace: firstProject() });
    } catch (e) {
      setError(createErrorMessage(e));
    } finally {
      setSubmitting(false);
    }
  }

  const form = (): JSX.Element => (
    <form class={firstProject() ? undefined : "max-w-115"} onSubmit={submit}>
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
      <Show when={error()}>
        <p class={ui.fieldError} role="alert">{error()}</p>
      </Show>
      <div class={firstProject() ? undefined : ui.actionRow}>
        <button class={ui.button} type="submit" disabled={submitting() || !name().trim() || !slug().trim()}>
          {submitting() ? "Creating..." : "Create project"}
        </button>
      </div>
    </form>
  );

  return (
    <Show when={!me.isPending && !projects.isPending} fallback={<AuthLoading>Loading...</AuthLoading>}>
      <Show
        when={firstProject()}
        fallback={
    <div class={ui.page}>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>New project</h1>
          <p class={ui.pageSubtitle}>
            Projects group deployments, runs, secrets, and worker access. Helmr will add Production as the default environment.
          </p>
        </div>
      </div>

      {form()}
    </div>
        }
      >
        <AuthScreen>
          <AuthTitle>Create your first project</AuthTitle>
          <AuthCopy>
            Projects group deployments, runs, secrets, and worker access. Helmr will add a Production environment automatically.
          </AuthCopy>
          {form()}
        </AuthScreen>
      </Show>
    </Show>
  );
}
