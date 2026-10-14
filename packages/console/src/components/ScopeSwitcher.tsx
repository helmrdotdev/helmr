import { useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { A, useLocation, useNavigate } from "@solidjs/router";
import { scopeSwitchPath } from "../lib/scope-navigation";
import { defaultEnvironmentColor } from "../features/projects/display";
import { NewEnvironmentDialog, NewProjectDialog } from "../features/projects/forms";
import { rememberProjectScope, useScope } from "../lib/scope";
import { EnvDot } from "../ui/EnvChip";
import { SearchInput } from "../ui/Field";
import { Popover } from "../ui/Popover";
import { cx, toneClass, ui } from "../ui/styles";

type CreateMode = "project" | "environment" | null;

const SEARCH = "h-6 min-h-6 px-2 font-mono text-badge";
const CREATE_ACTION =
  "flex h-7 w-full cursor-pointer items-center gap-1.5 border-0 bg-transparent px-2 text-left font-mono text-badge font-medium text-console-muted transition-colors hover:bg-console-surface hover:text-console-text disabled:cursor-not-allowed disabled:opacity-45";
const CREATE_ICON =
  "grid size-4 flex-shrink-0 place-items-center border border-console-border bg-console-surface text-control font-medium leading-none text-console-muted";
const COLUMN =
  "flex min-h-47 min-w-0 flex-col border-console-border even:border-l max-[720px]:even:border-l-0 max-[720px]:even:border-t";
const COLUMN_HEAD = "flex flex-col gap-1.5 border-b border-console-border bg-console-bg-panel p-1.5";
const NO_MATCH = "grid flex-1 place-items-center px-3 py-5 text-control text-console-subtle";

// `opener` receives a function that opens the switcher, for controls elsewhere in
// the shell (the phone menu) that change the scope through it.
export function ScopeSwitcher(props: { opener?: (open: () => void) => void }) {
  const scope = useScope();
  const navigate = useNavigate();
  const location = useLocation();
  const queryClient = useQueryClient();
  const [open, setOpen] = createSignal(false);
  const [trigger, setTrigger] = createSignal<HTMLButtonElement>();
  const [projectQuery, setProjectQuery] = createSignal("");
  const [envQuery, setEnvQuery] = createSignal("");
  const [creating, setCreating] = createSignal<CreateMode>(null);
  let projectSearchRef: HTMLInputElement | undefined;

  const activeProject = createMemo(() => scope.selectedProject());
  const filteredProjects = createMemo(() => {
    const q = projectQuery().trim().toLowerCase();
    if (!q) return scope.projects();
    return scope.projects().filter((project) =>
      project.name.toLowerCase().includes(q) || project.project.toLowerCase().includes(q),
    );
  });

  const filteredEnvs = createMemo(() => {
    const envs = activeProject()?.environments ?? [];
    const q = envQuery().trim().toLowerCase();
    if (!q) return envs;
    return envs.filter((env) =>
      env.name.toLowerCase().includes(q) || env.environment.toLowerCase().includes(q),
    );
  });

  function startCreate(mode: Exclude<CreateMode, null>) {
    if (mode === "environment" && !activeProject()) return;
    setOpen(false);
    setCreating(mode);
  }

  const toggle = () => {
    if (scope.isLoading()) return;
    if (open()) {
      setOpen(false);
      return;
    }
    setProjectQuery("");
    setEnvQuery("");
    setCreating(null);
    setOpen(true);
    queueMicrotask(() => projectSearchRef?.focus());
  };

  props.opener?.(() => { if (!open()) toggle(); });

  const pick = (apply: () => void) => {
    apply();
    navigate(scopeSwitchPath(location.pathname));
    setOpen(false);
  };

  const selectedEnvironment = createMemo(() => scope.selectedEnvironment());
  const hasNoProjects = createMemo(() => !scope.isLoading() && scope.projects().length === 0);

  return (
    <div class={"relative min-w-0"}>
      <button
        ref={setTrigger}
        type="button"
        class={ui.scopeTrigger}
        data-open={open() ? "true" : "false"}
        disabled={scope.isLoading()}
        aria-haspopup="dialog"
        aria-expanded={open()}
        onClick={toggle}
      >
        <Show
          when={!hasNoProjects()}
          fallback={<span class={"max-w-37.5 overflow-hidden text-ellipsis whitespace-nowrap font-medium text-console-text max-sm:max-w-27.5"}>No projects yet</span>}
        >
          <span class={"text-console-muted max-[720px]:sr-only"}>Project</span>
          <span class={"max-w-32.5 overflow-hidden text-ellipsis whitespace-nowrap font-medium text-console-text max-sm:max-w-23"}>{scope.selectedProject()?.name ?? "—"}</span>
          <span class={"text-console-faint"}>/</span>
          <span class={"text-console-muted max-[720px]:sr-only"}>Env</span>
          <span class={"flex max-w-32.5 items-center gap-1 overflow-hidden text-ellipsis whitespace-nowrap font-medium text-console-text max-sm:max-w-23"}>
            <EnvDot colorHex={selectedEnvironment()?.color_hex ?? defaultEnvironmentColor("")} />
            {selectedEnvironment()?.name ?? "—"}
          </span>
        </Show>
        <span class={cx("ml-0.5 grid size-4 place-items-center transition-transform", open() && "rotate-180")} aria-hidden="true">
          <span class={"block size-1.5 -translate-y-px rotate-45 border-b border-r border-console-muted"} />
        </span>
        <Show when={scope.isLoading()}>
          <span class={cx("ml-1.5 border px-1.5 py-0.5 text-micro font-medium", toneClass.warning)}>Loading…</span>
        </Show>
      </button>

      <Popover
        open={open()}
        anchor={trigger()}
        onDismiss={() => setOpen(false)}
        role="dialog"
        label="Choose project"
        title="Switch project"
        width={440}
        maxHeight={600}
        fullWidthOnPhones
      >
        <div class={"grid grid-cols-2 max-[720px]:grid-cols-1"}>
          <div class={COLUMN}>
            <div class={COLUMN_HEAD}>
              <span class={ui.microLabel}>Projects</span>
              <SearchInput
                ref={projectSearchRef}
                class={SEARCH}
                aria-label="Find loaded projects"
                placeholder="Find loaded projects…"
                value={projectQuery()}
                onInput={(event) => setProjectQuery(event.currentTarget.value)}
              />
            </div>
            <div class={"flex max-h-54 flex-1 flex-col gap-0.5 overflow-y-auto p-1"}>
              <Show when={filteredProjects().length > 0} fallback={<div class={NO_MATCH}>No matches</div>}>
                <For each={filteredProjects()}>
                  {(project) => (
                    <button
                      type="button"
                      class={cx(ui.scopeItem, project.project === scope.selectedProjectID() && ui.scopeItemSelected)}
                      onClick={() => pick(() => scope.setSelectedProjectID(project.project))}
                    >
                      <span />
                      <span class={"overflow-hidden text-ellipsis whitespace-nowrap font-medium"}>{project.name}</span>
                      <Show when={project.project === scope.selectedProjectID()} fallback={<span />}>
                        <span class={"text-control font-medium leading-none text-console-accent"} aria-label="current">✓</span>
                      </Show>
                    </button>
                  )}
                </For>
              </Show>
            </div>
            <div class={"border-t border-console-border bg-console-bg-panel p-1"}>
              <Show when={scope.hasMoreProjects()}>
                <button type="button" class={CREATE_ACTION} disabled={scope.isLoadingMoreProjects()} onClick={() => scope.loadMoreProjects()}>
                  <span class={CREATE_ICON} aria-hidden="true">↓</span>
                  <span class={"truncate"}>{scope.isLoadingMoreProjects() ? "Loading…" : "Load more projects"}</span>
                </button>
              </Show>
              <button type="button" class={CREATE_ACTION} onClick={() => startCreate("project")}>
                <span class={CREATE_ICON} aria-hidden="true">+</span>
                <span class={"truncate"}>New project</span>
              </button>
            </div>
          </div>

          <div class={COLUMN}>
            <div class={COLUMN_HEAD}>
              <span class={ui.microLabel}>
                Environments
                <Show when={activeProject()}>
                  {(project) => <span class={"ml-1.5 normal-case tracking-normal"}>in {project().name}</span>}
                </Show>
              </span>
              <SearchInput
                class={SEARCH}
                aria-label="Find an environment"
                placeholder="Find an environment…"
                value={envQuery()}
                onInput={(event) => setEnvQuery(event.currentTarget.value)}
              />
            </div>
            <div class={"flex max-h-54 flex-1 flex-col gap-0.5 overflow-y-auto p-1"}>
              <Show
                when={filteredEnvs().length > 0}
                fallback={
                  <div class={NO_MATCH}>
                    <Show when={activeProject()} fallback="No project selected">No environments yet</Show>
                  </div>
                }
              >
                <For each={filteredEnvs()}>
                  {(env) => (
                    <button
                      type="button"
                      class={cx(ui.scopeItem, env.environment === scope.selectedEnvironmentID() && ui.scopeItemSelected)}
                      onClick={() => pick(() => scope.setSelectedEnvironmentID(env.environment))}
                    >
                      <EnvDot colorHex={env.color_hex} />
                      <span class={"overflow-hidden text-ellipsis whitespace-nowrap font-medium"}>{env.name}</span>
                      <Show when={env.environment === scope.selectedEnvironmentID()} fallback={<span />}>
                        <span class={"text-control font-medium leading-none text-console-accent"} aria-label="current">✓</span>
                      </Show>
                    </button>
                  )}
                </For>
              </Show>
            </div>
            <div class={"border-t border-console-border bg-console-bg-panel p-1"}>
              <button type="button" class={CREATE_ACTION} disabled={!activeProject()} onClick={() => startCreate("environment")}>
                <span class={CREATE_ICON} aria-hidden="true">+</span>
                <span class={"truncate"}>New environment</span>
              </button>
            </div>
          </div>
        </div>
        <div class={"flex items-center justify-end border-t border-console-border bg-console-bg-panel px-2 py-1"}>
          <A
            href="/settings/project"
            class={"font-mono text-micro font-medium text-console-accent hover:text-console-accent-hover"}
            onClick={() => setOpen(false)}
          >
            Project settings
          </A>
        </div>
      </Popover>

      <Show when={creating() === "project"}>
        <NewProjectDialog
          onClose={() => setCreating(null)}
          onCreated={async (project) => {
            const environment = project.environments?.find((candidate) => candidate.is_default) ?? project.environments?.[0];
            rememberProjectScope(project);
            await queryClient.invalidateQueries({ queryKey: ["projects"] });
            scope.setSelectedProjectID(project.project);
            scope.setSelectedEnvironmentID(environment?.environment ?? "");
            navigate(scopeSwitchPath(location.pathname));
            setCreating(null);
          }}
        />
      </Show>

      <Show when={creating() === "environment" && activeProject()}>
        {(project) => (
          <NewEnvironmentDialog
            project={project()}
            onClose={() => setCreating(null)}
            onCreated={async (environment) => {
              await queryClient.invalidateQueries({ queryKey: ["projects"] });
              scope.setSelectedEnvironmentID(environment.environment);
              navigate(scopeSwitchPath(location.pathname));
              setCreating(null);
            }}
          />
        )}
      </Show>
    </div>
  );
}
