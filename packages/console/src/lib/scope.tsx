import { createQuery } from "@tanstack/solid-query";
import { createContext, createEffect, createMemo, createSignal, useContext, type JSX } from "solid-js";
import { listProjects, type Environment, type Project } from "./projects";

const PROJECT_STORAGE_KEY = "helmr.project_id";
const ENVIRONMENT_STORAGE_KEY = "helmr.environment_id";

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

export function ScopeProvider(props: { children: JSX.Element }) {
  const [selectedProjectID, setSelectedProjectIDState] = createSignal(localStorage.getItem(PROJECT_STORAGE_KEY) ?? "");
  const [selectedEnvironmentID, setSelectedEnvironmentIDState] = createSignal(localStorage.getItem(ENVIRONMENT_STORAGE_KEY) ?? "");
  const projectsQuery = createQuery(() => ({
    queryKey: ["projects"],
    queryFn: listProjects,
    retry: false,
  }));

  const projects = createMemo(() => projectsQuery.data?.projects ?? []);
  const selectedProject = createMemo(() => {
    const currentID = selectedProjectID();
    return projects().find((project) => project.id === currentID) ?? projects()[0];
  });
  const selectedEnvironment = createMemo(() => {
    const project = selectedProject();
    const environments = project?.environments ?? [];
    const currentID = selectedEnvironmentID();
    return environments.find((environment) => environment.id === currentID) ?? environments[0];
  });

  createEffect(() => {
    const project = selectedProject();
    if (project && selectedProjectID() !== project.id) {
      setSelectedProjectIDState(project.id);
    }
  });

  createEffect(() => {
    const environment = selectedEnvironment();
    if (environment && selectedEnvironmentID() !== environment.id) {
      setSelectedEnvironmentIDState(environment.id);
    }
  });

  createEffect(() => {
    const projectID = selectedProjectID();
    if (projectID) localStorage.setItem(PROJECT_STORAGE_KEY, projectID);
  });

  createEffect(() => {
    const environmentID = selectedEnvironmentID();
    if (environmentID) localStorage.setItem(ENVIRONMENT_STORAGE_KEY, environmentID);
  });

  const setSelectedProjectID = (id: string) => {
    setSelectedProjectIDState(id);
    const project = projects().find((candidate) => candidate.id === id);
    setSelectedEnvironmentIDState(project?.environments?.[0]?.id ?? "");
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
