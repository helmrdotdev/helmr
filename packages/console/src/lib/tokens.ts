import { postJson, request } from "./api";

export type TokenStatus = "pending" | "completed" | "expired" | "cancelled";

export type TokenListItem = {
  id: string;
  status: TokenStatus;
  tags: string[];
  timeout_at: string;
  completed_at?: string;
  created_at: string;
  updated_at: string;
};

export type ListTokensResponse = {
  tokens: TokenListItem[];
  next_cursor?: string;
};

export type TokenScope = {
  projectID: string;
  environmentID: string;
};

export async function listTokens(
  scope: TokenScope,
  options: { status?: TokenStatus | undefined; cursor?: string | undefined; limit?: number | undefined } = {},
): Promise<ListTokensResponse> {
  const params = new URLSearchParams();
  if (options.status) params.set("status", options.status);
  if (options.cursor) params.set("cursor", options.cursor);
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  const query = params.size === 0 ? "" : `?${params.toString()}`;
  return request<ListTokensResponse>(`${tokensPath(scope)}${query}`);
}

export async function completeToken(
  id: string,
  scope: TokenScope,
  input: { result: unknown; idempotency_key: string },
): Promise<TokenListItem> {
  return postJson<{ result: unknown; idempotency_key: string }, TokenListItem>(
    `${tokensPath(scope)}/${encodeURIComponent(id)}/complete`,
    input,
  );
}

export async function cancelToken(
  id: string,
  scope: TokenScope,
  input: { idempotency_key: string },
): Promise<TokenListItem> {
  return postJson<{ idempotency_key: string }, TokenListItem>(
    `${tokensPath(scope)}/${encodeURIComponent(id)}/cancel`,
    input,
  );
}

function tokensPath(scope: TokenScope): string {
  if (!scope.projectID || !scope.environmentID) {
    throw new Error("Token project and environment are required");
  }
  return `/api/projects/${encodeURIComponent(scope.projectID)}/environments/${encodeURIComponent(scope.environmentID)}/tokens`;
}
