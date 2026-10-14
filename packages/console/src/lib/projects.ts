import { postJson, request } from "./api";

export type ProjectSettings = { history_retention: string };

export type Environment = {
  environment: string;
  project: string;
  name: string;
  color_hex: string;
  is_default: boolean;
  created_at: string;
  updated_at: string;
};

export type Project = {
  project: string;
  name: string;
  default_region_id: string;
  is_default: boolean;
  created_at: string;
  updated_at: string;
  environments?: Environment[];
};

type ListProjectsResponse = {
  projects: Project[];
  next_cursor?: string;
};

type Region = {
  id: string;
  display_name: string;
  location?: string;
};

type ListRegionsResponse = {
  regions: Region[];
};

type CreateProjectInput = {
  project: string;
  name: string;
  default_region_id?: string;
};

type UpdateProjectInput = {
  name: string;
};

type CreateEnvironmentInput = {
  environment: string;
  name: string;
  color_hex: string;
};

type UpdateEnvironmentInput = {
  name: string;
  color_hex: string;
};

function resolveProject(projects: Project[], projectID: string): Project | undefined {
  return projects.find((project) => project.project === projectID) ??
    projects.find((project) => project.is_default) ??
    projects[0];
}

export function resolveProjectSelection(
  projects: Project[],
  projectID: string,
  detail: Project | undefined,
  detailNotFound: boolean,
  rejectedProjectIDs: ReadonlySet<string> = new Set(),
): { project: Project | undefined; settled: boolean } {
  if (!projectID) return { project: resolveProject(projects, ""), settled: true };
  if (detailNotFound) {
    return {
      project: resolveProject(
        projects.filter((project) => project.project !== projectID && !rejectedProjectIDs.has(project.project)),
        "",
      ),
      settled: true,
    };
  }
  if (detail?.project === projectID) return { project: detail, settled: true };
  return { project: undefined, settled: false };
}

export function resolveScopeID(resolvedID: string | undefined, requestedID: string, settled: boolean): string {
  return resolvedID ?? (settled ? "" : requestedID);
}

export async function listProjects(cursor?: string, limit?: number): Promise<ListProjectsResponse> {
  const params = new URLSearchParams();
  if (cursor) params.set("cursor", cursor);
  if (limit) params.set("limit", String(limit));
  const query = params.toString();
  return request<ListProjectsResponse>(`/api/projects${query ? `?${query}` : ""}`);
}

export async function getProject(projectRef: string): Promise<Project> {
  return request<Project>(`/api/projects/${encodeURIComponent(projectRef)}`);
}

export async function listRegions(): Promise<ListRegionsResponse> {
  return request<ListRegionsResponse>("/api/regions");
}

export async function createProject(input: CreateProjectInput): Promise<Project> {
  return postJson<CreateProjectInput, Project>("/api/projects", input);
}

export async function updateProject(projectID: string, input: UpdateProjectInput): Promise<Project> {
  return request<Project>(
    `/api/projects/${encodeURIComponent(projectID)}`,
    { method: "PATCH", body: JSON.stringify(input) },
  );
}

export async function createEnvironment(projectID: string, input: CreateEnvironmentInput): Promise<Environment> {
  return postJson<CreateEnvironmentInput, Environment>(
    `/api/projects/${encodeURIComponent(projectID)}/environments`,
    input,
  );
}

export async function updateEnvironment(
  projectID: string,
  environmentID: string,
  input: UpdateEnvironmentInput,
): Promise<Environment> {
  return request<Environment>(
    `/api/projects/${encodeURIComponent(projectID)}/environments/${encodeURIComponent(environmentID)}`,
    { method: "PATCH", body: JSON.stringify(input) },
  );
}

export function getProjectSettings(project: string): Promise<ProjectSettings> {
  return request<ProjectSettings>(`/api/projects/${encodeURIComponent(project)}/settings`);
}
export function updateProjectSettings(project: string, input: ProjectSettings): Promise<ProjectSettings> {
  return request<ProjectSettings>(`/api/projects/${encodeURIComponent(project)}/settings`, { method: "PATCH", body: JSON.stringify(input) });
}

const PROTECTED_ENVIRONMENT_IDS = new Set(["production", "staging"]);

// Protected Environments carry a warning stripe in the bar and a "protected" tag.
export function isProtectedEnvironment(environment: Pick<Environment, "environment">): boolean {
  return PROTECTED_ENVIRONMENT_IDS.has(environment.environment);
}
