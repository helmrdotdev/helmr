import { postJson, request } from "./api";

export type Environment = {
  id: string;
  project_id: string;
  slug: string;
  name: string;
  color_hex: string;
  is_default: boolean;
  created_at: string;
  updated_at: string;
};

export type Project = {
  id: string;
  slug: string;
  name: string;
  default_region_id: string;
  is_default: boolean;
  created_at: string;
  updated_at: string;
  environments?: Environment[];
};

export type ListProjectsResponse = {
  projects: Project[];
  next_cursor?: string;
};

export type Region = {
  id: string;
  display_name: string;
  location?: string;
};

export type ListRegionsResponse = {
  regions: Region[];
};

export type CreateProjectInput = {
  slug: string;
  name: string;
  default_region_id?: string;
};

export type UpdateProjectInput = {
  slug: string;
  name: string;
};

export type CreateEnvironmentInput = {
  slug: string;
  name: string;
  color_hex: string;
};

function resolveProject(projects: Project[], projectID: string): Project | undefined {
  return projects.find((project) => project.id === projectID) ??
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
        projects.filter((project) => project.id !== projectID && !rejectedProjectIDs.has(project.id)),
        "",
      ),
      settled: true,
    };
  }
  if (detail?.id === projectID) return { project: detail, settled: true };
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
