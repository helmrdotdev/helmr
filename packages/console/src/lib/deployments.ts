import { ApiError, postJson, request } from "./api";
type DeploymentEvent = {
  id: string;
  kind: string;
  message: string;
  at: string;
  attributes: unknown;
};

export type Deployment = {
  id: string;
  version: string;
  bundle_digest: string;
  created_at: string;
  preparation_state?: "ready" | "preparing" | "failed" | "unprepared" | "unavailable";
};

type ListDeploymentsResponse = {
  deployments: Deployment[];
  next_cursor?: string;
};

type DeploymentEventPage = {
  events: DeploymentEvent[];
  next_cursor?: string | null;
};

type DeploymentScope = {
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

export async function promoteDeployment(id: string, scope: DeploymentScope, signal?: AbortSignal): Promise<Deployment> {
  const path = `${deploymentsPath(scope)}/${encodeURIComponent(id)}`;
  const options = signal ? { signal } : {};
  for (;;) {
    signal?.throwIfAborted();
    let result = await postJson<Record<string, never>, Deployment>(`${path}/promote`, {}, options);
    if (result.preparation_state !== "preparing") return result;
    while (result.preparation_state === "preparing") {
      await new Promise(resolve => setTimeout(resolve, 1000));
      signal?.throwIfAborted();
      result = await request<Deployment>(path, options);
    }
    if (result.preparation_state !== "ready" && result.preparation_state !== "unprepared") {
      throw new Error(`Environment preparation ${result.preparation_state}. The current deployment has not changed.`);
    }
  }
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
