import { IDText } from "../ui/IDText";
import { useQueryClient } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, Show } from "solid-js";
import { ApiError } from "../lib/api";
import { updateProject } from "../lib/projects";
import { useScope } from "../lib/scope";
import { PageHeader } from "../ui/PageHeader";
import { StatePanel } from "../ui/StatePanel";
import { ui } from "../ui/styles";

function formErrorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  return "Something went wrong.";
}

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
      setSaveError(formErrorMessage(error));
    } finally {
      setSaving(false);
    }
  }

  return (
    <>
      <PageHeader title="Project" subtitle="Display name and permanent ID of the selected project." />

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
      </Show>
    </>
  );
}
