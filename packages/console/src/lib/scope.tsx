import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { batch, createContext, createEffect, createMemo, createSignal, useContext, type JSX } from "solid-js";
import { ApiError } from "./api";
import {
  getProject,
  listProjects,
  resolveProjectSelection,
  resolveScopeID,
  type Environment,
  type Project,
} from "./projects";

const PROJECT_STORAGE_KEY = "helmr.project_id";
const ENVIRONMENT_STORAGE_KEY = "helmr.environment_id";
const ENVIRONMENT_BY_PROJECT_STORAGE_KEY = "helmr.environment_id_by_project";

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
  localStorage.setItem(PROJECT_STORAGE_KEY, project.id);
  const environment = project.environments?.find((candidate) => candidate.is_default) ?? project.environments?.[0];
  if (environment) {
    localStorage.setItem(ENVIRONMENT_STORAGE_KEY, environment.id);
    writeEnvironmentSelection(project.id, environment.id);
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
  return environments.find((environment) => environment.id === environmentID) ??
    environments.find((environment) => environment.id === environmentSelections[project!.id]) ??
    environments.find((environment) => environment.is_default) ??
    environments[0];
}

export function ScopeProvider(props: { children: JSX.Element }) {
  const [selectedProjectID, setSelectedProjectIDState] = createSignal(localStorage.getItem(PROJECT_STORAGE_KEY) ?? "");
  const [selectedEnvironmentID, setSelectedEnvironmentIDState] = createSignal(localStorage.getItem(ENVIRONMENT_STORAGE_KEY) ?? "");
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

  createEffect(() => {
    const project = selectedProject();
    const projectID = selectedProjectID();
    if (projectDetailNotFound() && projectID) {
      batch(() => {
        setRejectedProjectIDs((current) => {
          if (current.has(projectID)) return current;
          return new Set([...current, projectID]);
        });
        if (project) selectProjectID(project.id);
      });
    } else if (project && projectID !== project.id) {
      selectProjectID(project.id);
    }
  });

  createEffect(() => {
    const environment = selectedEnvironment();
    if (environment && selectedEnvironmentID() !== environment.id) {
      setSelectedEnvironmentIDState(environment.id);
    } else if (!environment && projectDetailQuery.isSuccess && selectedEnvironmentID()) {
      setSelectedEnvironmentIDState("");
    }
  });

  createEffect(() => {
    const projectID = selectedProjectID();
    if (projectDetailNotFound() && projectResolutionSettled()) {
      const fallbackID = selectedProject()?.id;
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
        if (!(project.id in current)) return current;
        const { [project.id]: _removed, ...next } = current;
        localStorage.setItem(ENVIRONMENT_BY_PROJECT_STORAGE_KEY, JSON.stringify(next));
        return next;
      });
      return;
    }
    localStorage.setItem(ENVIRONMENT_STORAGE_KEY, environment.id);
    setEnvironmentSelections((current) => {
      if (current[project.id] === environment.id) return current;
      const next = { ...current, [project.id]: environment.id };
      localStorage.setItem(ENVIRONMENT_BY_PROJECT_STORAGE_KEY, JSON.stringify(next));
      return next;
    });
  });

  return (
    <ScopeContext.Provider value={{
      projects,
      selectedProject,
      selectedEnvironment,
      selectedProjectID: () => resolveScopeID(selectedProject()?.id, selectedProjectID(), projectResolutionSettled()),
      selectedEnvironmentID: () => resolveScopeID(selectedEnvironment()?.id, selectedEnvironmentID(), projectResolutionSettled()),
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
