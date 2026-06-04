import { afterEach, expect, test } from "bun:test";

import { ApiError, request } from "./api";

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
  delete (globalThis as { window?: unknown }).window;
});

test("redirects unauthorized requests and rejects instead of hanging", async () => {
  const windowMock = {
    location: {
      pathname: "/runs",
      search: "?status=waiting",
      href: "",
    },
  };
  (globalThis as { window?: typeof windowMock }).window = windowMock;
  globalThis.fetch = (async () =>
    Response.json({ error: "session authentication is required" }, { status: 401 })) as typeof fetch;

  let error: unknown;
  try {
    await request("/api/runs");
  } catch (e) {
    error = e;
  }

  expect(error).toBeInstanceOf(ApiError);
  expect((error as ApiError).errorKind).toBe("unauthorized");
  expect(windowMock.location.href).toBe("/login?next=%2Fruns%3Fstatus%3Dwaiting");
});

test("maps status-only api errors to stable error kinds", async () => {
  (globalThis as { window?: unknown }).window = {
    location: {
      pathname: "/settings/projects",
      search: "",
      href: "",
    },
  };
  globalThis.fetch = (async () =>
    Response.json({ error: "access denied" }, { status: 403 })) as typeof fetch;

  await expect(request("/api/projects")).rejects.toMatchObject({
    errorKind: "forbidden",
    message: "access denied",
    status: 403,
  });
});
