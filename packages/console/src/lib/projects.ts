import { del, postJson, request } from "./api";

export type Environment = {
  id: string;
  project_id: string;
  slug: string;
  name: string;
  is_default: boolean;
  created_at: string;
  updated_at: string;
};

export type Project = {
  id: string;
  slug: string;
  name: string;
  is_default: boolean;
  created_at: string;
  updated_at: string;
  environments?: Environment[];
};

export type ListProjectsResponse = {
  projects: Project[];
};

export type CreateProjectInput = {
  slug: string;
  name: string;
};

export type CreateEnvironmentInput = {
  slug: string;
  name: string;
};

export async function listProjects(): Promise<ListProjectsResponse> {
  return request<ListProjectsResponse>("/api/projects");
}

export async function createProject(input: CreateProjectInput): Promise<Project> {
  return postJson<CreateProjectInput, Project>("/api/projects", input);
}

export async function updateProject(projectID: string, input: CreateProjectInput): Promise<Project> {
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

export async function deleteProject(projectID: string): Promise<void> {
  await del<void>(`/api/projects/${encodeURIComponent(projectID)}`);
}

export async function deleteEnvironment(projectID: string, environmentID: string): Promise<void> {
  await del<void>(
    `/api/projects/${encodeURIComponent(projectID)}/environments/${encodeURIComponent(environmentID)}`,
  );
}
