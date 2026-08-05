import { describe, expect, test } from "bun:test"

import { HelmrClient, actor, secrets, task, workspaces } from "./index"
import { installRuntimeOperations } from "./internal"

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
          run_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
        }, { status: 201 })
      }) as typeof fetch,
    })
    const signal = new AbortController().signal

    const run = await client.tasks.start<typeof resizeImage>("resize-image", {
      payload: { imageId: "image-1" },
      workspace: workspaces.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"),
      idempotencyKey: "image-1",
      concurrencyKey: "customer-1",
      retry: { maxAttempts: 3 },
      metadata: { source: "backend" },
      tags: ["image"],
    }, { signal })

    expect(run).toEqual({ id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31" })
    expect(requests[0]!.url).toBe(
      "https://api.example.test/v1/tasks/resize-image/start",
    )
    expect(JSON.parse(String(requests[0]!.init?.body))).toEqual({
      payload: { imageId: "image-1" },
      workspace: { id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32" },
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
  test("creates from a Sandbox and uses Workspace UUID refs", async () => {
    const requests: Array<{ url: string; init?: RequestInit }> = []
    const workspaceSnapshot = {
      id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
      key: "repository",
      sandbox_id: "repository-agent",
      deployment_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
      status: "available",
      secrets: [],
      last_activity_at: "2026-07-24T11:50:00Z",
      created_at: "2026-07-24T11:50:00Z",
      updated_at: "2026-07-24T11:50:00Z",
    }
    const responses: unknown[] = [
      workspaceSnapshot,
      { workspaces: [{ ...workspaceSnapshot, status: "recovery_required" }] },
      {
        exit_code: 0,
        stdout_base64: "b2sK",
        stderr_base64: "",
      },
      { workspace_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32" },
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

    await expect(client.sandboxes.createWorkspace("repository-agent", {
      secrets: [{
        secret: { name: "GITHUB_TOKEN" } as never,
        env: "GITHUB_TOKEN",
      }],
    })).rejects.toThrow("secrets.fromName()")
    expect(requests).toHaveLength(0)

    const inertRef = client.workspaces.ref(
      "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
    )
    expect(inertRef.id).toBe("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32")
    expect(requests).toHaveLength(0)

    const created = await client.sandboxes.createWorkspace(
      "repository-agent",
      {
        key: "repository",
        secrets: [
          { secret: secrets.fromName("GITHUB_TOKEN"), env: "GITHUB_TOKEN" },
          { secret: secrets.fromName("MODEL_CONFIG"), file: "/run/secrets/model.json" },
        ],
        idempotencyKey: "create-repository",
      },
      { signal },
    )
    expect(created.id).toBe("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32")
    expect(requests[0]!.url).toBe(
      "https://api.example.test/v1/sandboxes/repository-agent/workspaces",
    )
    expect(JSON.parse(String(requests[0]!.init?.body))).toEqual({
      key: "repository",
      secrets: [
        { name: "GITHUB_TOKEN", env: "GITHUB_TOKEN" },
        { name: "MODEL_CONFIG", file: "/run/secrets/model.json" },
      ],
      idempotency_key: "create-repository",
    })
    expect(requests[0]!.init?.signal).toBe(signal)

    const matches = await client.workspaces.list({ key: "repository" })
    expect(matches.items[0]?.sandboxId).toBe("repository-agent")
    expect(matches.items[0]?.status).toBe("recovery_required")
    expect(requests[1]!.url).toBe(
      "https://api.example.test/v1/workspaces?key=repository",
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
    expect(requests[3]!.init?.method).toBe("DELETE")
    expect(JSON.parse(String(requests[3]!.init?.body))).toEqual({
      idempotency_key: "delete-1",
    })
  })
})

describe("HelmrClient Actors", () => {
  test("starts an Actor and addresses the resulting Session by UUID", async () => {
    const requests: Array<{ url: string; init?: RequestInit }> = []
    const responses: unknown[] = [
      {
        session_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
        run_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
      },
      {
        id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36",
        sequence: 1,
        data: { type: "continue" },
        source: { kind: "external" },
        created_at: "2026-07-24T11:50:01Z",
      },
      {
        id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
        actor_id: "operator",
        deployment_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
        key: "thread:1",
        status: "open",
        current_run_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
        created_at: "2026-07-24T11:50:00Z",
        updated_at: "2026-07-24T11:50:01Z",
      },
      {
        records: [{
          id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34",
          sequence: 1,
          data: { stage: "started" },
          content_type: "application/json",
          created_at: "2026-07-24T11:50:02Z",
          provenance: {
            run_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
            attempt_number: 1,
            deployment_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
          },
        }],
        next_after: 1,
        has_more: false,
      },
      {
        session_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
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
    const workspace = workspaces.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32")
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
    expect(started.run.id).toBe("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31")
    expect(JSON.parse(String(requests[0]!.init?.body))).toEqual({
      key: "thread:1",
      input: { type: "start" },
      workspace: { id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32" },
      idempotency_key: "actor-1",
      run: {
        concurrency_key: "thread:1",
        retry: { max_attempts: 2 },
        metadata: { source: "backend" },
        tags: ["agent"],
      },
    })
    expect(requests[0]!.init?.signal).toBe(signal)

    await started.session.input.send(
      { type: "continue" },
      { idempotencyKey: "input-1" },
      { signal },
    )
    expect(JSON.parse(String(requests[1]!.init?.body))).toEqual({
      input: { type: "continue" },
      idempotency_key: "input-1",
    })

    const status = await client.sessions.retrieve(started.session.id, { signal })
    expect(status).toMatchObject({
      id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
      key: "thread:1",
      status: "open",
      currentRunId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
    })
    expect(requests[2]!.url).toContain(
      "/v1/sessions/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
    )

    const records = await started.session.output.list(
      { after: 0, limit: 10 },
      { signal },
    )
    expect(records).toEqual([expect.objectContaining({
      id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34",
      sequence: 1,
      data: { stage: "started" },
    })])
    expect(requests[3]!.url).toContain(
      "/v1/sessions/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33/outputs?after=0&limit=10",
    )

    await started.session.close({ idempotencyKey: "close-1" }, { signal })
    expect(JSON.parse(String(requests[4]!.init?.body))).toEqual({
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
          id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37",
          status: "pending",
          callback_url: "https://api.example.test/v1/token-callbacks/token/secret",
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

    expect(token.id).toBe("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37")
    expect(requests).toHaveLength(1)
    expect(requests[0]!.url).toBe("https://api.example.test/v1/tokens")
    expect(requests[0]!.init?.method).toBe("POST")
    expect(requests[0]!.init?.headers).toMatchObject({
      Authorization: "Bearer api-key",
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
          error: {
            code: `token_error_${status}`,
            message: `request failed with ${status}`,
            details: { token_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37" },
          },
        }, { status })) as typeof fetch,
      })

      try {
        await client.tokens.retrieve("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37")
        throw new Error("expected Token retrieve to fail")
      } catch (error) {
        expect(error).toMatchObject({
          name: "HelmrError",
          message: `request failed with ${status}`,
          code: `token_error_${status}`,
          details: { token_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37" },
        })
      }
    },
  )
})

describe("HelmrClient Secrets", () => {
  test("uses stable ID refs and exact name collection lookup", async () => {
    const requests: Array<{ url: string; init?: RequestInit }> = []
    const secretID = "019c8f1e-9b42-7b2c-8a4c-4b3a7f9f6d21"
    const active = {
      id: secretID,
      name: "github-token",
      status: "active",
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
        next_cursor: "cursor-next",
      },
      {
        ...active,
        status: "revoked",
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

    await client.secrets.ref(secretID).retrieve({ signal })
    expect(requests[1]!.url).toBe(
      `https://api.example.test/v1/secrets/${secretID}`,
    )

    const secretRef = client.secrets.ref(secretID)
    expect(secretRef.id).toBe(secretID)
    const rotated = await secretRef.rotate({
      value: "second",
      idempotencyKey: "rotate-1",
    }, { signal })
    expect(rotated.rotatedAt?.toISOString()).toBe("2026-07-25T01:01:00.000Z")
    expect(requests[2]!.url).toBe(
      `https://api.example.test/v1/secrets/${secretID}/rotate`,
    )
    expect(JSON.parse(String(requests[2]!.init?.body))).toEqual({
      value: "second",
      idempotency_key: "rotate-1",
    })

    const page = await client.secrets.list(
      { cursor: "cursor-current", limit: 10 },
      { signal },
    )
    expect(page.nextCursor).toBe("cursor-next")
    expect(requests[3]!.url).toBe(
      "https://api.example.test/v1/secrets?cursor=cursor-current&limit=10",
    )

    const revoked = await secretRef.revoke(
      { idempotencyKey: "revoke-1" },
      { signal },
    )
    expect(revoked.status).toBe("revoked")
    expect(JSON.parse(String(requests[4]!.init?.body))).toEqual({
      idempotency_key: "revoke-1",
    })
  })

  test("rejects a non-v7 Secret ID before transport", () => {
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (() => {
        throw new Error("transport must not run")
      }) as typeof fetch,
    })

    expect(() =>
      client.secrets.ref("019c8f1e-9b42-4b2c-8a4c-4b3a7f9f6d21")
    ).toThrow("Secret ID must be a canonical UUIDv7")
  })
})

describe("HelmrClient Deployments", () => {
  test("distinguishes an absent current Deployment from retrieval", async () => {
    const responses: Response[] = [
      Response.json({
        error: {
          code: "no_current_deployment",
          message: "No current Deployment exists",
          details: {},
        },
      }, { status: 404 }),
      Response.json({
        id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
        version: "2026.07.25.1",
        project_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36",
        environment_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37",
        content_hash: "sha256:source",
        deployment_source: { digest: "sha256:artifact", size_bytes: 4096 },
        status: "deployed",
        created_at: "2026-07-25T10:00:00Z",
        deployed_at: "2026-07-25T10:01:00Z",
      }),
    ]
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async () => responses.shift()!) as typeof fetch,
    })

    await expect(client.deployments.current()).resolves.toBeNull()
    await expect(
      client.deployments.retrieve("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35"),
    ).resolves.toEqual({
      id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
      version: "2026.07.25.1",
      projectId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36",
      environmentId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37",
      contentHash: "sha256:source",
      deploymentSource: { digest: "sha256:artifact", sizeBytes: 4096 },
      status: "deployed",
      createdAt: "2026-07-25T10:00:00Z",
      deployedAt: "2026-07-25T10:01:00Z",
    })
  })
})

describe("HelmrClient Schedules", () => {
  test("retrieves and pages declarative Schedule status", async () => {
    const snapshot = {
      id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36",
      task_id: "scheduled-maintenance",
      workspace: { key: "maintenance" },
      workspace_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
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
              next_cursor: "cursor-next",
            })
          : Response.json(snapshot)
      }) as typeof fetch,
    })

    const retrieved = await client.schedules.retrieve(
      "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36",
    )
    const listed = await client.schedules.list({
      cursor: "cursor-previous",
      limit: 10,
    })

    expect(retrieved).toMatchObject({
      id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36",
      taskId: "scheduled-maintenance",
      status: "active",
      workspace: { key: "maintenance" },
      workspaceId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
    })
    expect(listed.items).toHaveLength(1)
    expect(listed.nextCursor).toBe("cursor-next")
    expect(requests[1]).toBe(
      "https://api.example.test/v1/schedules?cursor=cursor-previous&limit=10",
    )
  })

  test("rejects a non-v7 Workspace ID in a Schedule response", async () => {
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async () => Response.json({
        id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36",
        task_id: "scheduled-maintenance",
        workspace: { id: "60af6067-a253-47b5-915c-2b889fb132c7" },
        cron: { pattern: "0 * * * *", timezone: "UTC" },
        status: "active",
        created_at: "2026-07-24T11:00:00Z",
        updated_at: "2026-07-24T11:00:00Z",
      })) as typeof fetch,
    })

    await expect(client.schedules.retrieve(
      "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36",
    )).rejects.toThrow("Schedule workspace.id")
  })

  test("keeps an unresolved key-addressed Schedule unbound", async () => {
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async () => Response.json({
        id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36",
        task_id: "scheduled-maintenance",
        workspace: { key: "maintenance" },
        cron: { pattern: "0 * * * *", timezone: "UTC" },
        status: "pending_workspace",
        created_at: "2026-07-24T11:00:00Z",
        updated_at: "2026-07-24T11:00:00Z",
      })) as typeof fetch,
    })

    const schedule = await client.schedules.retrieve(
      "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36",
    )
    expect(schedule.workspace).toMatchObject({ key: "maintenance" })
    expect(schedule.workspaceId).toBeUndefined()
  })

  test("requires last_failure for an errored Schedule", async () => {
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async () => Response.json({
        id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36",
        task_id: "scheduled-maintenance",
        workspace: { key: "maintenance" },
        workspace_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
        cron: { pattern: "0 * * * *", timezone: "UTC" },
        status: "errored",
        created_at: "2026-07-24T11:00:00Z",
        updated_at: "2026-07-24T11:00:00Z",
      })) as typeof fetch,
    })

    await expect(client.schedules.retrieve(
      "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36",
    )).rejects.toThrow("must contain last_failure")
  })
})

describe("HelmrClient Sessions", () => {
  test("requires failure for an unsuccessful Session", async () => {
    const sessionId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33"
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async () => Response.json({
        id: sessionId,
        actor_id: "operator",
        deployment_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
        status: "failed",
        created_at: "2026-07-24T11:50:00Z",
        updated_at: "2026-07-24T11:50:01Z",
      })) as typeof fetch,
    })

    await expect(client.sessions.retrieve(sessionId)).rejects.toThrow(
      "inconsistent failure projection",
    )
  })

  test("validates Session failure code against its status", async () => {
    const sessionId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33"
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async () => Response.json({
        id: sessionId,
        actor_id: "operator",
        deployment_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
        status: "failed",
        failure: {
          code: "cancelled",
          message: "Session was cancelled",
          details: {},
        },
        created_at: "2026-07-24T11:50:00Z",
        updated_at: "2026-07-24T11:50:01Z",
      })) as typeof fetch,
    })

    await expect(client.sessions.retrieve(sessionId)).rejects.toThrow(
      "failure code is inconsistent",
    )
  })
})

describe("HelmrClient Runs", () => {
  const runID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"

  test("retrieves and lists the exact Run snapshot projection", async () => {
    const requests: string[] = []
    const snapshot = {
      id: runID,
      status: "running",
      entrypoint: { kind: "task", id: "resize-image" },
      deployment: {
        id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
        version: "2026.07.24.1",
      },
      workspace_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
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
          ? Response.json({ runs: [snapshot], next_cursor: "cursor-next" })
          : Response.json(snapshot)
      }) as typeof fetch,
    })

    const retrieved = await client.runs.retrieve(runID)
    const signal = new AbortController().signal
    const listed = await client.runs.list({
      status: ["running", "waiting"],
      cursor: "cursor-previous",
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
    expect(listed.nextCursor).toBe("cursor-next")
    expect(requests[1]).toBe(
      "https://api.example.test/v1/runs?status=running&status=waiting&cursor=cursor-previous&limit=10",
    )
  })

  test("reads finite structured logs and events with bound query cursors", async () => {
    const requests: Array<{ url: string; init?: RequestInit }> = []
    const responses = [
      {
        logs: [
          {
            id: "log-cursor",
            kind: "structured",
            run_id: runID,
            attempt_number: 2,
            level: "warn",
            message: "retrying",
            attributes: { dependency: "image-service" },
            at: "2026-07-24T11:50:02Z",
          },
          {
            id: "stderr-cursor",
            kind: "stderr",
            run_id: runID,
            attempt_number: 2,
            observed_sequence: 8,
            content_base64: "d2FybmluZwo=",
            bytes: 8,
            at: "2026-07-24T11:50:03Z",
          },
        ],
        next_cursor: "logs-next-cursor",
      },
      {
        events: [
          {
            id: "event-cursor",
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
      cursor: "logs-cursor",
      limit: 25,
      level: ["warn", "error"],
    }, { signal })
    const events = await client.runs.events(runID, {
      severity: "error",
    })

    expect(logs).toEqual({
      items: [
        {
          id: "log-cursor",
          kind: "structured",
          runId: runID,
          attemptNumber: 2,
          level: "warn",
          message: "retrying",
          attributes: { dependency: "image-service" },
          at: "2026-07-24T11:50:02Z",
        },
        {
          id: "stderr-cursor",
          kind: "stderr",
          runId: runID,
          attemptNumber: 2,
          observedSequence: 8,
          contentBase64: "d2FybmluZwo=",
          bytes: 8,
          at: "2026-07-24T11:50:03Z",
        },
      ],
      nextCursor: "logs-next-cursor",
    })
    expect(events.items[0]).toMatchObject({
      id: "event-cursor",
      runId: runID,
      severity: "error",
      attributes: { code: "task_failed" },
    })
    expect(requests[0]!.url).toBe(
      `https://api.example.test/v1/runs/${runID}/logs?cursor=logs-cursor&limit=25&level=warn&level=error`,
    )
    expect(requests[0]!.init?.signal).toBe(signal)
    expect(requests[1]!.url).toBe(
      `https://api.example.test/v1/runs/${runID}/events?severity=error`,
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
            id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
            version: "2026.07.24.1",
          },
          workspace_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
          current_attempt_number: 1,
          cause: { type: "api" },
          metadata: {},
          tags: [],
          failure: {
            code: "run_cancelled",
            message: "Run was cancelled",
            details: {},
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
      failure: {
        code: "run_cancelled",
        message: "Run was cancelled",
        details: {},
      },
    })
    expect(snapshot.failure).not.toBeInstanceOf(Error)
    expect(requests).toHaveLength(1)
    expect(requests[0]!.url).toBe(
      `https://api.example.test/v1/runs/${runID}/cancel`,
    )
    expect(requests[0]!.init?.method).toBe("POST")
    expect(requests[0]!.init?.body).toBeUndefined()
  })

  test("wait unwraps success and throws a recorded Run failure", async () => {
    const succeeded = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async () => Response.json({
        id: runID,
        status: "succeeded",
        entrypoint: { kind: "task", id: "resize-image" },
        deployment: {
          id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
          version: "2026.07.24.1",
        },
        workspace_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
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
          id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
          version: "2026.07.24.1",
        },
        workspace_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
        current_attempt_number: 3,
        cause: { type: "api" },
        metadata: {},
        tags: [],
        failure: {
          code: "task_failed",
          message: "resize failed",
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
        name: "RunFailure",
        message: "resize failed",
        code: "task_failed",
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
        id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
        version: "2026.07.24.1",
      },
      workspace_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
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
        id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37",
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
