import { afterEach, expect, test } from "bun:test";

import { activateSchedule, createSchedule, deleteSchedule, listSchedules, updateSchedule } from "./schedules";

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

  expect(requestedUrl).toBe("/api/schedules?project_id=project-1&environment_id=env-1");
});

test("creates schedules with required workspace source", async () => {
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
    workspace: {
      repository: "owner/repo",
      ref: "main",
    },
    active: true,
  });

  expect(requestedUrl).toBe("/api/schedules");
  expect(requestedBody).toEqual({
    project_id: "project-1",
    environment_id: "env-1",
    deduplication_key: "task-hourly",
    task: "task",
    cron: "0 * * * *",
    timezone: "UTC",
    workspace: {
      repository: "owner/repo",
      ref: "main",
    },
    active: true,
  });
});

test("creates schedules with a required schedule key", async () => {
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
    workspace: {
      repository: "owner/repo",
      ref: "main",
    },
  });

  expect(requestedBody).toEqual({
    project_id: "project-1",
    environment_id: "env-1",
    deduplication_key: "task-hourly",
    task: "task",
    cron: "0 * * * *",
    workspace: {
      repository: "owner/repo",
      ref: "main",
    },
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
    "/api/schedules/schedule%2F1/activate?project_id=project-1&environment_id=env-1",
    "/api/schedules/schedule%2F1?project_id=project-1&environment_id=env-1",
  ]);
});

test("updates schedules with scope query", async () => {
  let requestedUrl: string | undefined;
  let requestedMethod: string | undefined;
  let requestedBody: unknown;
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input);
    requestedMethod = init?.method;
    requestedBody = JSON.parse(String(init?.body));
    return Response.json({
      id: "schedule-1",
      project_id: "project-1",
      environment_id: "env-1",
      task: "task",
      deduplication_key: "task-hourly",
      cron: "*/10 * * * *",
      timezone: "UTC",
      active: true,
      status: "active",
      created_at: "2026-06-01T00:00:00Z",
      updated_at: "2026-06-01T00:00:00Z",
    });
  }) as typeof fetch;

  await updateSchedule("schedule/1", {
    project_id: "project-1",
    environment_id: "env-1",
    task: "task",
    cron: "*/10 * * * *",
    workspace: {
      repository: "owner/repo",
      ref: "main",
    },
  });

  expect(requestedUrl).toBe("/api/schedules/schedule%2F1?project_id=project-1&environment_id=env-1");
  expect(requestedMethod).toBe("PUT");
  expect(requestedBody).toEqual({
    project_id: "project-1",
    environment_id: "env-1",
    task: "task",
    cron: "*/10 * * * *",
    workspace: {
      repository: "owner/repo",
      ref: "main",
    },
  });
});
