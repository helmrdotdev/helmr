const ON_LOGIN_PATH = () => window.location.pathname === "/login";

type RequestOptions = RequestInit & {
  redirectOnUnauthorized?: boolean;
};

async function handleResponse<T>(
  response: Response,
  { redirectOnUnauthorized }: { redirectOnUnauthorized: boolean },
): Promise<T> {
  if (response.status === 401 && redirectOnUnauthorized && !ON_LOGIN_PATH()) {
    const next = encodeURIComponent(window.location.pathname + window.location.search);
    window.location.href = `/login?next=${next}`;
    throw new ApiError("unauthorized", "Authentication is required.", response.status);
  }
  if (!response.ok) {
    const body = await response.json().catch((): Record<string, unknown> => ({
      error: response.statusText,
    }));
    throw new ApiError(
      errorKind(response.status, body),
      errorMessage(body, response.statusText),
      response.status,
    );
  }
  if (response.status === 204) {
    return undefined as T;
  }
  return response.json() as Promise<T>;
}

function errorKind(status: number, body: Record<string, unknown>): string {
  const explicit = stringField(body, "error_kind") ?? stringField(body, "kind");
  if (explicit) return explicit;
  if (status === 400) return "bad_request";
  if (status === 401) return "unauthorized";
  if (status === 403) return "forbidden";
  if (status === 404) return "not_found";
  if (status === 409) return "conflict";
  if (status === 422) return "unprocessable_entity";
  if (status >= 500) return "internal";
  return "unknown";
}

function errorMessage(body: Record<string, unknown>, fallback: string): string {
  return stringField(body, "message") ?? stringField(body, "error") ?? fallback;
}

function stringField(body: Record<string, unknown>, field: string): string | undefined {
  const value = body[field];
  return typeof value === "string" && value.trim() !== "" ? value : undefined;
}

export class ApiError extends Error {
  errorKind: string;
  status: number;

  constructor(errorKind: string, message: string, status: number) {
    super(message);
    this.errorKind = errorKind;
    this.status = status;
  }
}

export async function request<T>(path: string, init: RequestOptions = {}): Promise<T> {
  const { redirectOnUnauthorized = true, ...fetchInit } = init;
  const headers = new Headers(fetchInit.headers);
  if (!headers.has("content-type")) {
    headers.set("content-type", "application/json");
  }
  const response = await fetch(path, {
    ...fetchInit,
    credentials: "include",
    headers,
  });
  return handleResponse<T>(response, { redirectOnUnauthorized });
}

export async function postJson<TReq, TRes>(
  path: string,
  body: TReq,
  init: RequestOptions = {},
): Promise<TRes> {
  return request<TRes>(path, { ...init, method: "POST", body: JSON.stringify(body ?? {}) });
}

export async function del<T>(path: string, init: RequestOptions = {}): Promise<T> {
  return request<T>(path, { ...init, method: "DELETE" });
}
