import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { useLocation, useNavigate } from "@solidjs/router";
import { batch, createContext, createEffect, createMemo, createSignal, onCleanup, onMount, useContext, type JSX } from "solid-js";
import { ApiError } from "./api";
import {
  getProject,
  listProjects,
  resolveProjectSelection,
  resolveScopeID,
  type Environment,
  type Project,
} from "./projects";
import { linkNotice, linkRestoreScope, readScopeHint, withoutScopeHint, type LinkNotice, type ScopeIDs, type ScopeLink } from "./scope-link";
import {
  browserStorage,
  readEnvironmentSelections,
  readTabScope,
  writeEnvironmentSelections,
  writeScope,
} from "./scope-storage";

type ScopeNotice = { kind: LinkNotice; projectName: string; environmentName: string };

type ScopeContextValue = {
  projects: () => Project[];
  selectedProject: () => Project | undefined;
  selectedEnvironment: () => Environment | undefined;
  selectedProjectID: () => string;
  selectedEnvironmentID: () => string;
  setSelectedProjectID: (id: string) => void;
  setSelectedEnvironmentID: (id: string) => void;
  hasMoreProjects: () => boolean;
  loadMoreProjects: () => void;
  isLoadingMoreProjects: () => boolean;
  isLoading: () => boolean;
  error: () => unknown;
  notice: () => ScopeNotice | undefined;
  dismissNotice: () => void;
};

const ScopeContext = createContext<ScopeContextValue>();

// A newly created Project becomes this tab's scope and the scope new tabs start in.
// It is written directly because the next page mounts a new ScopeProvider.
export function rememberProjectScope(project: Project) {
  const environment = project.environments?.find((candidate) => candidate.is_default) ?? project.environments?.[0];
  const scope = { projectID: project.project, environmentID: environment?.environment ?? "" };
  writeScope(browserStorage("sessionStorage"), scope);
  const last = browserStorage("localStorage");
  writeScope(last, scope);
  if (environment) writeEnvironmentSelections(last, { ...readEnvironmentSelections(last), [project.project]: environment.environment });
}

function resolveEnvironment(
  project: Project | undefined,
  environmentID: string,
  environmentSelections: Record<string, string>,
): Environment | undefined {
  const environments = project?.environments ?? [];
  if (environments.length === 0) return undefined;
  return environments.find((environment) => environment.environment === environmentID) ??
    environments.find((environment) => environment.environment === environmentSelections[project!.project]) ??
    environments.find((environment) => environment.is_default) ??
    environments[0];
}

export function ScopeProvider(props: { children: JSX.Element }) {
  const location = useLocation();
  const navigate = useNavigate();
  const tabStorage = browserStorage("sessionStorage");
  const lastStorage = browserStorage("localStorage");
  const stored = readTabScope(tabStorage, lastStorage);
  // Every route mounts its own provider, so a link's hint is read once here.
  // Pages mount with the linked scope and never load the previous scope first.
  const linkedPath = withoutScopeHint(location);
  const hint = readScopeHint(location.search);
  const [link, setLink] = createSignal<ScopeLink | undefined>(linkedPath === undefined ? undefined : { hint, previous: stored });
  const [selectedProjectID, setSelectedProjectIDState] = createSignal((hint ?? stored).projectID);
  const [selectedEnvironmentID, setSelectedEnvironmentIDState] = createSignal((hint ?? stored).environmentID);
  const [environmentSelections, setEnvironmentSelections] = createSignal(readEnvironmentSelections(lastStorage));
  const [rejectedProjectIDs, setRejectedProjectIDs] = createSignal<ReadonlySet<string>>(new Set());
  const [restored, setRestored] = createSignal(false);
  const projectsQuery = createInfiniteQuery(() => ({
    queryKey: ["projects", "list"],
    queryFn: ({ pageParam }) => listProjects(pageParam || undefined),
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor,
    retry: false,
  }));

  const projects = createMemo(() => projectsQuery.data?.pages.flatMap((page) => page.projects) ?? []);
  // The result names the Project it belongs to, so a result still showing for the
  // previous selection is never mistaken for the current one.
  const projectDetailQuery = createQuery(() => ({
    queryKey: ["projects", "detail", selectedProjectID()],
    queryFn: async ({ queryKey }) => {
      const projectID = queryKey[2]!;
      try {
        return { projectID, project: await getProject(projectID) as Project | undefined };
      } catch (error) {
        if (error instanceof ApiError && error.status === 404) return { projectID, project: undefined };
        throw error;
      }
    },
    enabled: selectedProjectID() !== "",
    retry: false,
  }));
  const projectDetailResult = createMemo(() => {
    const result = projectDetailQuery.data;
    return result?.projectID === selectedProjectID() ? result : undefined;
  });
  const projectDetail = () => projectDetailResult()?.project;
  const projectDetailNotFound = () => !!projectDetailResult() && !projectDetail();
  // The previous scope to restore, at most once, when the link's scope is unavailable.
  const restoreScope = createMemo(() => restored() ? undefined : linkRestoreScope(link(), {
    projectID: selectedProjectID(),
    notFound: projectDetailNotFound(),
    environments: projectDetail()?.environments?.map((environment) => environment.environment) ??
      (projectDetail() ? [] : undefined),
    rejectedProjectIDs: rejectedProjectIDs(),
  }));
  // While a restore is due, nothing resolves, so no page loads the linked or default scope.
  const projectResolution = createMemo(() => restoreScope()
    ? { project: undefined, settled: false }
    : resolveProjectSelection(projects(), selectedProjectID(), projectDetail(), projectDetailNotFound(), rejectedProjectIDs()));
  const selectedProject = createMemo(() => projectResolution().project);
  const projectResolutionSettled = createMemo(() =>
    projectResolution().settled && (selectedProjectID() !== "" || projectsQuery.isSuccess),
  );
  const selectedEnvironment = createMemo(() =>
    resolveEnvironment(selectedProject(), selectedEnvironmentID(), environmentSelections()),
  );
  // Settled once the selected Project's detail, including its Environments, has loaded.
  const settledScope = createMemo((): ScopeIDs | undefined => {
    if (!projectResolutionSettled()) return undefined;
    const project = selectedProject();
    if (!project) return { projectID: "", environmentID: "" };
    if (project.project !== selectedProjectID() || !projectDetail()) return undefined;
    return { projectID: project.project, environmentID: selectedEnvironment()?.environment ?? "" };
  });
  const tabProjectID = () => resolveScopeID(selectedProject()?.project, selectedProjectID(), projectResolutionSettled());
  const tabEnvironmentID = () => resolveScopeID(selectedEnvironment()?.environment, selectedEnvironmentID(), projectResolutionSettled());

  const selectProjectID = (id: string) => {
    batch(() => {
      setSelectedProjectIDState(id);
      setSelectedEnvironmentIDState(environmentSelections()[id] ?? "");
    });
  };

  onMount(() => {
    if (linkedPath !== undefined) navigate(linkedPath, { replace: true, scroll: false });
  });

  createEffect(() => {
    const restore = restoreScope();
    const project = selectedProject();
    const projectID = selectedProjectID();
    const notFound = projectDetailNotFound() && projectID !== "";
    batch(() => {
      if (notFound) {
        setRejectedProjectIDs((current) => current.has(projectID) ? current : new Set([...current, projectID]));
      }
      if (restore) {
        setRestored(true);
        setSelectedProjectIDState(restore.projectID);
        setSelectedEnvironmentIDState(restore.environmentID);
      } else if (project && projectID !== project.project) {
        selectProjectID(project.project);
      }
    });
  });

  createEffect(() => {
    // A fallback Project is only proposed; its Environment applies once it is selected.
    if (selectedProject()?.project !== selectedProjectID()) return;
    const environment = selectedEnvironment();
    if (environment && selectedEnvironmentID() !== environment.environment) {
      setSelectedEnvironmentIDState(environment.environment);
    } else if (!environment && projectDetail() && selectedEnvironmentID()) {
      setSelectedEnvironmentIDState("");
    }
  });

  // Only this tab's storage follows its scope; other tabs keep their own.
  createEffect(() => {
    writeScope(tabStorage, { projectID: tabProjectID(), environmentID: tabEnvironmentID() });
  });

  createEffect(() => {
    const project = selectedProject();
    const environment = selectedEnvironment();
    if (!project || !projectDetail()) return;
    setEnvironmentSelections((current) => {
      if (environment) {
        if (current[project.project] === environment.environment) return current;
        const next = { ...current, [project.project]: environment.environment };
        writeEnvironmentSelections(lastStorage, next);
        return next;
      }
      if (!(project.project in current)) return current;
      const { [project.project]: _removed, ...next } = current;
      writeEnvironmentSelections(lastStorage, next);
      return next;
    });
  });

  // New tabs start in the scope last used: the settled scope of whichever tab
  // is in front, including right after a selection or on returning to a tab.
  const rememberIfVisible = () => {
    const scope = settledScope();
    if (scope?.projectID && document.visibilityState === "visible") writeScope(lastStorage, scope);
  };
  createEffect(rememberIfVisible);
  onMount(() => {
    const onVisibilityChange = () => rememberIfVisible();
    document.addEventListener("visibilitychange", onVisibilityChange);
    window.addEventListener("focus", onVisibilityChange);
    onCleanup(() => {
      document.removeEventListener("visibilitychange", onVisibilityChange);
      window.removeEventListener("focus", onVisibilityChange);
    });
  });

  const notice = createMemo((): ScopeNotice | undefined => {
    const kind = linkNotice(link(), settledScope());
    if (!kind) return undefined;
    return { kind, projectName: selectedProject()?.name ?? "no Project", environmentName: selectedEnvironment()?.name ?? "no Environment" };
  });

  const chooseProjectID = (id: string) => {
    setLink(undefined);
    selectProjectID(id);
  };
  const chooseEnvironmentID = (id: string) => {
    setLink(undefined);
    setSelectedEnvironmentIDState(id);
  };

  return (
    <ScopeContext.Provider value={{
      projects,
      selectedProject,
      selectedEnvironment,
      selectedProjectID: tabProjectID,
      selectedEnvironmentID: tabEnvironmentID,
      setSelectedProjectID: chooseProjectID,
      setSelectedEnvironmentID: chooseEnvironmentID,
      hasMoreProjects: () => projectsQuery.hasNextPage,
      loadMoreProjects: () => { void projectsQuery.fetchNextPage(); },
      isLoadingMoreProjects: () => projectsQuery.isFetchingNextPage,
      isLoading: () => projectsQuery.isPending || (selectedProjectID() !== "" && !projectDetailResult() && !projectDetailQuery.isError),
      error: () => projectsQuery.error ?? projectDetailQuery.error,
      notice,
      dismissNotice: () => setLink(undefined),
    }}>
      {props.children}
    </ScopeContext.Provider>
  );
}

export function useScope() {
  const value = useContext(ScopeContext);
  if (!value) throw new Error("ScopeProvider is missing");
  return value;
}

// The tab's scope as the IDs API calls take.
export function useScopeIDs(): () => ScopeIDs {
  const scope = useScope();
  return () => ({ projectID: scope.selectedProjectID(), environmentID: scope.selectedEnvironmentID() });
}
