import { expect, test } from "bun:test";
import {
  readEnvironmentSelections,
  readTabScope,
  writeEnvironmentSelections,
  writeScope,
  type ScopeStorage,
} from "./scope-storage";

function memoryStorage(): ScopeStorage & { values: Map<string, string> } {
  const values = new Map<string, string>();
  return {
    values,
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => { values.set(key, value); },
    removeItem: (key) => { values.delete(key); },
  };
}

const throwing: ScopeStorage = {
  getItem: () => { throw new Error("blocked"); },
  setItem: () => { throw new Error("quota"); },
  removeItem: () => { throw new Error("blocked"); },
};

test("a new tab starts in the last chosen scope", () => {
  const tab = memoryStorage(), last = memoryStorage();
  writeScope(last, { projectID: "p", environmentID: "staging" });
  expect(readTabScope(tab, last)).toEqual({ projectID: "p", environmentID: "staging" });
});

test("a tab keeps its own scope after the last chosen scope moves on", () => {
  const tabA = memoryStorage(), tabB = memoryStorage(), last = memoryStorage();
  writeScope(tabA, { projectID: "p", environmentID: "production" });
  writeScope(last, { projectID: "p", environmentID: "production" });
  writeScope(tabB, { projectID: "p", environmentID: "staging" });
  writeScope(last, { projectID: "p", environmentID: "staging" });
  expect(readTabScope(tabA, last)).toEqual({ projectID: "p", environmentID: "production" });
  expect(readTabScope(tabB, last)).toEqual({ projectID: "p", environmentID: "staging" });
});

test("clearing a tab's scope falls back to the seed and an empty scope", () => {
  const tab = memoryStorage(), last = memoryStorage();
  writeScope(tab, { projectID: "p", environmentID: "e" });
  writeScope(tab, { projectID: "", environmentID: "" });
  expect(tab.values.size).toBe(0);
  expect(readTabScope(tab, last)).toEqual({ projectID: "", environmentID: "" });
});

test("malformed stored values are ignored", () => {
  const tab = memoryStorage(), last = memoryStorage();
  tab.values.set("helmr.scope", "{not json");
  last.values.set("helmr.scope", JSON.stringify({ project: 42 }));
  expect(readTabScope(tab, last)).toEqual({ projectID: "", environmentID: "" });
  last.values.set("helmr.environment_by_project", JSON.stringify({ p: "e", q: 1 }));
  expect(readEnvironmentSelections(last)).toEqual({ p: "e" });
  last.values.set("helmr.environment_by_project", "[]");
  expect(readEnvironmentSelections(last)).toEqual({});
});

test("unavailable or throwing storage never breaks scope selection", () => {
  expect(readTabScope(undefined, undefined)).toEqual({ projectID: "", environmentID: "" });
  expect(readTabScope(throwing, throwing)).toEqual({ projectID: "", environmentID: "" });
  expect(() => writeScope(throwing, { projectID: "p", environmentID: "e" })).not.toThrow();
  expect(() => writeScope(throwing, { projectID: "", environmentID: "" })).not.toThrow();
  expect(() => writeEnvironmentSelections(throwing, { p: "e" })).not.toThrow();
  expect(readEnvironmentSelections(throwing)).toEqual({});
});
