import { afterEach, expect, test } from "bun:test";

import {
  actorConsolePath,
  actorRunConsolePath,
  getActor,
  getActorOutput,
} from "./actors";

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("loads Actor status from the scoped read API", async () => {
  let requestedURL: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedURL = String(input);
    return Response.json({
      id: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
      status: "open",
      created_at: "2026-07-25T00:00:00Z",
      updated_at: "2026-07-25T00:00:00Z",
    });
  }) as typeof fetch;

  await getActor({
    declaredID: "operator/v1",
    actorID: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
    projectID: "project/1",
    environmentID: "env/1",
  });

  expect(requestedURL).toBe(
    "/api/projects/project%2F1/environments/env%2F1/actors/operator%2Fv1/status?actor_id=act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
  );
});

test("loads the next Actor output page with a bounded cursor", async () => {
  let requestedURL: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedURL = String(input);
    return Response.json({ records: [], next_after: 42, has_more: false });
  }) as typeof fetch;

  await getActorOutput({
    declaredID: "operator",
    actorID: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
    projectID: "project-1",
    environmentID: "env-1",
  }, { after: 42, limit: 100 });

  expect(requestedURL).toBe(
    "/api/projects/project-1/environments/env-1/actors/operator/output?actor_id=act_aaaaaaaaaaaaaaaaaaaaaaaaaa&after=42&limit=100",
  );
});

test("escapes both parts of an Actor console route", () => {
  expect(actorConsolePath("operator/v1", "act/id", "project/1", "env/1")).toBe(
    "/actors/operator%2Fv1/act%2Fid?project_id=project%2F1&environment_id=env%2F1",
  );
});

test("links only Actor Runs and preserves their explicit scope", () => {
  expect(actorRunConsolePath({
    entrypoint: { kind: "actor", id: "operator" },
    actor_id: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
  }, "prj_aaaaaaaaaaaaaaaaaaaaaaaaaa", "env_aaaaaaaaaaaaaaaaaaaaaaaaaa")).toBe(
    "/actors/operator/act_aaaaaaaaaaaaaaaaaaaaaaaaaa?project_id=prj_aaaaaaaaaaaaaaaaaaaaaaaaaa&environment_id=env_aaaaaaaaaaaaaaaaaaaaaaaaaa",
  );
  expect(actorRunConsolePath({
    entrypoint: { kind: "task", id: "resize-image" },
  }, "prj_aaaaaaaaaaaaaaaaaaaaaaaaaa", "env_aaaaaaaaaaaaaaaaaaaaaaaaaa")).toBeUndefined();
});
