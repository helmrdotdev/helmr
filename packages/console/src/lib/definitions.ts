import { request } from "./api";

export type DefinitionListItem = {
  id: string;
};

export type ListTasksResponse = {
  deployment_id: string;
  tasks: DefinitionListItem[];
  next_cursor?: string;
};

export type ListActorsResponse = {
  deployment_id: string;
  actors: DefinitionListItem[];
  next_cursor?: string;
};

export type ListSandboxesResponse = {
  deployment_id: string;
  sandboxes: DefinitionListItem[];
  next_cursor?: string;
};

export type DefinitionListOptions = {
  projectID: string;
  environmentID: string;
  deploymentID?: string | undefined;
  cursor?: string | undefined;
  limit?: number | undefined;
};

export async function listTasks(options: DefinitionListOptions): Promise<ListTasksResponse> {
  return request<ListTasksResponse>(definitionPath("tasks", options));
}

export async function listActors(options: DefinitionListOptions): Promise<ListActorsResponse> {
  return request<ListActorsResponse>(definitionPath("actors", options));
}

export async function listSandboxes(options: DefinitionListOptions): Promise<ListSandboxesResponse> {
  return request<ListSandboxesResponse>(definitionPath("sandboxes", options));
}

function definitionPath(kind: "tasks" | "actors" | "sandboxes", options: DefinitionListOptions): string {
  if (!options.projectID || !options.environmentID) {
    throw new Error("project and environment are required");
  }
  const params = new URLSearchParams();
  if (options.deploymentID) params.set("deployment_id", options.deploymentID);
  if (options.cursor) params.set("cursor", options.cursor);
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  const query = params.size === 0 ? "" : `?${params.toString()}`;
  return `/api/projects/${encodeURIComponent(options.projectID)}/environments/${encodeURIComponent(options.environmentID)}/${kind}${query}`;
}
