const ON_LOGIN_PATH = () => window.location.pathname === "/login";

type RequestOptions = RequestInit & {
  redirectOnUnauthorized?: boolean;
};

async function handleResponse<T>(
  response: Response,
  { redirectOnUnauthorized }: { redirectOnUnauthorized: boolean },
): Promise<T> {
  if (response.status === 401 && redirectOnUnauthorized && !ON_LOGIN_PATH()) {
    window.location.href = "/login";
    throw new ApiError("unauthorized", "Authentication is required.", response.status);
  }
  if (!response.ok) {
    const body = await response.json().catch((): Record<string, unknown> => ({}));
    const error = objectField(body, "error");
    throw new ApiError(
      stringField(error, "code") ?? statusCode(response.status),
      stringField(error, "message") ?? response.statusText,
      response.status,
      objectField(error, "details"),
    );
  }
  if (response.status === 204 || response.status === 205 || response.headers.get("content-length") === "0") {
    return undefined as T;
  }
  const body = await response.text();
  if (body.trim() === "") {
    return undefined as T;
  }
  return JSON.parse(body) as T;
}

function statusCode(status: number): string {
  if (status === 400) return "bad_request";
  if (status === 401) return "unauthorized";
  if (status === 403) return "forbidden";
  if (status === 404) return "not_found";
  if (status === 405) return "method_not_allowed";
  if (status === 409) return "conflict";
  if (status === 410) return "gone";
  if (status === 413) return "request_too_large";
  if (status === 422) return "unprocessable_entity";
  if (status === 429) return "rate_limited";
  if (status === 501) return "not_implemented";
  if (status === 502) return "bad_gateway";
  if (status === 503) return "service_unavailable";
  if (status >= 500) return "internal_error";
  return "http_error";
}

function stringField(body: Record<string, unknown>, field: string): string | undefined {
  const value = body[field];
  return typeof value === "string" && value.trim() !== "" ? value : undefined;
}

function objectField(body: Record<string, unknown>, field: string): Record<string, unknown> {
  const value = body[field];
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown>
    : {};
}

export class ApiError extends Error {
  code: string;
  status: number;
  details?: Readonly<Record<string, unknown>>;

  constructor(
    code: string,
    message: string,
    status: number,
    details: Readonly<Record<string, unknown>> = {},
  ) {
    super(message);
    this.code = code;
    this.status = status;
    if (Object.keys(details).length > 0) this.details = details;
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
