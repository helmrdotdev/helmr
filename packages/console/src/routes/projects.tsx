import { useMe } from "../lib/me";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, Show } from "solid-js";
import { errorMessage } from "../lib/error";
import { hasPermission } from "../lib/auth";
import { getProjectSettings, updateProjectSettings, updateProject } from "../lib/projects";
import { useScope } from "../lib/scope";
import { HistoryRetentionFields, historyRetentionDraft, historyRetentionPolicy, historyRetentionSeconds, type HistoryRetentionDraft } from "../ui/HistoryRetentionFields";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { StatePanel } from "../ui/StatePanel";
import { ui } from "../ui/styles";


export function Projects() {
  const scope = useScope();
  const queryClient = useQueryClient();
  const [name, setName] = createSignal("");
  const [saving, setSaving] = createSignal(false);
  const [saveError, setSaveError] = createSignal<string | null>(null);

  const project = createMemo(() => scope.selectedProject());
  const hasProject = createMemo(() => !!project());
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
    if (!current) return;
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
      <PageHeader title="Project" subtitle="Profile and settings shared by every Environment in the selected Project." />

      <Show when={hasProject()} fallback={<StatePanel empty="No project selected." />}>
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
              }}
              autocomplete="off"
            />
          </label>
          <label class={ui.field}>
            <span>Project ID</span>
            <IDText value={project()?.project ?? ""} mode="full" />
          </label>
          <Show when={saveError()}>
            <p class={ui.fieldError} role="alert">{saveError()}</p>
          </Show>
          <div class={ui.actionRow}>
            <button
              class={ui.button}
              type="submit"
              disabled={saving() || !dirty() || !name().trim()}
            >
              {saving() ? "Saving..." : "Save changes"}
            </button>
          </div>
        </form>
        <ProjectHistorySettings project={project()!.project} />
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
  const canManage = () => hasPermission(me.data, "projects.manage");

  createEffect(() => {
    const current = settings.data;
    if (current) setDraft(historyRetentionDraft(current.history_retention));
  });
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

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (!canManage() || !settings.data || saving()) return;
    setError(null);
    setSaved(false);
    const project = props.project;
    try {
      const next = historyRetentionPolicy(draft());
      const before = historyRetentionSeconds(settings.data.history_retention);
      const after = historyRetentionSeconds(next);
      if (after !== null && (before === null || after < before) &&
        !window.confirm("Shorten history retention? Older Session content in every Environment may be permanently deleted on the next cleanup pass. This cannot be undone.")) return;
      setSaving(true);
      const result = await updateProjectSettings(project, { history_retention: next });
      queryClient.setQueryData(["project-settings", project], result);
      if (props.project === project) setSaved(true);
    } catch (error) {
      if (props.project === project) setError(error instanceof Error ? error.message : "Could not save history retention.");
    } finally {
      setSaving(false);
    }
  }

  return (
    <section class="mt-4 max-w-220 border border-console-border-strong bg-console-surface px-4 py-4">
      <h2 class={ui.h2}>Session history</h2>
      <Show when={!settings.isPending} fallback={<p class={ui.muted}>Loading history retention...</p>}>
        <Show when={!settings.isError} fallback={
          <p class={ui.fieldError} role="alert">
            Could not load history retention. <button type="button" class={ui.secondaryButton} onClick={() => settings.refetch()}>Retry</button>
          </p>
        }>
          <form onSubmit={submit}>
            <fieldset disabled={!canManage() || saving()}>
              <HistoryRetentionFields value={draft()} onChange={value => { setDraft(value); setSaved(false); }} />
            </fieldset>
            <Show when={!canManage()}><p class={ui.muted}>Only Owners and Admins can change this setting.</p></Show>
            <Show when={error()}><p class={ui.fieldError} role="alert">{error()}</p></Show>
            <Show when={saved()}><p role="status">History retention saved.</p></Show>
            <Show when={canManage()}>
              <div class={ui.actionRow}>
                <button class={ui.button} type="submit" disabled={saving() || !dirty()}>
                  {saving() ? "Saving..." : "Save history retention"}
                </button>
              </div>
            </Show>
          </form>
        </Show>
      </Show>
    </section>
  );
}
