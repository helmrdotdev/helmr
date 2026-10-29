import { locationDestination, returnPath, withNext } from "./continuation";


type RequestOptions = RequestInit & {
  redirectOnUnauthorized?: boolean;
};

async function handleResponse<T>(
  response: Response,
): Promise<T> {
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
  const operator = path.startsWith("/admin/api/v1/");
  const identityPath = operator ? "/admin/api/v1/me" : "/api/me";
  const identityKey = operator ? "helmr.operator" : "helmr.account";
  const loginPath = operator ? "/admin/login" : "/login";
  const organization = operator ? "" : selectedOrganization();
  if (organization) headers.set("X-Helmr-Organization", organization);
  const account = sessionStorage.getItem(identityKey) ?? "";
  const send = () => fetch(path, { ...fetchInit, credentials: "include", headers });
  let response = await send();
  let result: T;
  try { result = await handleResponse<T>(response); }
  catch (error) {
    if (error instanceof ApiError && error.code === "recent_authentication_required") {
      window.dispatchEvent(new CustomEvent("helmr-confirmation-required", { detail: { operator } }));
    }
    const needsLogin = error instanceof ApiError && (error.code === "authentication_required" || (error.status === 401 && error.code === "unauthorized"));
    if (!needsLogin) throw error;
    // One current-cookie read distinguishes rotation from actual sign-out. It
    // bypasses this helper, and cannot recursively retry or replay a mutation.
    let currentID: string | undefined;
    try {
      const check = await fetch(identityPath, { credentials: "include", cache: "no-store" });
      if (check.ok) { const current = await check.json() as { id?: string; user_id?: string }; currentID = operator ? current.id : current.user_id; }
    } catch { /* Preserve the original failure when the network is unavailable. */ }
    if (currentID) {
      if (!account || currentID !== account || (!operator && selectedOrganization() !== organization)) {
        throw new ApiError("account_changed", "Your account or organization changed. Refresh before retrying.", 409);
      }
      if ((fetchInit.method ?? "GET").toUpperCase() !== "GET") {
        throw new ApiError("authentication_changed", "Your sign-in was refreshed. Retry this action.", 409);
      }
      response = await send();
      result = await handleResponse<T>(response);
    } else {
      if (redirectOnUnauthorized && window.location.pathname !== loginPath) window.location.href = withNext(loginPath, locationDestination(window.location));
      throw error;
    }
  }
  if (path === identityPath) {
    const identity = result as { id?: string; user_id?: string };
    const user = operator ? identity.id : identity.user_id;
    if (user) {
      if (!operator && account && account !== user) setOrganization("");
      sessionStorage.setItem(identityKey, user);
    }
  }
  if(response.ok&&fetchInit.method==="POST"&&(/^\/api\/auth\/(github\/finish|passkey\/verify|email\/[^/]+\/verify|logout)$/.test(path)||path==="/api/account/access/revoke-all"))localStorage.setItem("helmr.account-change",crypto.randomUUID());
  return result;
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

// Organization selection belongs to this tab. Changing it reloads the application
// so requests, streams and query caches from the previous context are discarded.
export function selectedOrganization():string {
 return sessionStorage.getItem("helmr.organization")??"";
}
export function setOrganization(id:string):void {
 if(id)sessionStorage.setItem("helmr.organization",id);else sessionStorage.removeItem("helmr.organization");
}
export function chooseOrganization(id:string,next="/"):void {setOrganization(id);window.location.assign(returnPath(next));}
