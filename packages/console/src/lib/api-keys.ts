import { ApiError, del, postJson, request } from "./api";

export type ApiKeyStatus = "active" | "expired" | "revoked";

export type ApiKeyScope =
  | "runs:create"
  | "runs:read"
  | "runs:manage"
  | "session-streams:read"
  | "session-input:send"
  | "session-output:append"
  | "tokens:create"
  | "tokens:read"
  | "tokens:complete"
  | "tokens:cancel"
  | "workspace-lifecycle:manage"
  | "workspace-files:read"
  | "workspace-files:write"
  | "workspace-versions:read"
  | "workspace-versions:capture"
  | "workspace-versions:restore"
  | "workspace-versions:diff"
  | "workspace-exec:create"
  | "workspace-exec:read"
  | "workspace-exec:manage"
  | "workspace-pty:create"
  | "workspace-pty:read"
  | "workspace-pty:manage"
  | "workspace-ports:expose"
  | "workspace-ports:read"
  | "workspace-ports:close"
  | "secrets:write"
  | "tasks:deploy";

type ApiKeyPermissionGrant = {
  scopes: ApiKeyScope[];
};

export type ApiKeySummary = {
  id: string;
  name: string;
  key_prefix: string;
  project_id: string;
  environment_id: string;
  permissions?: ApiKeyPermissionGrant[];
  status: ApiKeyStatus;
  created_at: string;
  last_used_at: string | null;
  expires_at: string | null;
  revoked_at: string | null;
};

export type ApiKeyIssued = ApiKeySummary & { raw_key: string };

export type ListFilter = "active" | "expired" | "revoked" | "all";

export type ListResponse = { items: ApiKeySummary[]; has_more: boolean };

export type IssueInput = {
  name: string;
  expires_in_days: number | null;
  permissions: ApiKeyPermissionGrant[];
};

export async function listApiKeys(projectID: string, environmentID: string, filter: ListFilter): Promise<ListResponse> {
  return request<ListResponse>(`${apiKeysPath(projectID, environmentID)}?filter=${encodeURIComponent(filter)}`);
}

export async function issueApiKey(projectID: string, environmentID: string, input: IssueInput): Promise<ApiKeyIssued> {
  return postJson<IssueInput, ApiKeyIssued>(apiKeysPath(projectID, environmentID), input);
}

export async function revokeApiKey(projectID: string, environmentID: string, id: string): Promise<void> {
  try {
    await del<Record<string, never>>(`${apiKeysPath(projectID, environmentID)}/${encodeURIComponent(id)}`);
  } catch (error) {
    if (error instanceof ApiError && error.errorKind === "not_found") {
      return;
    }
    throw error;
  }
}

function apiKeysPath(projectID: string, environmentID: string): string {
  return `/api/projects/${encodeURIComponent(projectID)}/environments/${encodeURIComponent(environmentID)}/api-keys`;
}
