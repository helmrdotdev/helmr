import { ApiError, del, postJson, request } from "./api";

export type ApiKeyStatus = "active" | "expired" | "revoked";

export type ApiKeyScope =
  | "runs:create"
  | "runs:read"
  | "secrets:write"
  | "waitpoints:respond"
  | "waitpoint-policies:manage"
  | "tasks:deploy";

export type ApiKeyPermissionGrant = {
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
  project_id: string;
  environment_id: string;
  expires_in_days: number | null;
  permissions: ApiKeyPermissionGrant[];
};

export async function listApiKeys(filter: ListFilter): Promise<ListResponse> {
  return request<ListResponse>(`/api/api-keys?filter=${encodeURIComponent(filter)}`);
}

export async function issueApiKey(input: IssueInput): Promise<ApiKeyIssued> {
  return postJson<IssueInput, ApiKeyIssued>("/api/api-keys", input);
}

export async function revokeApiKey(id: string): Promise<void> {
  try {
    await del<Record<string, never>>(`/api/api-keys/${encodeURIComponent(id)}`);
  } catch (error) {
    if (error instanceof ApiError && error.errorKind === "not_found") {
      return;
    }
    throw error;
  }
}
