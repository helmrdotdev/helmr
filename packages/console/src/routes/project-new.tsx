import { returnPath } from "../lib/continuation";
import { A, useNavigate, useLocation } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, Show, type JSX } from "solid-js";
import { errorMessage } from "../lib/error";
import { ME_QUERY_KEY, useMe } from "../lib/me";
import { slugify } from "../lib/slug";
import { createProject, listRegions } from "../lib/projects";
import { rememberProjectScope } from "../lib/scope";
import { AuthWorking, AuthWindow, AuthTitle } from "../ui/AuthWindow";
import { Field } from "../ui/Field";
import { PageHeader } from "../ui/PageHeader";
import { Select } from "../ui/Select";
import { ui } from "../ui/styles";


export function ProjectNew() {
  const navigate = useNavigate();
  const location = useLocation();
  const queryClient = useQueryClient();
  const [name, setName] = createSignal("");
  const [slug, setSlug] = createSignal("");
  const [slugTouched, setSlugTouched] = createSignal(false);
  const [selectedRegionID, setSelectedRegionID] = createSignal("");
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const me = useMe();
  const regions = createQuery(() => ({
    queryKey: ["regions"],
    queryFn: listRegions,
    enabled: !!me.data?.org_id,
    retry: false,
    staleTime: 60_000,
  }));
  const regionOptions = createMemo(() => regions.data?.regions ?? []);
  const firstProject = createMemo(() => me.data?.project_required === true);

  createEffect(() => {
    if (!selectedRegionID()) setSelectedRegionID(regionOptions()[0]?.id ?? "");
  });


  async function submit(event: SubmitEvent) {
    event.preventDefault();
    const nextName = name().trim();
    const nextSlug = slug().trim();
    const nextRegionID = selectedRegionID().trim();
    if (!nextName || !nextSlug || !nextRegionID) {
      setError("Name, Project ID, and region are required.");
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      const wasFirstProject = firstProject();
      const project = await createProject({
        name: nextName,
        project: nextSlug,
        default_region_id: nextRegionID,
      });
      rememberProjectScope(project);
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
      await queryClient.invalidateQueries({ queryKey: ME_QUERY_KEY });
      navigate(returnPath(new URLSearchParams(location.search).get("next")), { replace: wasFirstProject });
    } catch (e) {
      setError(errorMessage(e));
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
      <Show when={!regions.isPending && regionOptions().length === 0}>
        <div class={ui.warning} role="status">
          A platform Region is required before a project can be created.
          <Show when={me.data?.admin}> <A class="text-console-accent" href="/admin/regions">Create a Region</A>.</Show>
        </div>
      </Show>
      <label class={ui.field}>
        <span>Project ID</span>
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
      <Field label="Region">
        <Select
          value={selectedRegionID()}
          options={regionOptions().map((region) => ({ value: region.id, label: region.display_name || region.id }))}
          onChange={setSelectedRegionID}
          disabled={regions.isPending || regionOptions().length === 0}
          minWidth="100%"
        />
      </Field>
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
    <Show when={!me.isPending && !regions.isPending} fallback={<AuthWorking>Loading...</AuthWorking>}>
      <Show
        when={firstProject()}
        fallback={
    <div class={ui.page}>
      <PageHeader title="New project" />

      {form()}
    </div>
        }
      >
        <AuthWindow>
          <AuthTitle>Create your first project</AuthTitle>
          {form()}
        </AuthWindow>
      </Show>
    </Show>
  );
}
