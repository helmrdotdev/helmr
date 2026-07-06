import { useNavigate } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, Show, type JSX } from "solid-js";
import { ApiError } from "../lib/api";
import { getMe } from "../lib/auth";
import { createProject, listProjects, listRegions } from "../lib/projects";
import { rememberProjectScope } from "../lib/scope";
import { AuthLoading, AuthScreen, AuthTitle } from "../ui/AuthScreen";
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
  const [selectedRegionID, setSelectedRegionID] = createSignal("");
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
  const regions = createQuery(() => ({
    queryKey: ["regions"],
    queryFn: listRegions,
    enabled: !!me.data?.org_id,
    retry: false,
    staleTime: 60_000,
  }));
  const availableRegions = createMemo(() =>
    (regions.data?.regions ?? []).filter((region) => region.state === "available"),
  );
  const firstProject = createMemo(() =>
    !projects.isPending && !projects.isError && (projects.data?.projects.length ?? 0) === 0,
  );

  createEffect(() => {
    if (!selectedRegionID()) setSelectedRegionID(availableRegions()[0]?.id ?? "");
  });

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
    const nextRegionID = selectedRegionID().trim();
    if (!nextName || !nextSlug || !nextRegionID) {
      setError("Name, slug, and region are required.");
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      const wasFirstProject = firstProject();
      const project = await createProject({
        name: nextName,
        slug: nextSlug,
        default_region_id: nextRegionID,
      });
      rememberProjectScope(project);
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
      await queryClient.invalidateQueries({ queryKey: ["me"] });
      navigate("/", { replace: wasFirstProject });
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
      <label class={ui.field}>
        <span>Region</span>
        <select
          class={ui.input}
          value={selectedRegionID()}
          onChange={(event) => setSelectedRegionID(event.currentTarget.value)}
          disabled={regions.isPending || availableRegions().length === 0}
        >
          <For each={availableRegions()}>
            {(region) => (
              <option value={region.id}>
                {region.display_name || region.id}
              </option>
            )}
          </For>
        </select>
      </label>
      <Show when={error()}>
        <p class={ui.fieldError} role="alert">{error()}</p>
      </Show>
      <div class={firstProject() ? undefined : ui.actionRow}>
        <button class={ui.button} type="submit" disabled={submitting() || !name().trim() || !slug().trim() || !selectedRegionID().trim()}>
          {submitting() ? "Creating..." : "Create"}
        </button>
      </div>
    </form>
  );

  return (
    <Show when={!me.isPending && !projects.isPending && !regions.isPending} fallback={<AuthLoading>Loading...</AuthLoading>}>
      <Show
        when={firstProject()}
        fallback={
    <div class={ui.page}>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>New project</h1>
        </div>
      </div>

      {form()}
    </div>
        }
      >
        <AuthScreen>
          <AuthTitle>Create your first project</AuthTitle>
          {form()}
        </AuthScreen>
      </Show>
    </Show>
  );
}
