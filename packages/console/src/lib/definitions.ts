import { request } from "./api";

export type DefinitionListItem = {
  id: string;
};

export type AgentDefinitionListItem = DefinitionListItem & { agent_id: string };

export type ListAgentDefinitionsResponse = {
  deployment_id: string;
  agent_definitions: AgentDefinitionListItem[];
  next_cursor?: string;
};

export type ListComputerDefinitionsResponse = {
  deployment_id: string;
  computer_definitions: DefinitionListItem[];
  next_cursor?: string;
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

export async function listAgentDefinitions(options: DefinitionListOptions): Promise<ListAgentDefinitionsResponse> {
  return request<ListAgentDefinitionsResponse>(definitionPath("agent-definitions", options));
}

export async function listComputerDefinitions(options: DefinitionListOptions): Promise<ListComputerDefinitionsResponse> {
  return request<ListComputerDefinitionsResponse>(definitionPath("computer-definitions", options));
}

function definitionPath(kind: "agent-definitions" | "computer-definitions", options: DefinitionScope & Partial<DefinitionListOptions>): string {
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
