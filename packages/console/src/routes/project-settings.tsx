import { useMe } from "../lib/me";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, Show } from "solid-js";
import { errorMessage } from "../lib/error";
import { hasPermission } from "../lib/auth";
import { getProjectSettings, listRegions, updateProjectSettings, updateProject } from "../lib/projects";
import { useScope } from "../lib/scope";
import { ConfirmDialog } from "../ui/Dialog";
import { Field, FormActions, TextInput } from "../ui/Field";
import { HistoryRetentionFields, historyRetentionDraft, historyRetentionPolicy, historyRetentionSeconds, type HistoryRetentionDraft } from "../ui/HistoryRetentionFields";
import { IDText } from "../ui/IDText";
import { InlineStatus, Notice } from "../ui/Notice";
import { PageHeader } from "../ui/PageHeader";
import { Panel } from "../ui/Panel";
import { EmptyState, QueryState } from "../ui/QueryState";
import { LinesSkeleton } from "../ui/Skeleton";
import { ui } from "../ui/styles";

export function ProjectSettings() {
  const scope = useScope();
  const queryClient = useQueryClient();
  const [name, setName] = createSignal("");
  const [saving, setSaving] = createSignal(false);
  const [saveError, setSaveError] = createSignal<string | null>(null);

  const project = createMemo(() => scope.selectedProject());
  const regions = createQuery(() => ({ queryKey: ["regions"], queryFn: listRegions, retry: false }));
  const regionName = (id: string | undefined) => {
    const region = regions.data?.regions.find((candidate) => candidate.id === id);
    return region?.display_name || id || "—";
  };
  const dirty = createMemo(() => {
    const current = project();
    if (!current) return false;
    return name().trim() !== current.name;
  });

  createEffect(() => {
    const current = project();
    setName(current?.name ?? "");
    setSaveError(null);
  });

  async function submitProject(event: SubmitEvent) {
    event.preventDefault();
    const current = project();
    if (!current || !dirty()) return;
    const nextName = name().trim();
    if (!nextName) {
      setSaveError("Name is required.");
      return;
    }
    setSaving(true);
    setSaveError(null);
    try {
      await updateProject(current.project, { name: nextName });
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
    } catch (error) {
      setSaveError(errorMessage(error));
    } finally {
      setSaving(false);
    }
  }

  return (
    <>
      <PageHeader
        title="General"
        meta={
          <Show when={project()}>
            {(current) => (
              <>
                <span class="inline-flex items-baseline gap-1.5">Project ID <IDText value={current().project} mode="full" /></span>
                <span class="inline-flex items-baseline gap-1.5">Region <span class="text-console-text">{regionName(current().default_region_id)}</span></span>
              </>
            )}
          </Show>
        }
      />

      <Show when={project()} fallback={<EmptyState title="No project selected." />}>
        {(current) => (
          <div class={ui.sectionStack}>
            <Panel title="Profile">
              <form onSubmit={submitProject}>
                <Field label="Name" error={saveError()}>
                  <TextInput value={name()} onInput={(event) => setName(event.currentTarget.value)} autocomplete="off" />
                </Field>
                <Show when={dirty()}>
                  <FormActions
                    submitLabel="Save changes"
                    busyLabel="Saving..."
                    busy={saving()}
                    disabled={!name().trim()}
                    onCancel={() => { setName(current().name); setSaveError(null); }}
                  />
                </Show>
              </form>
            </Panel>
            <ProjectHistorySettings project={current().project} />
          </div>
        )}
      </Show>
    </>
  );
}

function ProjectHistorySettings(props: { project: string }) {
  const queryClient = useQueryClient();
  const me = useMe();
  const settings = createQuery(() => ({
    queryKey: ["project-settings", props.project],
    queryFn: () => getProjectSettings(props.project),
    retry: false,
  }));
  const [draft, setDraft] = createSignal<HistoryRetentionDraft>(historyRetentionDraft("30d"));
  const [saving, setSaving] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const [saved, setSaved] = createSignal(false);
  const [shortening, setShortening] = createSignal<string | null>(null);
  const canManage = () => hasPermission(me.data, "projects.update_retention");

  const reset = () => {
    const current = settings.data;
    if (current) setDraft(historyRetentionDraft(current.history_retention));
  };
  createEffect(reset);
  createEffect(() => {
    props.project;
    setError(null);
    setSaved(false);
  });
  const dirty = () => {
    try {
      return historyRetentionPolicy(draft()) !== settings.data?.history_retention;
    } catch {
      return true;
    }
  };

  async function save(next: string) {
    const project = props.project;
    setSaving(true);
    try {
      const result = await updateProjectSettings(project, { history_retention: next });
      queryClient.setQueryData(["project-settings", project], result);
      if (props.project === project) setSaved(true);
    } catch (cause) {
      if (props.project === project) setError(errorMessage(cause, "Could not save history retention."));
    } finally {
      setSaving(false);
    }
  }

  function submit(event: SubmitEvent) {
    event.preventDefault();
    if (!canManage() || !settings.data || saving() || !dirty()) return;
    setError(null);
    setSaved(false);
    let next: string;
    try {
      next = historyRetentionPolicy(draft());
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Could not save history retention.");
      return;
    }
    const before = historyRetentionSeconds(settings.data.history_retention);
    const after = historyRetentionSeconds(next);
    if (after !== null && (before === null || after < before)) setShortening(next);
    else void save(next);
  }

  return (
    <Panel title="Session history retention" actions={<Show when={canManage() && settings.data && dirty()}><span class="font-mono text-chrome text-console-warning">Unsaved changes</span></Show>}>
      <QueryState query={settings} pending={<LinesSkeleton label="Loading history retention" />} error={() => "Could not load history retention."}>
        {() => (
          <form onSubmit={submit}>
            <fieldset class="m-0 border-0 p-0" disabled={!canManage() || saving()}>
              <HistoryRetentionFields value={draft()} onChange={value => { setDraft(value); setSaved(false); }} />
            </fieldset>
            <Show when={!canManage()}><p class={ui.muted}>Update project retention permission is required to change this setting.</p></Show>
            <Show when={error()}>{(message) => <Notice tone="danger">{message()}</Notice>}</Show>
            <Show when={canManage()}>
              <div class="flex flex-wrap items-center justify-end gap-3">
                <Show when={saved()}><InlineStatus class="mt-3">History retention saved.</InlineStatus></Show>
                <Show when={dirty()}>
                  <FormActions
                    submitLabel="Save history retention"
                    busyLabel="Saving..."
                    busy={saving()}
                    onCancel={() => { reset(); setError(null); setSaved(false); }}
                  />
                </Show>
              </div>
            </Show>
          </form>
        )}
      </QueryState>
      <Show when={shortening()}>
        {(next) => (
          <ConfirmDialog
            title="Shorten history retention"
            confirmLabel="Shorten retention"
            busyLabel="Saving..."
            tone="danger"
            onClose={() => setShortening(null)}
            onConfirm={() => save(next())}
          >
            Older session content in every environment may be permanently deleted on the next cleanup pass. This cannot be undone.
          </ConfirmDialog>
        )}
      </Show>
    </Panel>
  );
}
