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
    return new Promise<T>(() => {});
  }
  if (!response.ok) {
    const body = await response.json().catch(() => ({
      error: response.statusText,
    }));
    throw new ApiError(
      body.error_kind ?? body.kind ?? "unknown",
      body.message ?? body.error ?? response.statusText,
      response.status,
    );
  }
  if (response.status === 204) {
    return undefined as T;
  }
  return response.json() as Promise<T>;
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
  const response = await fetch(path, {
    ...fetchInit,
    credentials: "include",
    headers: {
      "content-type": "application/json",
      ...fetchInit.headers,
    },
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
