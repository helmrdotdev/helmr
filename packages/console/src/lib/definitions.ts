import { postJson, request } from "./api";

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

export type StartTaskInput = {
  workspace: { id: string };
  payload?: unknown;
  idempotency_key: string;
};

export type StartActorInput = {
  workspace: { id: string };
  key?: string;
  input?: unknown;
  idempotency_key: string;
};

export type DefinitionScope = {
  projectID: string;
  environmentID: string;
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

export async function startTask(
  taskID: string,
  scope: DefinitionScope,
  input: StartTaskInput,
): Promise<{ run_id: string }> {
  return postJson<StartTaskInput, { run_id: string }>(
    `${definitionPath("tasks", scope)}/${encodeURIComponent(taskID)}/start`,
    input,
  );
}

export async function startActor(
  actorID: string,
  scope: DefinitionScope,
  input: StartActorInput,
): Promise<{ session_id: string; run_id: string }> {
  return postJson<StartActorInput, { session_id: string; run_id: string }>(
    `${definitionPath("actors", scope)}/${encodeURIComponent(actorID)}/start`,
    input,
  );
}

function definitionPath(kind: "tasks" | "actors" | "sandboxes", options: DefinitionScope & Partial<DefinitionListOptions>): string {
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
