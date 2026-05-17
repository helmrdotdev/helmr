import { createQuery } from "@tanstack/solid-query";
import { For, Show } from "solid-js";
import { envTone } from "../features/projects/display";
import { ApiError } from "../lib/api";
import { listProjects, type Environment } from "../lib/projects";
import { envDotClass, ui } from "../ui/styles";

function projectsErrorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  return "Could not load projects.";
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
  const projects = createQuery(() => ({
    queryKey: ["projects"],
    queryFn: listProjects,
    retry: false,
  }));

  return (
    <>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Projects</h1>
          <p class={ui.pageSubtitle}>
            Projects and environments are managed from the scope switcher in the top bar.
            This page shows everything you have access to.
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
                    </tr>
                  )}
                </For>
              </tbody>
            </table>
          </div>
        </Show>
      </Show>
    </>
  );
}
