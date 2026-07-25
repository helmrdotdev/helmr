import { describe, expect, test } from "bun:test"

import { HelmrClient, actor, task, workspaces } from "./index"
import type { WorkspaceDefinition } from "./index"
import { installRuntimeOperations } from "./internal"
import {
  HELMR_API_VERSION,
  HELMR_API_VERSION_HEADER,
  HELMR_SDK_VERSION,
  HELMR_SDK_VERSION_HEADER,
} from "./version"

describe("HelmrClient Tasks", () => {
  test("starts a typed Task by runtime declared ID", async () => {
    const requests: Array<{ url: string; init?: RequestInit }> = []
    const resizeImage = task({
      id: "resize-image",
      payload: {
        "~standard": {
          version: 1,
          vendor: "test",
          validate(value: unknown) {
            return { value: value as { imageId: string } }
          },
        },
      },
      run: async (payload) => ({ resized: payload.imageId }),
    })
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async (input: URL | RequestInfo, init?: RequestInit) => {
        requests.push({ url: String(input), init })
        return Response.json({
          run_id: "run_aaaaaaaaaaaaaaaaaaaaaaaaaa",
        }, { status: 201 })
      }) as typeof fetch,
    })
    const signal = new AbortController().signal

    const run = await client.tasks.start<typeof resizeImage>("resize-image", {
      payload: { imageId: "image-1" },
      workspace: workspaces.ref({
        id: "wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa",
      }),
      idempotencyKey: "image-1",
      concurrencyKey: "customer-1",
      retry: { maxAttempts: 3 },
      metadata: { source: "backend" },
      tags: ["image"],
    }, { signal })

    expect(run).toEqual({ id: "run_aaaaaaaaaaaaaaaaaaaaaaaaaa" })
    expect(requests[0]!.url).toBe(
      "https://api.example.test/api/tasks/resize-image/start",
    )
    expect(JSON.parse(String(requests[0]!.init?.body))).toEqual({
      payload: { imageId: "image-1" },
      workspace: { id: "wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa" },
      idempotency_key: "image-1",
      concurrency_key: "customer-1",
      retry: { max_attempts: 3 },
      metadata: { source: "backend" },
      tags: ["image"],
    })
    expect(requests[0]!.init?.signal).toBe(signal)
  })
})

describe("HelmrClient Workspaces", () => {
  test("uses declared IDs for typed create and key refs", async () => {
    const requests: Array<{ url: string; init?: RequestInit }> = []
    const responses: unknown[] = [
      { workspace_id: "wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa" },
      {
        id: "wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa",
        key: "repository",
        declared_id: "repository-agent",
        status: "available",
        secrets: [{ name: "github", env: "GITHUB_TOKEN" }],
        last_activity_at: "2026-07-24T11:50:00Z",
        created_at: "2026-07-24T11:50:00Z",
        updated_at: "2026-07-24T11:50:00Z",
      },
      {
        exit_code: 0,
        stdout_base64: "b2sK",
        stderr_base64: "",
      },
      { workspace_id: "wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa" },
    ]
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async (input: URL | RequestInfo, init?: RequestInit) => {
        requests.push({ url: String(input), init })
        return Response.json(responses.shift(), { status: 200 })
      }) as typeof fetch,
    })
    const repositoryWorkspace = null as unknown as WorkspaceDefinition
    const signal = new AbortController().signal

    const created = await client.workspaces.create<typeof repositoryWorkspace>(
      "repository-agent",
      {
        key: "repository",
        secrets: [{ name: "github", env: "GITHUB_TOKEN" }],
        idempotencyKey: "create-repository",
      },
      { signal },
    )
    expect(created.id).toBe("wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa")
    expect(requests[0]!.url).toBe(
      "https://api.example.test/api/workspaces/repository-agent/create",
    )
    expect(JSON.parse(String(requests[0]!.init?.body))).toEqual({
      key: "repository",
      secrets: [{ name: "github", env: "GITHUB_TOKEN" }],
      idempotency_key: "create-repository",
    })
    expect(requests[0]!.init?.signal).toBe(signal)

    const ref = client.workspaces.ref<typeof repositoryWorkspace>(
      "repository-agent",
      { key: "repository" },
    )
    const snapshot = await ref.retrieve()
    expect(snapshot.declaredId).toBe("repository-agent")
    expect(requests[1]!.url).toBe(
      "https://api.example.test/api/workspaces/by-key/repository-agent?key=repository",
    )

    const result = await created.exec({
      command: ["printf", "ok\n"],
      stdin: new TextEncoder().encode("input"),
      idempotencyKey: "exec-1",
    }, { signal })
    expect(new TextDecoder().decode(result.stdout)).toBe("ok\n")
    expect(JSON.parse(String(requests[2]!.init?.body))).toEqual({
      command: ["printf", "ok\n"],
      stdin_base64: "aW5wdXQ=",
      idempotency_key: "exec-1",
    })

    await created.delete({ idempotencyKey: "delete-1" }, { signal })
    expect(JSON.parse(String(requests[3]!.init?.body))).toEqual({
      idempotency_key: "delete-1",
    })
  })
})

describe("HelmrClient Actors", () => {
  test("uses declared IDs and client-bound refs for the complete Actor surface", async () => {
    const requests: Array<{ url: string; init?: RequestInit }> = []
    const responses: unknown[] = [
      {
        actor_id: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
        run_id: "run_aaaaaaaaaaaaaaaaaaaaaaaaaa",
      },
      { sequence: 1 },
      {
        id: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
        key: "thread:1",
        status: "open",
        current_run_id: "run_aaaaaaaaaaaaaaaaaaaaaaaaaa",
        created_at: "2026-07-24T11:50:00Z",
        updated_at: "2026-07-24T11:50:01Z",
      },
      {
        records: [{
          id: "arec_aaaaaaaaaaaaaaaaaaaaaaaaaa",
          sequence: 1,
          data: { stage: "started" },
          content_type: "application/json",
          created_at: "2026-07-24T11:50:02Z",
          provenance: {
            run_id: "run_aaaaaaaaaaaaaaaaaaaaaaaaaa",
            attempt_number: 1,
            deployment_id: "dep_aaaaaaaaaaaaaaaaaaaaaaaaaa",
          },
        }],
        next_after: 1,
        has_more: false,
      },
      {
        actor_id: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
        accepted_at: "2026-07-24T11:50:03Z",
      },
    ]
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async (input: URL | RequestInfo, init?: RequestInit) => {
        requests.push({ url: String(input), init })
        return Response.json(responses.shift(), { status: 200 })
      }) as typeof fetch,
    })
    const operator = actor({ id: "operator", run: async () => {} })
    const workspace = workspaces.ref({
      id: "wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa",
    })
    const signal = new AbortController().signal

    const started = await client.actors.start<typeof operator>(
      "operator",
      {
        key: "thread:1",
        input: { type: "start" },
        workspace,
        idempotencyKey: "actor-1",
        run: {
          concurrencyKey: "thread:1",
          retry: { maxAttempts: 2 },
          metadata: { source: "backend" },
          tags: ["agent"],
        },
      },
      { signal },
    )
    expect(started.run.id).toBe("run_aaaaaaaaaaaaaaaaaaaaaaaaaa")
    expect(JSON.parse(String(requests[0]!.init?.body))).toEqual({
      key: "thread:1",
      input: { type: "start" },
      workspace: { id: "wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa" },
      idempotency_key: "actor-1",
      run: {
        concurrency_key: "thread:1",
        retry: { max_attempts: 2 },
        metadata: { source: "backend" },
        tags: ["agent"],
      },
    })
    expect(requests[0]!.init?.signal).toBe(signal)

    await started.ref.input.send(
      { type: "continue" },
      { idempotencyKey: "input-1" },
      { signal },
    )
    expect(JSON.parse(String(requests[1]!.init?.body))).toEqual({
      actor_id: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
      input: { type: "continue" },
      idempotency_key: "input-1",
    })

    const status = await started.ref.status({ signal })
    expect(status).toMatchObject({
      id: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
      key: "thread:1",
      status: "open",
      currentRunId: "run_aaaaaaaaaaaaaaaaaaaaaaaaaa",
    })
    expect(requests[2]!.url).toContain(
      "/api/actors/operator/status?actor_id=act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
    )

    const records = await started.ref.output.list(
      { after: 0, limit: 10 },
      { signal },
    )
    expect(records).toEqual([expect.objectContaining({
      id: "arec_aaaaaaaaaaaaaaaaaaaaaaaaaa",
      sequence: 1,
      data: { stage: "started" },
    })])
    expect(requests[3]!.url).toContain(
      "/api/actors/operator/output?actor_id=act_aaaaaaaaaaaaaaaaaaaaaaaaaa&after=0&limit=10",
    )

    await started.ref.close({ idempotencyKey: "close-1" }, { signal })
    expect(JSON.parse(String(requests[4]!.init?.body))).toEqual({
      actor_id: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
      idempotency_key: "close-1",
    })
  })
})

describe("HelmrClient Tokens", () => {
  test("creates an Environment-scoped Token through authenticated REST", async () => {
    const requests: Array<{ url: string; init?: RequestInit }> = []
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async (input: URL | RequestInfo, init?: RequestInit) => {
        requests.push({ url: String(input), init })
        return Response.json({
          id: "tok_aaaaaaaaaaaaaaaaaaaaaaaaaa",
          status: "pending",
          callback_url: "https://api.example.test/api/token-callbacks/token/secret",
          public_access_token: "hlmr_pat_secret",
          timeout_at: "2026-07-24T12:00:00Z",
          metadata: { approval: true },
          tags: ["review"],
          created_at: "2026-07-24T11:50:00Z",
          updated_at: "2026-07-24T11:50:00Z",
        }, { status: 201 })
      }) as typeof fetch,
    })
    const signal = new AbortController().signal

    const token = await client.tokens.create({
      timeout: "10m",
      metadata: { approval: true },
      tags: ["review"],
      idempotencyKey: "approval-1",
    }, { signal })

    expect(token.id).toBe("tok_aaaaaaaaaaaaaaaaaaaaaaaaaa")
    expect(requests).toHaveLength(1)
    expect(requests[0]!.url).toBe("https://api.example.test/api/tokens")
    expect(requests[0]!.init?.method).toBe("POST")
    expect(requests[0]!.init?.headers).toMatchObject({
      Authorization: "Bearer api-key",
      [HELMR_API_VERSION_HEADER]: HELMR_API_VERSION,
      [HELMR_SDK_VERSION_HEADER]: HELMR_SDK_VERSION,
    })
    expect(JSON.parse(String(requests[0]!.init?.body))).toEqual({
      timeout: "10m",
      metadata: { approval: true },
      tags: ["review"],
      idempotency_key: "approval-1",
    })
    expect(requests[0]!.init?.signal).toBe(signal)
  })

  test.each([400, 401, 403, 409, 410])(
    "preserves structured Helmr errors for status %i",
    async (status) => {
      const client = new HelmrClient({
        url: "https://api.example.test",
        apiKey: "api-key",
        fetch: (async () => Response.json({
          error: `request failed with ${status}`,
          code: `token_error_${status}`,
          retryable: status === 409,
          request_id: `request-${status}`,
        }, { status })) as typeof fetch,
      })

      try {
        await client.tokens.retrieve("tok_aaaaaaaaaaaaaaaaaaaaaaaaaa")
        throw new Error("expected Token retrieve to fail")
      } catch (error) {
        expect(error).toMatchObject({
          name: "HelmrError",
          message: `request failed with ${status}`,
          code: `token_error_${status}`,
          retryable: status === 409,
          requestId: `request-${status}`,
        })
      }
    },
  )
})

describe("HelmrClient Secrets", () => {
  test("uses stable ID and name refs with flat mutation requests", async () => {
    const requests: Array<{ url: string; init?: RequestInit }> = []
    const secretID = "019c8f1e-9b42-7b2c-8a4c-4b3a7f9f6d21"
    const active = {
      id: secretID,
      name: "github-token",
      state: "active",
      created_at: "2026-07-25T01:00:00Z",
    }
    const responses: unknown[] = [
      active,
      active,
      {
        ...active,
        rotated_at: "2026-07-25T01:01:00Z",
      },
      {
        secrets: [active],
        next_cursor: "sec1.next",
      },
      {
        ...active,
        state: "revoked",
        revoked_at: "2026-07-25T01:02:00Z",
      },
    ]
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async (input: URL | RequestInfo, init?: RequestInit) => {
        requests.push({ url: String(input), init })
        return Response.json(responses.shift(), { status: 200 })
      }) as typeof fetch,
    })
    const signal = new AbortController().signal

    const created = await client.secrets.create({
      name: "github-token",
      value: "first",
      idempotencyKey: "create-1",
    }, { signal })
    expect(created.id).toBe(secretID)
    expect(JSON.parse(String(requests[0]!.init?.body))).toEqual({
      name: "github-token",
      value: "first",
      idempotency_key: "create-1",
    })
    expect(requests[0]!.init?.signal).toBe(signal)

    await client.secrets.ref({ id: secretID }).retrieve({ signal })
    expect(requests[1]!.url).toBe(
      `https://api.example.test/api/secrets/${secretID}`,
    )

    const byName = client.secrets.ref({ name: "github-token" })
    const rotated = await byName.rotate({
      value: "second",
      idempotencyKey: "rotate-1",
    }, { signal })
    expect(rotated.rotatedAt?.toISOString()).toBe("2026-07-25T01:01:00.000Z")
    expect(requests[2]!.url).toBe(
      "https://api.example.test/api/secrets/by-name/github-token/rotate",
    )
    expect(JSON.parse(String(requests[2]!.init?.body))).toEqual({
      value: "second",
      idempotency_key: "rotate-1",
    })

    const page = await client.secrets.list(
      { cursor: "sec1.current", limit: 10 },
      { signal },
    )
    expect(page.nextCursor).toBe("sec1.next")
    expect(requests[3]!.url).toBe(
      "https://api.example.test/api/secrets?cursor=sec1.current&limit=10",
    )

    const revoked = await byName.revoke(
      { idempotencyKey: "revoke-1" },
      { signal },
    )
    expect(revoked.state).toBe("revoked")
    expect(JSON.parse(String(requests[4]!.init?.body))).toEqual({
      idempotency_key: "revoke-1",
    })
  })
})

describe("HelmrClient Deployments", () => {
  test("distinguishes an absent current Deployment from retrieval", async () => {
    const responses: unknown[] = [
      { deployment: null },
      {
        id: "dep_aaaaaaaaaaaaaaaaaaaaaaaaaa",
        version: "2026.07.25.1",
        tasks: ["resize-image"],
        actors: ["operator"],
        workspaces: ["repository-agent"],
      },
    ]
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async () =>
        Response.json(responses.shift(), { status: 200 })) as typeof fetch,
    })

    await expect(client.deployments.current()).resolves.toBeNull()
    await expect(
      client.deployments.retrieve("dep_aaaaaaaaaaaaaaaaaaaaaaaaaa"),
    ).resolves.toEqual({
      id: "dep_aaaaaaaaaaaaaaaaaaaaaaaaaa",
      version: "2026.07.25.1",
      tasks: ["resize-image"],
      actors: ["operator"],
      workspaces: ["repository-agent"],
    })
  })
})

describe("HelmrClient Schedules", () => {
  test("retrieves and pages declarative Schedule status", async () => {
    const snapshot = {
      id: "sch_aaaaaaaaaaaaaaaaaaaaaaaaaa",
      task: "scheduled-maintenance",
      workspace: { key: "maintenance" },
      cron: { pattern: "0 * * * *", timezone: "UTC" },
      status: "active",
      generation: 1,
      effective_from: "2026-07-24T11:00:00Z",
      next_fire_at: "2026-07-24T12:00:00Z",
      created_at: "2026-07-24T11:00:00Z",
      updated_at: "2026-07-24T11:00:00Z",
    }
    const requests: string[] = []
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async (input: URL | RequestInfo) => {
        requests.push(String(input))
        return String(input).includes("?")
          ? Response.json({
              schedules: [snapshot],
              next_cursor: "sc1.next",
            })
          : Response.json(snapshot)
      }) as typeof fetch,
    })

    const retrieved = await client.schedules.retrieve(
      "sch_aaaaaaaaaaaaaaaaaaaaaaaaaa",
    )
    const listed = await client.schedules.list({
      cursor: "sc1.previous",
      limit: 10,
    })

    expect(retrieved).toMatchObject({
      id: "sch_aaaaaaaaaaaaaaaaaaaaaaaaaa",
      task: "scheduled-maintenance",
      status: "active",
      workspace: { key: "maintenance" },
    })
    expect(listed.items).toHaveLength(1)
    expect(listed.nextCursor).toBe("sc1.next")
    expect(requests[1]).toBe(
      "https://api.example.test/api/schedules?cursor=sc1.previous&limit=10",
    )
  })
})

describe("HelmrClient Runs", () => {
  const runID = "run_aaaaaaaaaaaaaaaaaaaaaaaaaa"

  test("retrieves and lists the exact Run snapshot projection", async () => {
    const requests: string[] = []
    const snapshot = {
      id: runID,
      status: "running",
      entrypoint: { kind: "task", id: "resize-image" },
      deployment: {
        id: "dep_aaaaaaaaaaaaaaaaaaaaaaaaaa",
        version: "2026.07.24.1",
      },
      workspace_id: "wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa",
      current_attempt_number: 2,
      cause: { type: "api" },
      metadata: { source: "backend" },
      tags: ["image"],
      created_at: "2026-07-24T11:50:00Z",
      started_at: "2026-07-24T11:50:01Z",
    }
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async (input: URL | RequestInfo) => {
        requests.push(String(input))
        return String(input).includes("?")
          ? Response.json({ runs: [snapshot], next_cursor: "rn1.next" })
          : Response.json(snapshot)
      }) as typeof fetch,
    })

    const retrieved = await client.runs.retrieve(runID)
    const signal = new AbortController().signal
    const listed = await client.runs.list({
      status: ["running", "waiting"],
      cursor: "rn1.previous",
      limit: 10,
    }, { signal })

    expect(retrieved).toMatchObject({
      id: runID,
      status: "running",
      entrypoint: { kind: "task", id: "resize-image" },
      currentAttemptNumber: 2,
      cause: { type: "api" },
    })
    expect(listed.items).toHaveLength(1)
    expect(listed.nextCursor).toBe("rn1.next")
    expect(requests[1]).toBe(
      "https://api.example.test/api/runs?status=running&status=waiting&cursor=rn1.previous&limit=10",
    )
  })

  test("reads finite structured logs and events with bound query cursors", async () => {
    const requests: Array<{ url: string; init?: RequestInit }> = []
    const responses = [
      {
        logs: [
          {
            id: "rt1.log",
            kind: "structured",
            run_id: runID,
            attempt_number: 2,
            level: "warn",
            message: "retrying",
            attributes: { dependency: "image-service" },
            at: "2026-07-24T11:50:02Z",
          },
          {
            id: "rt1.stderr",
            kind: "stderr",
            run_id: runID,
            attempt_number: 2,
            observed_sequence: 8,
            content_base64: "d2FybmluZwo=",
            bytes: 8,
            at: "2026-07-24T11:50:03Z",
          },
        ],
        next_cursor: "rt1.logs-next",
      },
      {
        events: [
          {
            id: "rt1.event",
            run_id: runID,
            attempt_number: 2,
            category: "lifecycle",
            severity: "error",
            source: "runtime",
            kind: "run.failed",
            message: "Task failed",
            attributes: { code: "task_failed" },
            occurred_at: "2026-07-24T11:50:04Z",
            at: "2026-07-24T11:50:05Z",
          },
        ],
      },
    ]
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async (input: URL | RequestInfo, init?: RequestInit) => {
        requests.push({ url: String(input), init })
        return Response.json(responses.shift())
      }) as typeof fetch,
    })
    const signal = new AbortController().signal

    const logs = await client.runs.logs(runID, {
      cursor: "rt1.logs",
      limit: 25,
      level: ["warn", "error"],
    }, { signal })
    const events = await client.runs.events(runID, {
      severity: "error",
    })

    expect(logs).toEqual({
      items: [
        {
          id: "rt1.log",
          kind: "structured",
          runId: runID,
          attemptNumber: 2,
          level: "warn",
          message: "retrying",
          attributes: { dependency: "image-service" },
          at: "2026-07-24T11:50:02Z",
        },
        {
          id: "rt1.stderr",
          kind: "stderr",
          runId: runID,
          attemptNumber: 2,
          observedSequence: 8,
          contentBase64: "d2FybmluZwo=",
          bytes: 8,
          at: "2026-07-24T11:50:03Z",
        },
      ],
      nextCursor: "rt1.logs-next",
    })
    expect(events.items[0]).toMatchObject({
      id: "rt1.event",
      runId: runID,
      severity: "error",
      attributes: { code: "task_failed" },
    })
    expect(requests[0]!.url).toBe(
      `https://api.example.test/api/runs/${runID}/logs?cursor=rt1.logs&limit=25&level=warn&level=error`,
    )
    expect(requests[0]!.init?.signal).toBe(signal)
    expect(requests[1]!.url).toBe(
      `https://api.example.test/api/runs/${runID}/events?severity=error`,
    )
  })

  test("cancels a Run and returns its terminal snapshot", async () => {
    const requests: Array<{ url: string; init?: RequestInit }> = []
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async (input: URL | RequestInfo, init?: RequestInit) => {
        requests.push({ url: String(input), init })
        return Response.json({
          id: runID,
          status: "cancelled",
          entrypoint: { kind: "task", id: "resize-image" },
          deployment: {
            id: "dep_aaaaaaaaaaaaaaaaaaaaaaaaaa",
            version: "2026.07.24.1",
          },
          workspace_id: "wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa",
          current_attempt_number: 1,
          cause: { type: "api" },
          metadata: {},
          tags: [],
          terminal_reason_code: "run_cancelled",
          error: {
            code: "run_cancelled",
            message: "Run was cancelled",
            retryable: false,
          },
          created_at: "2026-07-24T11:50:00Z",
          terminal_at: "2026-07-24T11:50:05Z",
        })
      }) as typeof fetch,
    })

    const snapshot = await client.runs.cancel(runID)

    expect(snapshot).toMatchObject({
      id: runID,
      status: "cancelled",
      terminalReasonCode: "run_cancelled",
      error: {
        code: "run_cancelled",
        message: "Run was cancelled",
        retryable: false,
      },
    })
    expect(requests).toHaveLength(1)
    expect(requests[0]!.url).toBe(
      `https://api.example.test/api/runs/${runID}/cancel`,
    )
    expect(requests[0]!.init?.method).toBe("POST")
    expect(requests[0]!.init?.body).toBeUndefined()
  })

  test("wait unwraps success and throws a recorded RunError", async () => {
    const succeeded = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async () => Response.json({
        id: runID,
        status: "succeeded",
        entrypoint: { kind: "task", id: "resize-image" },
        deployment: {
          id: "dep_aaaaaaaaaaaaaaaaaaaaaaaaaa",
          version: "2026.07.24.1",
        },
        workspace_id: "wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa",
        current_attempt_number: 1,
        cause: { type: "api" },
        metadata: {},
        tags: [],
        output: { resized: "image-1" },
        created_at: "2026-07-24T11:50:00Z",
        terminal_at: "2026-07-24T11:50:05Z",
      })) as typeof fetch,
    })
    await expect(succeeded.runs.wait(runID).unwrap()).resolves.toEqual({
      resized: "image-1",
    })

    const failed = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async () => Response.json({
        id: runID,
        status: "failed",
        entrypoint: { kind: "task", id: "resize-image" },
        deployment: {
          id: "dep_aaaaaaaaaaaaaaaaaaaaaaaaaa",
          version: "2026.07.24.1",
        },
        workspace_id: "wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa",
        current_attempt_number: 3,
        cause: { type: "api" },
        metadata: {},
        tags: [],
        terminal_reason_code: "task_failed",
        error: {
          code: "task_failed",
          message: "resize failed",
          retryable: false,
          details: { imageId: "image-1" },
        },
        created_at: "2026-07-24T11:50:00Z",
        terminal_at: "2026-07-24T11:50:05Z",
      })) as typeof fetch,
    })
    try {
      await failed.runs.wait(runID).unwrap()
      throw new Error("expected Run wait to fail")
    } catch (error) {
      expect(error).toMatchObject({
        name: "RunError",
        message: "resize failed",
        code: "task_failed",
        retryable: false,
        details: { imageId: "image-1" },
      })
    }
  })

  test("polls active Runs and honors AbortSignal between requests", async () => {
    let requests = 0
    const active = {
      id: runID,
      status: "running",
      entrypoint: { kind: "task", id: "resize-image" },
      deployment: {
        id: "dep_aaaaaaaaaaaaaaaaaaaaaaaaaa",
        version: "2026.07.24.1",
      },
      workspace_id: "wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa",
      current_attempt_number: 1,
      cause: { type: "api" },
      metadata: {},
      tags: [],
      created_at: "2026-07-24T11:50:00Z",
      started_at: "2026-07-24T11:50:01Z",
    }
    const polling = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async () => {
        requests++
        return Response.json(requests === 1
          ? active
          : {
              ...active,
              status: "succeeded",
              output: { resized: "image-1" },
              terminal_at: "2026-07-24T11:50:05Z",
            })
      }) as typeof fetch,
    })
    await expect(polling.runs.wait(runID).unwrap()).resolves.toEqual({
      resized: "image-1",
    })
    expect(requests).toBe(2)

    requests = 0
    const controller = new AbortController()
    const aborting = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async () => {
        requests++
        return Response.json(active)
      }) as typeof fetch,
    })
    const waiting = aborting.runs.wait(runID, { signal: controller.signal }).unwrap()
    await Promise.resolve()
    controller.abort(new Error("stop waiting"))
    await expect(waiting).rejects.toThrow("stop waiting")
    const requestsAtAbort = requests
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(requests).toBe(requestsAtAbort)
  })

  test("rejects external wait inside the managed runtime", () => {
    const uninstall = installRuntimeOperations({
      waitFor: async () => {},
      waitUntil: async () => {},
      actorInputSend: async () => ({ sequence: 1 }),
      tokenCreate: async () => ({
        id: "tok_aaaaaaaaaaaaaaaaaaaaaaaaaa",
        status: "pending",
        timeoutAt: "2026-07-24T12:00:00Z",
        metadata: {},
        tags: [],
        createdAt: "2026-07-24T11:50:00Z",
        updatedAt: "2026-07-24T11:50:00Z",
        callbackUrl: "https://api.example.test/callback",
        publicAccessToken: "hlmr_pat_secret",
      }),
      tokenWait: async () => null,
    })
    try {
      const client = new HelmrClient({
        url: "https://api.example.test",
        apiKey: "api-key",
        fetch: (async () => {
          throw new Error("fetch must not be called")
        }) as typeof fetch,
      })
      expect(() => client.runs.wait(runID)).toThrow(
        "client.runs.wait() is unavailable inside an active Helmr Run",
      )
    } finally {
      uninstall()
    }
  })
})
