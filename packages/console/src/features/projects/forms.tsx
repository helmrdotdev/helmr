import { createQuery } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, Show } from "solid-js";
import { errorMessage } from "../../lib/error";
import { useMe } from "../../lib/me";
import { createEnvironment, createProject, listRegions, type Environment, type Project } from "../../lib/projects";
import { FormDialog } from "../../ui/Dialog";
import { ColorField, createDerivedSlug, Field, SlugInput, TextInput } from "../../ui/Field";
import { Notice } from "../../ui/Notice";
import { Select } from "../../ui/Select";
import { defaultEnvironmentColor, ENVIRONMENT_COLOR_PRESETS } from "./display";

// The new-project form shared by the scope selector and the new-project page:
// name, a Project ID that follows the name until edited, and the default Region.
export function createProjectDraft() {
  const me = useMe();
  const [name, setName] = createSignal("");
  const slug = createDerivedSlug(name);
  const [regionID, setRegionID] = createSignal("");
  const regions = createQuery(() => ({
    queryKey: ["regions"],
    queryFn: listRegions,
    enabled: !!me.data?.org_id,
    retry: false,
    staleTime: 60_000,
  }));
  const regionOptions = createMemo(() => (regions.data?.regions ?? []).map((region) => ({ value: region.id, label: region.display_name || region.id })));
  createEffect(() => {
    if (!regionID()) setRegionID(regionOptions()[0]?.value ?? "");
  });

  return {
    valid: () => !!name().trim() && !!slug.value().trim() && !!regionID(),
    submit: (): Promise<Project> => createProject({ name: name().trim(), project: slug.value().trim(), default_region_id: regionID() }),
    Fields: () => (
      <>
        <Field label="Name">
          <TextInput value={name()} onInput={(event) => setName(event.currentTarget.value)} placeholder="My app" autocomplete="off" autofocus />
        </Field>
        <Field label="Project ID" hint="Lowercase letters, numbers and hyphens. Used in URLs and the CLI; it cannot be changed later.">
          <SlugInput slug={slug} placeholder="my-app" />
        </Field>
        <Show when={!regions.isPending && regionOptions().length === 0}>
          <Notice tone="warning" class="mb-3">
            A platform region is required before a project can be created.
          </Notice>
        </Show>
        <Field label="Region">
          <Select
            value={regionID()}
            options={regionOptions()}
            onChange={setRegionID}
            disabled={regions.isPending || regionOptions().length === 0}
            minWidth="100%"
          />
        </Field>
      </>
    ),
  };
}

// The new-environment form shared by the scope selector and the Environments
// page: name, an Environment ID that follows the name until edited, and a color
// that follows the ID until chosen.
export function createEnvironmentDraft() {
  const [name, setName] = createSignal("");
  const slug = createDerivedSlug(name);
  const [chosenColor, setChosenColor] = createSignal<string | null>(null);
  const colorHex = () => chosenColor() ?? defaultEnvironmentColor(slug.value());

  return {
    valid: () => !!name().trim() && !!slug.value().trim(),
    submit: (projectID: string): Promise<Environment> => createEnvironment(projectID, { name: name().trim(), environment: slug.value().trim(), color_hex: colorHex() }),
    Fields: () => (
      <>
        <Field label="Name">
          <TextInput value={name()} onInput={(event) => setName(event.currentTarget.value)} placeholder="Preview" autocomplete="off" autofocus />
        </Field>
        <Field label="Environment ID">
          <SlugInput slug={slug} placeholder="preview" />
        </Field>
        <ColorField value={colorHex()} onChange={setChosenColor} presets={ENVIRONMENT_COLOR_PRESETS} ariaLabel="Environment color" />
      </>
    ),
  };
}

function useSubmission<T>(submit: () => Promise<T>, done: (result: T) => Promise<void> | void) {
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  return {
    busy,
    error,
    run: async () => {
      setBusy(true);
      setError(null);
      try {
        await done(await submit());
      } catch (cause) {
        setError(errorMessage(cause));
      } finally {
        setBusy(false);
      }
    },
  };
}

export function NewProjectDialog(props: { onCreated: (project: Project) => Promise<void> | void; onClose: () => void }) {
  const draft = createProjectDraft();
  const submission = useSubmission(draft.submit, props.onCreated);
  return (
    <FormDialog
      title="New project"
      submitLabel="Create"
      busyLabel="Creating…"
      busy={submission.busy()}
      disabled={!draft.valid()}
      error={submission.error()}
      onSubmit={() => void submission.run()}
      onClose={props.onClose}
    >
      <draft.Fields />
    </FormDialog>
  );
}

export function NewEnvironmentDialog(props: { project: Project; onCreated: (environment: Environment) => Promise<void> | void; onClose: () => void }) {
  const draft = createEnvironmentDraft();
  const submission = useSubmission(() => draft.submit(props.project.project), props.onCreated);
  return (
    <FormDialog
      title={`New environment in ${props.project.name}`}
      submitLabel="Create"
      busyLabel="Creating…"
      busy={submission.busy()}
      disabled={!draft.valid()}
      error={submission.error()}
      onSubmit={() => void submission.run()}
      onClose={props.onClose}
    >
      <draft.Fields />
    </FormDialog>
  );
}
