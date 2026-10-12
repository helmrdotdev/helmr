import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { useSearchParams } from "@solidjs/router";
import { batch, createContext, createEffect, createMemo, createSignal, on, useContext, type JSX } from "solid-js";
import { ApiError } from "./api";
import {
  getProject,
  listProjects,
  resolveProjectSelection,
  resolveScopeID,
  type Environment,
  type Project,
} from "./projects";

const PROJECT_STORAGE_KEY = "helmr.project";
const ENVIRONMENT_STORAGE_KEY = "helmr.environment";
const ENVIRONMENT_BY_PROJECT_STORAGE_KEY = "helmr.environment_by_project";

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
};

const ScopeContext = createContext<ScopeContextValue>();

function readEnvironmentSelections(): Record<string, string> {
  try {
    const value = localStorage.getItem(ENVIRONMENT_BY_PROJECT_STORAGE_KEY);
    if (!value) return {};
    const parsed: unknown = JSON.parse(value);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return {};
    return Object.fromEntries(
      Object.entries(parsed).filter((entry): entry is [string, string] =>
        typeof entry[0] === "string" && typeof entry[1] === "string",
      ),
    );
  } catch {
    return {};
  }
}

function writeEnvironmentSelection(projectID: string, environmentID: string) {
  const next = { ...readEnvironmentSelections(), [projectID]: environmentID };
  localStorage.setItem(ENVIRONMENT_BY_PROJECT_STORAGE_KEY, JSON.stringify(next));
}

export function rememberProjectScope(project: Project) {
  localStorage.setItem(PROJECT_STORAGE_KEY, project.project);
  const environment = project.environments?.find((candidate) => candidate.is_default) ?? project.environments?.[0];
  if (environment) {
    localStorage.setItem(ENVIRONMENT_STORAGE_KEY, environment.environment);
    writeEnvironmentSelection(project.project, environment.environment);
  } else {
    localStorage.removeItem(ENVIRONMENT_STORAGE_KEY);
  }
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
  const [search] = useSearchParams();
  const routeValue = (key: string) => typeof search[key] === "string" ? search[key] as string : "";
  const [selectedProjectID, setSelectedProjectIDState] = createSignal(routeValue("project") || localStorage.getItem(PROJECT_STORAGE_KEY) || "");
  const [selectedEnvironmentID, setSelectedEnvironmentIDState] = createSignal(routeValue("environment") || localStorage.getItem(ENVIRONMENT_STORAGE_KEY) || "");
  const [environmentSelections, setEnvironmentSelections] = createSignal(readEnvironmentSelections());
  const [rejectedProjectIDs, setRejectedProjectIDs] = createSignal<ReadonlySet<string>>(new Set());
  const projectsQuery = createInfiniteQuery(() => ({
    queryKey: ["projects", "list"],
    queryFn: ({ pageParam }) => listProjects(pageParam || undefined),
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor,
    retry: false,
  }));

  const projects = createMemo(() => projectsQuery.data?.pages.flatMap((page) => page.projects) ?? []);
  const projectDetailQuery = createQuery(() => ({
    queryKey: ["projects", "detail", selectedProjectID()],
    queryFn: () => getProject(selectedProjectID()),
    enabled: selectedProjectID() !== "",
    retry: false,
  }));
  const projectDetailNotFound = createMemo(() =>
    projectDetailQuery.error instanceof ApiError && projectDetailQuery.error.status === 404,
  );
  const projectResolution = createMemo(() => resolveProjectSelection(
    projects(), selectedProjectID(), projectDetailQuery.data, projectDetailNotFound(), rejectedProjectIDs(),
  ));
  const selectedProject = createMemo(() => projectResolution().project);
  const projectResolutionSettled = createMemo(() =>
    projectResolution().settled && (selectedProjectID() !== "" || projectsQuery.isSuccess),
  );
  const selectedEnvironment = createMemo(() =>
    resolveEnvironment(selectedProject(), selectedEnvironmentID(), environmentSelections()),
  );

  const selectProjectID = (id: string) => {
    batch(() => {
      setSelectedProjectIDState(id);
      setSelectedEnvironmentIDState(environmentSelections()[id] ?? "");
    });
  };

  // Deep links establish the same scope shown by the global selector. Explicit
  // selector changes navigate away from old resource/query identities.
  createEffect(on(() => [routeValue("project"), routeValue("environment")] as const, ([project, environment]) => {
    batch(() => {
      if (project) selectProjectID(project);
      if (environment) setSelectedEnvironmentIDState(environment);
    });
  }, { defer: true }));

  createEffect(() => {
    const project = selectedProject();
    const projectID = selectedProjectID();
    if (projectDetailNotFound() && projectID) {
      batch(() => {
        setRejectedProjectIDs((current) => {
          if (current.has(projectID)) return current;
          return new Set([...current, projectID]);
        });
        if (project) selectProjectID(project.project);
      });
    } else if (project && projectID !== project.project) {
      selectProjectID(project.project);
    }
  });

  createEffect(() => {
    const environment = selectedEnvironment();
    if (environment && selectedEnvironmentID() !== environment.environment) {
      setSelectedEnvironmentIDState(environment.environment);
    } else if (!environment && projectDetailQuery.isSuccess && selectedEnvironmentID()) {
      setSelectedEnvironmentIDState("");
    }
  });

  createEffect(() => {
    const projectID = selectedProjectID();
    if (projectDetailNotFound() && projectResolutionSettled()) {
      const fallbackID = selectedProject()?.project;
      if (fallbackID) {
        localStorage.setItem(PROJECT_STORAGE_KEY, fallbackID);
      } else {
        localStorage.removeItem(PROJECT_STORAGE_KEY);
      }
      return;
    }
    if (projectID) {
      localStorage.setItem(PROJECT_STORAGE_KEY, projectID);
    } else if (projectsQuery.isSuccess && projectResolutionSettled()) {
      localStorage.removeItem(PROJECT_STORAGE_KEY);
    }
  });

  createEffect(() => {
    const project = selectedProject();
    const environment = selectedEnvironment();
    if (!project) {
      if (projectsQuery.isSuccess && projectResolutionSettled()) localStorage.removeItem(ENVIRONMENT_STORAGE_KEY);
      return;
    }
    if (!projectDetailQuery.isSuccess) return;
    if (!environment) {
      localStorage.removeItem(ENVIRONMENT_STORAGE_KEY);
      setEnvironmentSelections((current) => {
        if (!(project.project in current)) return current;
        const { [project.project]: _removed, ...next } = current;
        localStorage.setItem(ENVIRONMENT_BY_PROJECT_STORAGE_KEY, JSON.stringify(next));
        return next;
      });
      return;
    }
    localStorage.setItem(ENVIRONMENT_STORAGE_KEY, environment.environment);
    setEnvironmentSelections((current) => {
      if (current[project.project] === environment.environment) return current;
      const next = { ...current, [project.project]: environment.environment };
      localStorage.setItem(ENVIRONMENT_BY_PROJECT_STORAGE_KEY, JSON.stringify(next));
      return next;
    });
  });

  return (
    <ScopeContext.Provider value={{
      projects,
      selectedProject,
      selectedEnvironment,
      selectedProjectID: () => resolveScopeID(selectedProject()?.project, selectedProjectID(), projectResolutionSettled()),
      selectedEnvironmentID: () => resolveScopeID(selectedEnvironment()?.environment, selectedEnvironmentID(), projectResolutionSettled()),
      setSelectedProjectID: selectProjectID,
      setSelectedEnvironmentID: setSelectedEnvironmentIDState,
      hasMoreProjects: () => projectsQuery.hasNextPage,
      loadMoreProjects: () => { void projectsQuery.fetchNextPage(); },
      isLoadingMoreProjects: () => projectsQuery.isFetchingNextPage,
      isLoading: () => projectsQuery.isPending || (selectedProjectID() !== "" && projectDetailQuery.isPending),
      error: () => projectsQuery.error ?? projectDetailQuery.error,
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

export function useRouteScope() {
  const scope = useScope();
  const [search] = useSearchParams();
  const value = (key: string) => typeof search[key] === "string" ? search[key] as string : "";
  return () => ({
    projectID: value("project") || scope.selectedProjectID(),
    environmentID: value("environment") || scope.selectedEnvironmentID(),
  });
}
