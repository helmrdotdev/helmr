import "./browser-test-env";
import { beforeEach, afterEach, expect, test } from "bun:test";

import { ApiError, request } from "./api";

const originalFetch = globalThis.fetch;

beforeEach(() => {
 const values = new Map<string,string>();
 Object.defineProperty(globalThis,"sessionStorage",{configurable:true,value:{getItem:(key:string)=>values.get(key)??null,setItem:(key:string,value:string)=>values.set(key,value),removeItem:(key:string)=>values.delete(key)}});
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  delete (globalThis as { window?: unknown }).window;
});

test("redirects unauthorized requests and rejects instead of hanging", async () => {
  const windowMock = {
    location: {
      pathname: "/runs",
      search: "",
      href: "",
    },
  };
  (globalThis as { window?: typeof windowMock }).window = windowMock;
  globalThis.fetch = (async () =>
    Response.json({
      error: { code: "unauthorized", message: "session authentication is required" },
    }, { status: 401 })) as typeof fetch;

  let error: unknown;
  try {
    await request("/api/runs");
  } catch (e) {
    error = e;
  }

  expect(error).toBeInstanceOf(ApiError);
  expect((error as ApiError).code).toBe("unauthorized");
  expect(windowMock.location.href).toBe("/login?next=%2Fruns");
});

test("decodes the common api error envelope", async () => {
  (globalThis as { window?: unknown }).window = {
    location: {
      pathname: "/settings/project",
      search: "",
      href: "",
    },
  };
  globalThis.fetch = (async () =>
    Response.json({
      error: {
        code: "forbidden",
        message: "access denied",
        details: { permission: "projects.read" },
      },
    }, { status: 403 })) as typeof fetch;

  await expect(request("/api/projects")).rejects.toMatchObject({
    code: "forbidden",
    message: "access denied",
    status: 403,
    details: { permission: "projects.read" },
  });
});

for (const method of ["GET", "POST"]) {
  test(`rotation rechecks the same account and ${method === "GET" ? "retries one read" : "does not replay a mutation"}`, async () => {
    const location = { pathname: "/settings/roles", search: "", href: "" };
    (globalThis as { window?: unknown }).window = { location };
    sessionStorage.setItem("helmr.account", "user-1");
    sessionStorage.setItem("helmr.organization", "org-1");
    let operations = 0;
    globalThis.fetch = (async (input: string | URL | Request) => {
      if (input === "/api/me") return Response.json({ user_id: "user-1" });
      operations++;
      return operations === 1 ? Response.json({ error: { code: "unauthorized", message: "old cookie" } }, { status: 401 }) : Response.json({ value: "fresh" });
    }) as typeof fetch;
    if (method === "GET") expect(await request("/api/roles", { method })).toEqual({ value: "fresh" });
    else await expect(request("/api/roles", { method })).rejects.toMatchObject({ code: "authentication_changed" });
    expect(operations).toBe(method === "GET" ? 2 : 1);
    expect(location.href).toBe("");
  });
}

test("a different account never retries the old request", async () => {
  (globalThis as { window?: unknown }).window = { location: { pathname: "/roles", href: "" } };
  sessionStorage.setItem("helmr.account", "user-1");
  let operations = 0;
  globalThis.fetch = (async (input: string | URL | Request) => {
    if (input === "/api/me") return Response.json({ user_id: "user-2" });
    operations++;
    return Response.json({ error: { code: "authentication_required", message: "sign in" } }, { status: 403 });
  }) as typeof fetch;
  await expect(request("/api/roles")).rejects.toMatchObject({ code: "account_changed" });
  expect(operations).toBe(1);
});

for (const path of ["/admin/api/v1/regions", "/admin/api/v1/credentials"]) {
  test(`expired operator authority uses its own login for ${path}`, async () => {
    const location = { pathname: "/admin/security", search: "", href: "" };
    (globalThis as { window?: unknown }).window = { location };
    sessionStorage.setItem("helmr.account", "ordinary-user");
    const calls: string[] = [];
    globalThis.fetch = (async (input: string | URL | Request) => {
      calls.push(String(input));
      if (input === "/api/me") return Response.json({ user_id: "ordinary-user" });
      return Response.json({ error: { code: "authentication_required", message: "Operator sign-in required" } }, { status: 403 });
    }) as typeof fetch;
    await expect(request(path, { method: "POST" })).rejects.toMatchObject({ code: "authentication_required" });
    expect(calls).toEqual([path, "/admin/api/v1/me"]);
    expect(location.href).toBe("/admin/login?next=%2Fadmin%2Fsecurity");
  });
}

test("same operator rotation retries a read independently of the ordinary account", async () => {
  (globalThis as { window?: unknown }).window = { location: { pathname: "/admin/security", search: "", href: "" } };
  sessionStorage.setItem("helmr.operator", "operator-1");
  sessionStorage.setItem("helmr.account", "ordinary-user");
  let reads = 0;
  globalThis.fetch = (async (input: string | URL | Request) => {
    if (input === "/admin/api/v1/me") return Response.json({ id: "operator-1" });
    reads++;
    return reads === 1 ? Response.json({ error: { code: "authentication_required", message: "old cookie" } }, { status: 403 }) : Response.json({ credentials: [] });
  }) as typeof fetch;
  expect(await request("/admin/api/v1/credentials")).toEqual({ credentials: [] });
  expect(reads).toBe(2);
});


test("operator rotation never replays a mutation", async () => {
  (globalThis as { window?: unknown }).window = { location: { pathname: "/admin/security", search: "", href: "" } };
  sessionStorage.setItem("helmr.operator", "operator-1");
  let writes = 0;
  globalThis.fetch = (async (input: string | URL | Request) => {
    if (input === "/admin/api/v1/me") return Response.json({ id: "operator-1" });
    writes++;
    return Response.json({ error: { code: "authentication_required", message: "Sign in" } }, { status: 403 });
  }) as typeof fetch;
  await expect(request("/admin/api/v1/credentials/enroll", { method: "POST" })).rejects.toMatchObject({ code: "authentication_changed" });
  expect(writes).toBe(1);
});
