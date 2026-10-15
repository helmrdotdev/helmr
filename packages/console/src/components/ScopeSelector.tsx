import { useQueryClient } from "@tanstack/solid-query";
import { A, useLocation, useNavigate } from "@solidjs/router";
import { createEffect, createMemo, createSignal, on, Show, type JSX } from "solid-js";
import { environmentChip } from "../features/projects/display";
import { NewEnvironmentDialog, NewProjectDialog } from "../features/projects/forms";
import { isProtectedEnvironment, type Project } from "../lib/projects";
import { rememberProjectScope, useScope } from "../lib/scope";
import { scopeSwitchPath } from "../lib/scope-navigation";
import { CommandList, type CommandItem } from "../ui/CommandList";
import { EnvDot, ProtectedBadge } from "../ui/EnvChip";
import { Popover } from "../ui/Popover";
import { SkeletonBar } from "../ui/Skeleton";
import { cx } from "../ui/styles";

// While the picker is open, further pages of projects load by themselves up to
// this many projects, so the search covers them; past it "Load more" remains.
const AUTO_LOAD_PROJECTS = 200;

const TRIGGER =
  "flex h-7 min-w-0 cursor-pointer items-center gap-1.5 rounded-xs border border-transparent bg-transparent px-2 py-0 font-mono text-chrome font-medium text-console-text transition hover:border-console-border-strong hover:bg-console-bg-panel disabled:cursor-not-allowed disabled:opacity-50 data-[open=true]:border-console-border-strong data-[open=true]:bg-console-bg-panel";
const FOOTER = "flex items-center justify-between gap-2 border-t border-console-border bg-console-bg-panel p-1";
const FOOTER_ACTION =
  "flex h-7 min-w-0 cursor-pointer items-center gap-1.5 rounded-xs border-0 bg-transparent px-2 font-mono text-badge font-medium text-console-muted transition-colors hover:bg-console-surface hover:text-console-text disabled:cursor-not-allowed disabled:opacity-45";
const FOOTER_ICON =
  "grid size-4 shrink-0 place-items-center border border-console-border bg-console-surface text-control font-medium leading-none text-console-muted";
const MANAGE = "flex h-7 shrink-0 items-center rounded-xs px-2 font-mono text-micro font-medium text-console-accent hover:text-console-accent-hover";

function Chevron(props: { open: boolean }) {
  return (
    <span class={cx("grid size-3 shrink-0 place-items-center transition-transform", props.open && "rotate-180")} aria-hidden="true">
      <span class="block size-1.5 -translate-y-px rotate-45 border-b border-r border-console-muted" />
    </span>
  );
}

// One segment of the scope selector: a trigger and its searchable picker.
function Picker(props: {
  name: string;
  label: string;
  title: string;
  open: boolean;
  setOpen: (open: boolean) => void;
  disabled: boolean;
  trigger: JSX.Element;
  items: readonly CommandItem[];
  search: { label: string; placeholder: string; empty: string };
  footer: JSX.Element;
}) {
  const [anchor, setAnchor] = createSignal<HTMLButtonElement>();
  let search: HTMLInputElement | undefined;
  // Opening puts focus in the search box.
  createEffect(on(() => props.open, (open) => {
    if (open) requestAnimationFrame(() => search?.focus());
  }));
  return (
    <>
      <button
        ref={setAnchor}
        type="button"
        class={TRIGGER}
        data-open={props.open ? "true" : "false"}
        disabled={props.disabled}
        aria-haspopup="dialog"
        aria-expanded={props.open}
        onClick={() => props.setOpen(!props.open)}
      >
        <span class="sr-only">{`${props.name}: `}</span>
        {props.trigger}
        <Chevron open={props.open} />
      </button>
      <Popover
        open={props.open}
        anchor={anchor()}
        onDismiss={() => props.setOpen(false)}
        role="dialog"
        label={props.label}
        title={props.title}
        width={300}
        maxHeight={420}
        fullWidthOnPhones
        bodyClass="flex min-h-0 flex-col"
      >
        <CommandList
          label={props.label}
          searchLabel={props.search.label}
          placeholder={props.search.placeholder}
          empty={props.search.empty}
          items={props.items}
          inputRef={(element) => { search = element; }}
          class="max-h-80"
        />
        <div class={FOOTER}>{props.footer}</div>
      </Popover>
    </>
  );
}

// The tab's scope in the header: `<Project> / <Environment>`, each opening its
// own searchable picker with create and manage actions.
export function ScopeSelector() {
  const scope = useScope();
  const navigate = useNavigate();
  const location = useLocation();
  const queryClient = useQueryClient();
  const [openProject, setOpenProject] = createSignal(false);
  const [openEnvironment, setOpenEnvironment] = createSignal(false);
  const [creating, setCreating] = createSignal<"project" | "environment">();

  const switchTo = (apply: () => void) => {
    apply();
    navigate(scopeSwitchPath(location.pathname));
  };

  // The selected Project heads the list even when it sits on a page not loaded yet.
  const projects = createMemo((): Project[] => {
    const loaded = scope.projects();
    const selected = scope.selectedProject();
    return selected && !loaded.some((project) => project.project === selected.project) ? [selected, ...loaded] : loaded;
  });
  const projectItems = createMemo((): CommandItem[] => projects().map((project) => ({
    id: project.project,
    label: project.name,
    keywords: project.project,
    current: project.project === scope.selectedProjectID(),
    onSelect: () => {
      setOpenProject(false);
      if (project.project !== scope.selectedProjectID()) switchTo(() => scope.setSelectedProjectID(project.project));
    },
  })));
  const environmentItems = createMemo((): CommandItem[] => (scope.selectedProject()?.environments ?? []).map((environment) => ({
    id: environment.environment,
    label: environment.name,
    keywords: environment.environment,
    leading: <EnvDot colorHex={environmentChip(environment)?.colorHex} />,
    trailing: isProtectedEnvironment(environment) ? <ProtectedBadge /> : undefined,
    current: environment.environment === scope.selectedEnvironmentID(),
    onSelect: () => {
      setOpenEnvironment(false);
      if (environment.environment !== scope.selectedEnvironmentID()) switchTo(() => scope.setSelectedEnvironmentID(environment.environment));
    },
  })));

  // Load the remaining pages while the picker is open, once per page, so a
  // failed page is not retried in a loop.
  let autoLoadedAt = -1;
  createEffect(() => {
    const count = scope.projects().length;
    if (!openProject() || !scope.hasMoreProjects() || scope.isLoadingMoreProjects()) return;
    if (count >= AUTO_LOAD_PROJECTS || count === autoLoadedAt) return;
    autoLoadedAt = count;
    scope.loadMoreProjects();
  });

  const startCreate = (mode: "project" | "environment") => {
    setOpenProject(false);
    setOpenEnvironment(false);
    setCreating(mode);
  };

  const environment = () => environmentChip(scope.selectedEnvironment());
  const noProjects = () => !scope.isLoading() && projects().length === 0;
  const allLoaded = () => !scope.hasMoreProjects();

  return (
    <div class="flex min-w-0 items-center gap-0.5">
      <Picker
        name="Project"
        label="Choose project"
        title="Project"
        open={openProject()}
        setOpen={setOpenProject}
        disabled={scope.isLoading()}
        trigger={
          <Show when={!scope.isLoading()} fallback={<SkeletonBar width="72px" />}>
            <span class="max-w-40 truncate max-sm:max-w-22">
              {noProjects() ? "No projects yet" : scope.selectedProject()?.name ?? "—"}
            </span>
          </Show>
        }
        items={projectItems()}
        search={{
          label: allLoaded() ? "Find a project" : "Find loaded projects",
          placeholder: allLoaded() ? "Find a project…" : "Find loaded projects…",
          empty: projects().length === 0 ? "No projects yet" : "No matching projects",
        }}
        footer={
          <>
            <div class="flex min-w-0 items-center">
              <button type="button" class={FOOTER_ACTION} onClick={() => startCreate("project")}>
                <span class={FOOTER_ICON} aria-hidden="true">+</span>
                <span class="truncate">New project</span>
              </button>
              <Show when={scope.hasMoreProjects()}>
                <button type="button" class={FOOTER_ACTION} disabled={scope.isLoadingMoreProjects()} onClick={() => scope.loadMoreProjects()}>
                  <span class="truncate">{scope.isLoadingMoreProjects() ? "Loading…" : "Load more projects"}</span>
                </button>
              </Show>
            </div>
            <A href="/settings/project" class={MANAGE} onClick={() => setOpenProject(false)}>Manage</A>
          </>
        }
      />
      <Show when={!noProjects()}>
        <span class="text-console-faint" aria-hidden="true">/</span>
        <Picker
          name="Environment"
          label="Choose environment"
          title={`Environment · ${scope.selectedProject()?.name ?? "—"}`}
          open={openEnvironment()}
          setOpen={setOpenEnvironment}
          disabled={scope.isLoading() || !scope.selectedProject()}
          trigger={
            <>
              <EnvDot colorHex={environment()?.colorHex} />
              <span class="max-w-40 truncate max-sm:max-w-22">{environment()?.name ?? "—"}</span>
              <Show when={environment()?.protected}><span class="max-sm:hidden"><ProtectedBadge /></span></Show>
            </>
          }
          items={environmentItems()}
          search={{ label: "Find an environment", placeholder: "Find an environment…", empty: environmentItems().length === 0 ? "No environments yet" : "No matching environments" }}
          footer={
            <>
              <button type="button" class={FOOTER_ACTION} onClick={() => startCreate("environment")}>
                <span class={FOOTER_ICON} aria-hidden="true">+</span>
                <span class="truncate">New environment</span>
              </button>
              <A href="/settings/environments" class={MANAGE} onClick={() => setOpenEnvironment(false)}>Manage</A>
            </>
          }
        />
      </Show>

      <Show when={creating() === "project"}>
        <NewProjectDialog
          onClose={() => setCreating(undefined)}
          onCreated={async (project) => {
            const created = project.environments?.find((candidate) => candidate.is_default) ?? project.environments?.[0];
            rememberProjectScope(project);
            await queryClient.invalidateQueries({ queryKey: ["projects"] });
            switchTo(() => {
              scope.setSelectedProjectID(project.project);
              scope.setSelectedEnvironmentID(created?.environment ?? "");
            });
            setCreating(undefined);
          }}
        />
      </Show>
      <Show when={creating() === "environment" && scope.selectedProject()}>
        {(project) => (
          <NewEnvironmentDialog
            project={project()}
            onClose={() => setCreating(undefined)}
            onCreated={async (created) => {
              await queryClient.invalidateQueries({ queryKey: ["projects"] });
              switchTo(() => scope.setSelectedEnvironmentID(created.environment));
              setCreating(undefined);
            }}
          />
        )}
      </Show>
    </div>
  );
}
