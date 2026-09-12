import { afterEach, expect, test } from "bun:test";

import { listActors, listSandboxes, listTasks } from "./definitions";

const originalFetch = globalThis.fetch;
afterEach(() => {
  globalThis.fetch = originalFetch;
});

function captureFetch(body: unknown) {
  const urls: string[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    urls.push(String(input));
    return Response.json(body);
  }) as typeof fetch;
  return urls;
}

const scope = { projectID: "project-1", environmentID: "env-1" };

test("lists current-deployment tasks without params", async () => {
  const urls = captureFetch({ deployment_id: "d-1", tasks: [] });
  await listTasks(scope);
  expect(urls[0]).toBe("/api/projects/project-1/environments/env-1/tasks");
});

test("lists definitions for a specific deployment", async () => {
  const urls = captureFetch({ deployment_id: "d/1", tasks: [], actors: [], sandboxes: [] });
  await listTasks({ ...scope, deploymentID: "d/1", limit: 100 });
  await listActors({ ...scope, deploymentID: "d/1", cursor: "c1" });
  await listSandboxes({ ...scope, deploymentID: "d/1" });
  expect(urls).toEqual([
    "/api/projects/project-1/environments/env-1/tasks?deployment_id=d%2F1&limit=100",
    "/api/projects/project-1/environments/env-1/actors?deployment_id=d%2F1&cursor=c1",
    "/api/projects/project-1/environments/env-1/sandboxes?deployment_id=d%2F1",
  ]);
});
