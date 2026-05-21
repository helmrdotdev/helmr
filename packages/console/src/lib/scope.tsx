import { createQuery } from "@tanstack/solid-query";
import { createContext, createEffect, createMemo, createSignal, useContext, type JSX } from "solid-js";
import { listProjects, type Environment, type Project } from "./projects";

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

function resolveProject(projects: Project[], projectID: string): Project | undefined {
  return projects.find((project) => project.id === projectID) ??
    projects.find((project) => project.is_default) ??
    projects[0];
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
  const projectsQuery = createQuery(() => ({
    queryKey: ["projects"],
    queryFn: listProjects,
    retry: false,
  }));

  const projects = createMemo(() => projectsQuery.data?.projects ?? []);
  const selectedProject = createMemo(() => resolveProject(projects(), selectedProjectID()));
  const selectedEnvironment = createMemo(() =>
    resolveEnvironment(selectedProject(), selectedEnvironmentID(), environmentSelections()),
  );

  createEffect(() => {
    const project = selectedProject();
    if (project && selectedProjectID() !== project.id) {
      setSelectedProjectIDState(project.id);
    } else if (!project && projectsQuery.isSuccess && selectedProjectID()) {
      setSelectedProjectIDState("");
    }
  });

  createEffect(() => {
    const environment = selectedEnvironment();
    if (environment && selectedEnvironmentID() !== environment.id) {
      setSelectedEnvironmentIDState(environment.id);
    } else if (!environment && projectsQuery.isSuccess && selectedEnvironmentID()) {
      setSelectedEnvironmentIDState("");
    }
  });

  createEffect(() => {
    const projectID = selectedProjectID();
    if (projectID) {
      localStorage.setItem(PROJECT_STORAGE_KEY, projectID);
    } else if (projectsQuery.isSuccess) {
      localStorage.removeItem(PROJECT_STORAGE_KEY);
    }
  });

  createEffect(() => {
    const project = selectedProject();
    const environment = selectedEnvironment();
    if (!project) {
      if (projectsQuery.isSuccess) localStorage.removeItem(ENVIRONMENT_STORAGE_KEY);
      return;
    }
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

  const setSelectedProjectID = (id: string) => {
    setSelectedProjectIDState(id);
    const project = projects().find((candidate) => candidate.id === id);
    const environment = resolveEnvironment(project, "", environmentSelections());
    setSelectedEnvironmentIDState(environment?.id ?? "");
  };

  return (
    <ScopeContext.Provider value={{
      projects,
      selectedProject,
      selectedEnvironment,
      selectedProjectID: () => selectedProject()?.id ?? "",
      selectedEnvironmentID: () => selectedEnvironment()?.id ?? "",
      setSelectedProjectID,
      setSelectedEnvironmentID: setSelectedEnvironmentIDState,
      isLoading: () => projectsQuery.isPending,
      error: () => projectsQuery.error,
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
