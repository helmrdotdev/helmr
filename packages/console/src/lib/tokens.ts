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

// The GET response never carries callback_url or public_access_token; only the
// create path returns them and the console does not create Tokens.
export type Token = TokenListItem & {
  metadata: unknown;
  result?: unknown;
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

export async function getToken(id: string, scope: TokenScope): Promise<Token> {
  return request<Token>(`${tokensPath(scope)}/${encodeURIComponent(id)}`);
}

export function isTerminalTokenStatus(status: TokenStatus): boolean {
  return status !== "pending";
}

export async function completeToken(
  id: string,
  scope: TokenScope,
  input: { result: unknown; idempotency_key: string },
): Promise<Token> {
  return postJson<{ result: unknown; idempotency_key: string }, Token>(
    `${tokensPath(scope)}/${encodeURIComponent(id)}/complete`,
    input,
  );
}

export async function cancelToken(
  id: string,
  scope: TokenScope,
  input: { idempotency_key: string },
): Promise<Token> {
  return postJson<{ idempotency_key: string }, Token>(
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
