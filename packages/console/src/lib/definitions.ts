import { request } from "./api";

export type DefinitionListItem = {
  id: string;
};

export type AgentDefinitionListItem = DefinitionListItem & { agent_id: string; template_id: string };
export type TemplateDefinitionListItem = DefinitionListItem & { resources: { milliCpu: number; memoryMiB: number; diskMiB?: number } };
export type AgentDefinition = AgentDefinitionListItem & { deployment_id: string };
export type TemplateDefinition = TemplateDefinitionListItem & { deployment_id: string };

export type ListAgentDefinitionsResponse = {
  deployment_id: string;
  agent_definitions: AgentDefinitionListItem[];
  next_cursor?: string;
};

export type ListTemplateDefinitionsResponse = {
  deployment_id: string;
  template_definitions: TemplateDefinitionListItem[];
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

export async function listTemplateDefinitions(options: DefinitionListOptions): Promise<ListTemplateDefinitionsResponse> {
  return request<ListTemplateDefinitionsResponse>(definitionPath("template-definitions", options));
}

function definitionPath(kind: "agent-definitions" | "template-definitions", options: DefinitionScope & Partial<DefinitionListOptions>): string {
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

export function getAgentDefinition(id: string, options: DefinitionScope & { deploymentID?: string | undefined }): Promise<AgentDefinition> {
  return request(definitionItemPath("agent-definitions", id, options));
}
export function getTemplateDefinition(id: string, options: DefinitionScope & { deploymentID?: string | undefined }): Promise<TemplateDefinition> {
  return request(definitionItemPath("template-definitions", id, options));
}
function definitionItemPath(kind: "agent-definitions" | "template-definitions", id: string, options: DefinitionScope & { deploymentID?: string | undefined }): string {
  const base = definitionPath(kind, { projectID: options.projectID, environmentID: options.environmentID });
  const query = options.deploymentID ? `?${new URLSearchParams({ deployment_id: options.deploymentID })}` : "";
  return `${base}/${encodeURIComponent(id)}${query}`;
}
export type DefinitionKind = "agents" | "templates";
export function definitionHref(kind: DefinitionKind, id: string, options: DefinitionScope & { deploymentID: string }): string {
  const query = new URLSearchParams({ project_id: options.projectID, environment_id: options.environmentID, deployment_id: options.deploymentID });
  return `/${kind}/${encodeURIComponent(id)}?${query}`;
}
export function scopedConsoleHref(path: string, options: DefinitionScope): string {
  return `${path}?${new URLSearchParams({ project_id: options.projectID, environment_id: options.environmentID })}`;
}
