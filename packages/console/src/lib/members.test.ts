import "./browser-test-env";
import { afterEach, expect, test } from "bun:test";

import { createInvitations, parseInviteEmails, validInviteAddress } from "./members";

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("splits pasted addresses on commas, semicolons, spaces and line breaks", () => {
  expect(parseInviteEmails(" Ada@Example.test, grace@example.test;\n\nlin@example.test\tbob@example.test ")).toEqual([
    { email: "ada@example.test", state: "valid" },
    { email: "grace@example.test", state: "valid" },
    { email: "lin@example.test", state: "valid" },
    { email: "bob@example.test", state: "valid" },
  ]);
  expect(parseInviteEmails("")).toEqual([]);
  expect(parseInviteEmails(" ,\n ")).toEqual([]);
});

test("marks invalid addresses and repeats of an earlier address", () => {
  expect(parseInviteEmails("ada@example.test not-an-address ADA@example.test not-an-address x@y a..b@example.test")).toEqual([
    { email: "ada@example.test", state: "valid" },
    { email: "not-an-address", state: "invalid" },
    { email: "ada@example.test", state: "duplicate" },
    { email: "not-an-address", state: "duplicate" },
    { email: "x@y", state: "valid" },
    { email: "a..b@example.test", state: "invalid" },
  ]);
});

test("takes the address of a mail-client entry and drops its display name", () => {
  expect(parseInviteEmails('Ada Lovelace <Ada@Example.test>, "Hopper, Grace" <grace@example.test>; lin@example.test Bob <bob@example.test>\nCarol')).toEqual([
    { email: "ada@example.test", state: "valid" },
    { email: "grace@example.test", state: "valid" },
    { email: "lin@example.test", state: "valid" },
    { email: "bob@example.test", state: "valid" },
    { email: "carol", state: "invalid" },
  ]);
  expect(parseInviteEmails("<ada@example.test> <not an address>")).toEqual([
    { email: "ada@example.test", state: "valid" },
    { email: "<not", state: "invalid" },
    { email: "an", state: "invalid" },
    { email: "address>", state: "invalid" },
  ]);
});

test("accepts the bare addresses the server accepts", () => {
  for (const address of ["o'brien+team@example.test", "first.last@sub.example.test", "zoë@example.test", "x@y"]) {
    expect(validInviteAddress(address)).toBe(true);
  }
  for (const address of [".ada@example.test", "ada.@example.test", "ada@example..test", "ada@", "@example.test", "a b@example.test", "ada@exa(mple).test", `${"a".repeat(310)}@example.test`]) {
    expect(validInviteAddress(address)).toBe(false);
  }
});

test("sends every address in one invitation request and returns its results", async () => {
  let body: unknown;
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    body = JSON.parse(String(init?.body));
    return Response.json({ results: [{ email: "ada@example.test", status: "invited" }] });
  }) as typeof fetch;

  const results = await createInvitations({ emails: ["ada@example.test", "grace@example.test"] });

  expect(body).toEqual({ emails: ["ada@example.test", "grace@example.test"] });
  expect(results).toEqual([{ email: "ada@example.test", status: "invited" }]);
});
