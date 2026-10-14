import { request } from "./api";

type AgentListItem = { agent: string; template: string };
export type TemplateListItem = { template: string; resources: { milliCpu: number; memoryMiB: number; diskMiB?: number } };
export type DefinitionListItem = AgentListItem | TemplateListItem;
export function definitionName(item: DefinitionListItem): string { return "agent" in item ? item.agent : item.template; }
export type AgentDefinition = AgentListItem & { deployment_id: string };
export type TemplateDefinition = TemplateListItem & { deployment_id: string };

type ListAgentDefinitionsResponse = {
  deployment_id: string;
  agents: AgentListItem[];
  next_cursor?: string;
};

type ListTemplateDefinitionsResponse = {
  deployment_id: string;
  templates: TemplateListItem[];
  next_cursor?: string;
};

export type DefinitionScope = {
  projectID: string;
  environmentID: string;
};

type DefinitionListOptions = {
  projectID: string;
  environmentID: string;
  deploymentID?: string | undefined;
  cursor?: string | undefined;
  limit?: number | undefined;
};

export async function listAgentDefinitions(options: DefinitionListOptions): Promise<ListAgentDefinitionsResponse> {
  return request<ListAgentDefinitionsResponse>(definitionPath("agents", options));
}

export async function listTemplateDefinitions(options: DefinitionListOptions): Promise<ListTemplateDefinitionsResponse> {
  return request<ListTemplateDefinitionsResponse>(definitionPath("templates", options));
}

function definitionPath(kind: "agents" | "templates", options: DefinitionScope & Partial<DefinitionListOptions>): string {
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
  return request(definitionItemPath("agents", id, options));
}
export function getTemplateDefinition(id: string, options: DefinitionScope & { deploymentID?: string | undefined }): Promise<TemplateDefinition> {
  return request(definitionItemPath("templates", id, options));
}
function definitionItemPath(kind: "agents" | "templates", id: string, options: DefinitionScope & { deploymentID?: string | undefined }): string {
  const base = definitionPath(kind, { projectID: options.projectID, environmentID: options.environmentID });
  const query = options.deploymentID ? `?${new URLSearchParams({ deployment_id: options.deploymentID })}` : "";
  return `${base}/${encodeURIComponent(id)}${query}`;
}
export type DefinitionKind = "agents" | "templates";
export function definitionHref(kind: DefinitionKind, id: string, deploymentID: string): string {
  return `/deployments/${encodeURIComponent(deploymentID)}/${kind}/${encodeURIComponent(id)}`;
}

type RetainedAgent = { agent: string; current_deployment_id: string | null; defined_in_current: boolean };
export function agentHref(agent: string): string {
  return `/agents/${encodeURIComponent(agent)}`;
}
export function listRetainedAgents(scope: DefinitionScope, cursor?: string): Promise<{ agents: RetainedAgent[]; next_cursor?: string }> {
  const query = new URLSearchParams({ view: "all", limit: "100" });
  if (cursor) query.set("cursor", cursor);
  return request(`${definitionPath("agents", scope)}?${query}`);
}
export function getAgentStatus(agent: string, scope: DefinitionScope): Promise<RetainedAgent> {
  return request(`${definitionItemPath("agents", agent, scope)}/status`);
}
