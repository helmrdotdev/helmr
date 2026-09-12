import { ApiError, postJson, request } from "./api";
import type { RunEventRecord } from "./runs";

export type Deployment = {
  id: string;
  version: string;
  bundle_digest: string;
  created_at: string;
};

export type ListDeploymentsResponse = {
  deployments: Deployment[];
  next_cursor?: string;
};

export type DeploymentEventPage = {
  events: RunEventRecord[];
  next_cursor?: string | null;
};

export type DeploymentScope = {
  projectID: string;
  environmentID: string;
};

export async function getCurrentDeployment(scope: DeploymentScope): Promise<Deployment | null> {
  try {
    return await request<Deployment>(`${deploymentsPath(scope)}/current`);
  } catch (error) {
    if (error instanceof ApiError && error.code === "no_current_deployment") return null;
    throw error;
  }
}

export async function listDeployments(
  scope: DeploymentScope,
  options: { cursor?: string | undefined; limit?: number | undefined } = {},
): Promise<ListDeploymentsResponse> {
  const params = new URLSearchParams();
  if (options.cursor) params.set("cursor", options.cursor);
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  const query = params.size === 0 ? "" : `?${params.toString()}`;
  return request<ListDeploymentsResponse>(`${deploymentsPath(scope)}${query}`);
}

export async function getDeployment(id: string, scope: DeploymentScope): Promise<Deployment> {
  return request<Deployment>(`${deploymentsPath(scope)}/${encodeURIComponent(id)}`);
}

export async function promoteDeployment(id: string, scope: DeploymentScope): Promise<Deployment> {
  return postJson<Record<string, never>, Deployment>(
    `${deploymentsPath(scope)}/${encodeURIComponent(id)}/promote`,
    {},
  );
}

export async function getDeploymentEvents(
  id: string,
  scope: DeploymentScope,
  options: { cursor?: string | undefined; limit?: number | undefined } = {},
): Promise<DeploymentEventPage> {
  const params = new URLSearchParams();
  if (options.cursor) params.set("cursor", options.cursor);
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  const query = params.size === 0 ? "" : `?${params.toString()}`;
  return request<DeploymentEventPage>(`${deploymentsPath(scope)}/${encodeURIComponent(id)}/events${query}`);
}

function deploymentsPath(scope: DeploymentScope): string {
  if (!scope.projectID || !scope.environmentID) {
    throw new Error("project and environment are required");
  }
  return `/api/projects/${encodeURIComponent(scope.projectID)}/environments/${encodeURIComponent(scope.environmentID)}/deployments`;
}
