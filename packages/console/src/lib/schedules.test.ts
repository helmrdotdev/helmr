import { afterEach, expect, test } from "bun:test";

import { activateSchedule, createSchedule, deleteSchedule, listSchedules } from "./schedules";

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("lists schedules with project and environment scope", async () => {
  let requestedUrl: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input);
    return Response.json({ schedules: [] });
  }) as typeof fetch;

  await listSchedules({ projectID: "project-1", environmentID: "env-1" });

  expect(requestedUrl).toBe("/api/projects/project-1/environments/env-1/schedules");
});

test("creates schedules with current API fields", async () => {
  let requestedUrl: string | undefined;
  let requestedBody: unknown;
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input);
    requestedBody = JSON.parse(String(init?.body));
    return Response.json({
      id: "schedule-1",
      project_id: "project-1",
      environment_id: "env-1",
      task: "task",
      deduplication_key: "task-hourly",
      cron: "0 * * * *",
      timezone: "UTC",
      active: true,
      status: "active",
      created_at: "2026-06-01T00:00:00Z",
      updated_at: "2026-06-01T00:00:00Z",
    });
  }) as typeof fetch;

  await createSchedule({
    project_id: "project-1",
    environment_id: "env-1",
    deduplication_key: "task-hourly",
    task: "task",
    cron: "0 * * * *",
    timezone: "UTC",
    active: true,
  });

  expect(requestedUrl).toBe("/api/projects/project-1/environments/env-1/schedules");
  expect(requestedBody).toEqual({
    deduplication_key: "task-hourly",
    task: "task",
    cron: "0 * * * *",
    timezone: "UTC",
    active: true,
  });
});

test("creates schedules without workspace fields", async () => {
  let requestedBody: unknown;
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    requestedBody = JSON.parse(String(init?.body));
    return Response.json({
      id: "schedule-1",
      project_id: "project-1",
      environment_id: "env-1",
      task: "task",
      deduplication_key: "task-hourly",
      cron: "0 * * * *",
      timezone: "UTC",
      active: true,
      status: "active",
      created_at: "2026-06-01T00:00:00Z",
      updated_at: "2026-06-01T00:00:00Z",
    });
  }) as typeof fetch;

  await createSchedule({
    project_id: "project-1",
    environment_id: "env-1",
    deduplication_key: "task-hourly",
    task: "task",
    cron: "0 * * * *",
  });

  expect(requestedBody).toEqual({
    deduplication_key: "task-hourly",
    task: "task",
    cron: "0 * * * *",
  });
});

test("scopes schedule actions and escapes ids", async () => {
  const requestedUrls: string[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrls.push(String(input));
    return Response.json({});
  }) as typeof fetch;

  await activateSchedule("schedule/1", { projectID: "project-1", environmentID: "env-1" });
  await deleteSchedule("schedule/1", { projectID: "project-1", environmentID: "env-1" });

  expect(requestedUrls).toEqual([
    "/api/projects/project-1/environments/env-1/schedules/schedule%2F1/activate",
    "/api/projects/project-1/environments/env-1/schedules/schedule%2F1",
  ]);
});
