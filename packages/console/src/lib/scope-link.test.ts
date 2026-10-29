import "./browser-test-env";
import { expect, test } from "bun:test";
import { linkNotice, linkRestoreScope, readScopeHint, scopeShareURL, withoutScopeHint } from "./scope-link";

test("a share hint needs both Project and Environment", () => {
  expect(readScopeHint("?project=p%2F1&environment=prod&status=open")).toEqual({ projectID: "p/1", environmentID: "prod" });
  expect(readScopeHint("?project=p1")).toBeUndefined();
  expect(readScopeHint("?environment=prod")).toBeUndefined();
  expect(readScopeHint("?project=&environment=prod")).toBeUndefined();
  expect(readScopeHint("")).toBeUndefined();
});

test("stripping a hint keeps the path, other params and hash", () => {
  expect(withoutScopeHint({ pathname: "/sessions", search: "?project=p&status=open&environment=e", hash: "#turns" }))
    .toBe("/sessions?status=open#turns");
  expect(withoutScopeHint({ pathname: "/agents/support", search: "?project=p&environment=e", hash: "" })).toBe("/agents/support");
  expect(withoutScopeHint({ pathname: "/computers", search: "?project=p", hash: "" })).toBe("/computers");
});

test("a location without hint params needs no rewrite", () => {
  expect(withoutScopeHint({ pathname: "/sessions", search: "?status=open", hash: "#x" })).toBeUndefined();
  expect(withoutScopeHint({ pathname: "/", search: "", hash: "" })).toBeUndefined();
});

test("share URLs are absolute and carry the tab scope", () => {
  expect(scopeShareURL("https://console.example/sessions/s1?status=open#turns", { projectID: "p/1", environmentID: "prod" }))
    .toBe("https://console.example/sessions/s1?status=open&project=p%2F1&environment=prod#turns");
  expect(scopeShareURL("https://console.example/agents/x?project=old&environment=old", { projectID: "p", environmentID: "e" }))
    .toBe("https://console.example/agents/x?project=p&environment=e");
});

test("a share URL round-trips into the same hint and a clean address", () => {
  const url = new URL(scopeShareURL("https://console.example/deployments/d1", { projectID: "p", environmentID: "e" }));
  expect(readScopeHint(url.search)).toEqual({ projectID: "p", environmentID: "e" });
  expect(withoutScopeHint(url)).toBe("/deployments/d1");
});

const production = { projectID: "p", environmentID: "production" };
const staging = { projectID: "p", environmentID: "staging" };
const empty = { projectID: "", environmentID: "" };

test("a link that switches the tab is announced once its scope settles", () => {
  const link = { hint: production, previous: staging };
  expect(linkNotice(link, undefined)).toBeUndefined();
  expect(linkNotice(link, production)).toBe("switched");
});

test("a link to the scope the tab already had, or a first scope, is not announced", () => {
  expect(linkNotice({ hint: production, previous: production }, production)).toBeUndefined();
  expect(linkNotice({ hint: production, previous: empty }, production)).toBeUndefined();
  expect(linkNotice(undefined, production)).toBeUndefined();
});

test("a link that does not resolve to its exact scope is never silent", () => {
  // Unknown Environment, unknown Project, a Project without Environments, and a partial hint.
  expect(linkNotice({ hint: { projectID: "p", environmentID: "missing" }, previous: empty }, production)).toBe("unavailable");
  expect(linkNotice({ hint: { projectID: "missing", environmentID: "production" }, previous: staging }, staging)).toBe("unavailable");
  expect(linkNotice({ hint: production, previous: staging }, { projectID: "p", environmentID: "" })).toBe("unavailable");
  expect(linkNotice({ hint: undefined, previous: staging }, staging)).toBe("unavailable");
});

const linkedEnvironments = ["production", "staging"];
const state = (overrides: Partial<Parameters<typeof linkRestoreScope>[1]> = {}) =>
  ({ projectID: "q", notFound: false, environments: undefined, rejectedProjectIDs: new Set<string>(), ...overrides });

test("an unavailable linked Project returns the tab to its previous scope", () => {
  const link = { hint: { projectID: "q", environmentID: "production" }, previous: staging };
  expect(linkRestoreScope(link, state())).toBeUndefined();
  expect(linkRestoreScope(link, state({ notFound: true }))).toEqual(staging);
});

test("an unknown linked Environment returns the tab to its previous scope once the Project has loaded", () => {
  const link = { hint: { projectID: "q", environmentID: "gone" }, previous: staging };
  expect(linkRestoreScope(link, state())).toBeUndefined();
  expect(linkRestoreScope(link, state({ environments: linkedEnvironments }))).toEqual(staging);
  expect(linkRestoreScope(link, state({ environments: [] }))).toEqual(staging);
  // A previous scope in the same Project is restored too.
  expect(linkRestoreScope({ hint: { projectID: "p", environmentID: "gone" }, previous: staging }, state({ projectID: "p", environments: linkedEnvironments }))).toEqual(staging);
});

test("an available link scope is kept", () => {
  const link = { hint: { projectID: "q", environmentID: "production" }, previous: staging };
  expect(linkRestoreScope(link, state({ environments: linkedEnvironments }))).toBeUndefined();
});

test("without a usable previous scope the link's own fallback applies", () => {
  const gone = { projectID: "q", environmentID: "gone" };
  expect(linkRestoreScope(undefined, state({ notFound: true }))).toBeUndefined();
  expect(linkRestoreScope({ hint: gone, previous: empty }, state({ notFound: true }))).toBeUndefined();
  expect(linkRestoreScope({ hint: gone, previous: empty }, state({ environments: linkedEnvironments }))).toBeUndefined();
  // A partial link adopted nothing, so there is nothing to undo.
  expect(linkRestoreScope({ hint: undefined, previous: staging }, state({ notFound: true }))).toBeUndefined();
  // The previous Project is itself gone.
  expect(linkRestoreScope({ hint: gone, previous: staging }, state({ notFound: true, rejectedProjectIDs: new Set(["p"]) }))).toBeUndefined();
  expect(linkRestoreScope({ hint: { projectID: "p", environmentID: "x" }, previous: staging }, state({ projectID: "p", notFound: true }))).toBeUndefined();
  // Only the linked Project is judged.
  expect(linkRestoreScope({ hint: gone, previous: staging }, state({ projectID: "other", notFound: true }))).toBeUndefined();
});
