import { ApiError, del, postJson, request } from "./api";

type ApiKeyStatus = "active" | "expired" | "revoked";

export type ApiKeyScope = "agents.use" | "sessions.view" | "sessions.manage" | "deployments.publish" | "deployments.promote" | "computers.operate" | "secrets.manage";

type ApiKeyPermissionGrant = {
  scopes: ApiKeyScope[];
};

export type ApiKeySummary = {
  created_by_member_id: string | null;
  id: string;
  name: string;
  key_prefix: string;
  project: string;
  environment: string;
  permissions?: ApiKeyPermissionGrant[];
  status: ApiKeyStatus;
  created_at: string;
  last_used_at: string | null;
  expires_at: string | null;
  revoked_at: string | null;
};

export type ApiKeyIssued = ApiKeySummary & { raw_key: string };

export type ListFilter = "active" | "expired" | "revoked" | "all";

type ListResponse = { api_keys: ApiKeySummary[]; next_cursor?: string };

type IssueInput = {
  name: string;
  expires_in_days: number | null;
  permissions: ApiKeyPermissionGrant[];
};

export async function listApiKeys(
  projectID: string,
  environmentID: string,
  filter: ListFilter,
  cursor?: string,
): Promise<ListResponse> {
  const query = new URLSearchParams({ filter });
  if (cursor) query.set("cursor", cursor);
  return request<ListResponse>(`${apiKeysPath(projectID, environmentID)}?${query.toString()}`);
}

export async function issueApiKey(projectID: string, environmentID: string, input: IssueInput): Promise<ApiKeyIssued> {
  return postJson<IssueInput, ApiKeyIssued>(apiKeysPath(projectID, environmentID), input);
}

export async function revokeApiKey(projectID: string, environmentID: string, id: string): Promise<void> {
  try {
    await del<Record<string, never>>(`${apiKeysPath(projectID, environmentID)}/${encodeURIComponent(id)}`);
  } catch (error) {
    if (error instanceof ApiError && error.code === "not_found") {
      return;
    }
    throw error;
  }
}

function apiKeysPath(projectID: string, environmentID: string): string {
  return `/api/projects/${encodeURIComponent(projectID)}/environments/${encodeURIComponent(environmentID)}/api-keys`;
}

export function rotateApiKey(project: string, environment: string, id: string): Promise<ApiKeyIssued> {
 return postJson(`${apiKeysPath(project,environment)}/${encodeURIComponent(id)}/rotate`, {});
}
export function updateApiKey(project: string, environment: string, id: string, input: {name:string;permissions:ApiKeyPermissionGrant[];expires_at:string|null}): Promise<ApiKeySummary> {
 return request(`${apiKeysPath(project,environment)}/${encodeURIComponent(id)}`, {method:"PUT",headers:{"Content-Type":"application/json"},body:JSON.stringify(input)});
}
