import { describe, expect, test } from "bun:test"

import { HelmrClient } from "./index"

describe("HelmrClient Computers", () => {
  test("creates from a Template definition and uses Computer UUID refs", async () => {
    const requests: Array<{ url: string; init?: RequestInit }> = []
    const computer = {
      id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
      template: "repository-agent",
      key: "repository",
      delete_after_unused_ms: null, unused_since: null, execution_wait: null,
      deployment_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
      status: "available",
      residency: "cold",
      secrets: [],
      last_activity_at: "2026-07-24T11:50:00Z",
      created_at: "2026-07-24T11:50:00Z",
      updated_at: "2026-07-24T11:50:00Z",
    }
    const responses: unknown[] = [
      computer,
		computer,
		{
			computers: [{
				id: computer.id,
				key: computer.key,
				template: computer.template,
      delete_after_unused_ms: null, unused_since: null, execution_wait: null,
				deployment_id: computer.deployment_id,
				status: "available",
 residency: "unavailable",
 error: { code: "computer_recovery_required", message: "Computer requires recovery" },
				last_activity_at: computer.last_activity_at,
				created_at: computer.created_at,
				updated_at: computer.updated_at,
			}],
		},
      { command_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36" },
      { computer_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32", sessions_cancelled: 1, commands_cancelled: 0 },
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

    await expect(client.computers.create({ template: "repository-agent", secrets: [{
        secret: { name: "GITHUB_TOKEN" } as never,
        env: { name: "GITHUB_TOKEN", mode: "raw" },
      }], })).rejects.toThrow("Secret name")
    expect(requests).toHaveLength(0)

    const inertRef = client.computers.ref(
      "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
    )
    expect(inertRef.id).toBe("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32")
    expect(requests).toHaveLength(0)

    const created = await client.computers.create({ template: "repository-agent", key: "repository",
        secrets: [
          { secret: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc38", env: { name: "GITHUB_TOKEN", mode: "raw" } },
          { secret: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc39", file: { path: "/run/secrets/model.json" } },
        ],
        idempotencyKey: "create-repository", }, { signal })
    expect(created.id).toBe("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32")
    expect(requests[0]!.url).toBe(
      "https://api.example.test/v1/computers",
    )
    expect(JSON.parse(String(requests[0]!.init?.body))).toEqual({
      template: "repository-agent",
      key: "repository",
      secrets: [
        { secret: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc38", env: { name: "GITHUB_TOKEN", mode: "raw" } },
        { secret: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc39", file: { path: "/run/secrets/model.json" } },
      ],
      idempotency_key: "create-repository",
    })
    expect(requests[0]!.init?.signal).toBe(signal)

    const retrieved = await created.get({ signal })
    expect(retrieved).toMatchObject({
      id: computer.id,
      key: "repository",
      template: "repository-agent",
    })
    expect(requests[1]!.url).toBe(
      "https://api.example.test/v1/computers/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
    )

    const matches = await client.computers.list({ key: "repository" })
    expect(matches.items[0]?.template).toBe("repository-agent")
    expect(matches.items[0]?.residency).toBe("unavailable")
    expect(requests[2]!.url).toBe(
      "https://api.example.test/v1/computers?key=repository",
    )

    const result = await created.exec({
      command: ["printf", "ok\n"],
      stdin: new TextEncoder().encode("input"),
      idempotencyKey: "exec-1",
    }, { signal })
    expect(result.id).toBe("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36")
    expect(JSON.parse(String(requests[3]!.init?.body))).toEqual({
      command: ["printf", "ok\n"],
      stdin_base64: "aW5wdXQ=",
      idempotency_key: "exec-1",
    })

    responses.unshift(computer)
    const preserved = await created.update({ deleteAfterUnused: "never" })
    expect(preserved.deleteAfterUnused).toBe("never")
    expect(requests[4]!.init?.method).toBe("PATCH")
    expect(JSON.parse(String(requests[4]!.init?.body))).toEqual({ delete_after_unused_ms: null })

    expect(await created.delete({ idempotencyKey: "delete-1" }, { signal })).toEqual({ computerId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32", sessionsCancelled: 1, commandsCancelled: 0 })
    expect(requests[5]!.init?.method).toBe("DELETE")
    expect(JSON.parse(String(requests[5]!.init?.body))).toEqual({
      idempotency_key: "delete-1",
    })
  })

  test("returns an admitted Exec and retrieves its nonzero outcome independently", async () => {
    const computerId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"
    const commandId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36"
    const requests: string[] = []
    const outcome = { command_id: commandId, kind: "exited", exit_code: 17, terminal_at: "2026-09-26T00:00:00Z" }
    const responses = [
      { command_id: commandId },
      { id: commandId, computer_id: computerId, status: "exited", process_reconciled: false, outcome },
      { id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37", computer_id: computerId, status: "running", process_reconciled: false },
    ]
    const client = new HelmrClient({
      url: "https://api.example.test", apiKey: "api-key",
      fetch: (async (input: URL | RequestInfo) => {
        requests.push(String(input))
        return Response.json(responses.shift())
      }) as typeof fetch,
    })
    const ref = await client.computers.ref(computerId).exec({ command: ["false"] })
    expect(ref.id).toBe(commandId)
    expect(requests).toHaveLength(1)
    const reconstructed = client.commands.ref(commandId)
    expect(requests).toHaveLength(1)
    expect(await reconstructed.wait()).toMatchObject({ commandId, kind: "exited", exitCode: 17 })
    expect(requests[1]).toBe(`https://api.example.test/v1/commands/${commandId}`)
    await expect(ref.get()).rejects.toThrow("changed ID")
  })
})

describe("HelmrClient transport errors", () => {
  test.each([400, 401, 403, 409, 410])(
    "preserves structured Helmr errors for status %i",
    async (status) => {
      const client = new HelmrClient({
        url: "https://api.example.test",
        apiKey: "api-key",
        fetch: (async () => Response.json({
          error: {
            code: `session_error_${status}`,
            message: `request failed with ${status}`,
            details: { session_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37" },
          },
        }, {
          status,
          headers: { "X-Request-ID": "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc38" },
        })) as typeof fetch,
      })

      try {
        await client.sessions.get("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37")
        throw new Error("expected Session get to fail")
      } catch (error) {
        expect(error).toMatchObject({
          name: "HelmrError",
          message: `request failed with ${status}`,
          code: `session_error_${status}`,
          requestId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc38",
          details: { session_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37" },
        })
      }
    },
  )

  test("preserves request correlation when the error body is malformed", async () => {
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async () => new Response("not JSON", {
        status: 502,
        headers: { "X-Request-ID": "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc39" },
      })) as typeof fetch,
    })

    try {
      await client.sessions.get("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37")
      throw new Error("expected Session get to fail")
    } catch (error) {
      expect(error).toMatchObject({
        name: "HelmrError",
        code: "bad_gateway",
        requestId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc39",
      })
    }
  })
})

describe("HelmrClient Secrets", () => {
  test("uses one public identifier for creation, refs and retrieval", async () => {
    const requests: Array<{ url: string; init?: RequestInit }> = []
    const secretID = "github-token"
    const active = {
      secret: secretID,
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
      secret: "github-token",
      value: "first",
      idempotencyKey: "create-1",
    }, { signal })
    expect(created.secret).toBe(secretID)
    expect(JSON.parse(String(requests[0]!.init?.body))).toEqual({
      secret: "github-token",
      value: "first",
      idempotency_key: "create-1",
    })
    expect(requests[0]!.init?.signal).toBe(signal)

    await client.secrets.get(secretID, { signal })
    expect(requests[1]!.url).toBe(
      `https://api.example.test/v1/secrets/${secretID}`,
    )

    const secretRef = client.secrets.ref(secretID)
    expect(secretRef.secret).toBe(secretID)
    const rotated = await secretRef.rotate({
      value: "second",
      idempotencyKey: "rotate-1",
    }, { signal })
    expect(rotated.rotatedAt).toBe("2026-07-25T01:01:00Z")
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

  test("generates a fresh idempotency key per Secret rotation and revocation", async () => {
    const bodies: Array<{ idempotency_key?: string }> = []
    const active = { secret: "github-token", status: "active", created_at: "2026-07-25T01:00:00Z" }
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async (_input: URL | RequestInfo, init?: RequestInit) => {
        bodies.push(JSON.parse(String(init?.body)))
        return Response.json(active)
      }) as typeof fetch,
    })
    const secretRef = client.secrets.ref("github-token")
    await secretRef.rotate({ value: "second" })
    await secretRef.rotate({ value: "third" })
    await secretRef.revoke()
    const keys = bodies.map((body) => body.idempotency_key)
    expect(keys.every((key) => typeof key === "string" && key !== "")).toBe(true)
    expect(new Set(keys).size).toBe(3)
  })

  test("rejects a malformed Secret ID before transport", () => {
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (() => {
        throw new Error("transport must not run")
      }) as typeof fetch,
    })

    expect(() =>
      client.secrets.ref("bad/name")
    ).toThrow("Secret name is invalid")
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
        bundle_digest: "sha256:bundle",
        created_at: "2026-07-25T10:00:00Z",
      }),
    ]
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async () => responses.shift()!) as typeof fetch,
    })

    await expect(client.deployments.current()).resolves.toBeNull()
    await expect(
      client.deployments.get("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35"),
    ).resolves.toEqual({
      id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
      version: "2026.07.25.1",
      bundleDigest: "sha256:bundle",
      createdAt: "2026-07-25T10:00:00Z",
    })
  })

  test("lists bounded Deployment projections", async () => {
    const requests: string[] = []
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async (input: URL | RequestInfo) => {
        requests.push(String(input))
        return Response.json({
          deployments: [{
            id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
            version: "2026.07.25.1",
            bundle_digest: "sha256:bundle",
            created_at: "2026-07-25T10:00:00Z",
          }],
          next_cursor: "cursor-next",
        })
      }) as typeof fetch,
    })

    const page = await client.deployments.list({ cursor: "cursor-current", limit: 10 })
    expect(page.items[0]).toEqual({
      id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
      version: "2026.07.25.1",
      bundleDigest: "sha256:bundle",
      createdAt: "2026-07-25T10:00:00Z",
    })
    expect(page.nextCursor).toBe("cursor-next")
    expect(requests[0]).toBe(
      "https://api.example.test/v1/deployments?cursor=cursor-current&limit=10",
    )
  })
})

describe("HelmrClient Triggers", () => {
  const id = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36"
  const trigger = { id, agent:"reporter", deployment_id:id, trigger_key:"daily", input:[{type:"text",text:"Create the report"}], cron:{pattern:"0 * * * *",timezone:"UTC"}, active_from:"2026-07-24T11:00:00Z", next_fire_at:"2026-07-24T12:00:00Z" }
  test("retrieves pinned activations and pages Agent-filtered triggers", async () => {
    const requests:string[]=[]
    const client = new HelmrClient({url:"https://api.example.test",apiKey:"api-key",fetch:(async (input: URL | RequestInfo) => {
      requests.push(String(input)); return String(input).includes("?") ? Response.json({triggers:[trigger],next_cursor:"next"}) : Response.json(trigger)
    }) as typeof fetch})
    expect(await client.triggers.get(id)).toMatchObject({id,agent:"reporter",deploymentId:id,triggerKey:"daily",input:[{type:"text",text:"Create the report"}],activeFrom:trigger.active_from})
    const page = await client.triggers.list({agent:"reporter",limit:2,cursor:"previous"})
    expect(page.items).toHaveLength(1)
    expect(page.nextCursor).toBe("next")
    expect(requests[1]).toBe(`https://api.example.test/v1/triggers?agent=reporter&cursor=previous&limit=2`)
  })
  test("requires the pinned Agent and Deployment identities",async()=>{
    for (const field of ["id","deployment_id"] as const) {
      const client = new HelmrClient({url:"https://api.example.test",apiKey:"api-key",fetch:(async()=>Response.json({...trigger,[field]:"invalid"})) as typeof fetch})
      await expect(client.triggers.get(id)).rejects.toThrow("ID")
    }
  })
})

describe("HelmrClient Sessions", () => {
  const session = {
    agent: "worker", template: "machine",
    id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
    deployment_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
    root_session_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
    initial_turn: null, activity: { process: null },
    parent_session_id: null,
    requester_session_id: null,
    holds: [],
    status: "open",
    created_at: "2026-07-24T11:50:00Z",
  }
  test("rejects a missing Computer field; empty Sessions require explicit null", async () => {
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async () => Response.json(session)) as typeof fetch,
    })

    await expect(client.sessions.get(
      "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
    )).rejects.toThrow("Session.computer_id")
  })

  test("lists Sessions by public status with bound pagination", async () => {
    const requests: string[] = []
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async (input: URL | RequestInfo) => {
        requests.push(String(input))
        return Response.json({ sessions: [], next_cursor: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc39" })
      }) as typeof fetch,
    })

    const page = await client.sessions.list({
      status: ["open", "closing"],
      cursor: "cursor-previous",
      limit: 5,
    })
    expect(page).toEqual({ items: [], nextCursor: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc39" })
    await client.sessions.list({ status: "closed" })
    expect(requests).toEqual([
      "https://api.example.test/v1/sessions?status=open&status=closing&cursor=cursor-previous&limit=5",
      "https://api.example.test/v1/sessions?status=closed",
    ])
    await expect(client.sessions.list({
      // @ts-expect-error unknown Session statuses are rejected.
      status: "unknown",
    })).rejects.toThrow("Session list status is invalid")
    await expect(client.sessions.list({
      agent: "worker",
      key: "thread:1",
      // @ts-expect-error the exact key lookup does not filter by status.
      status: "open",
    })).rejects.toThrow("does not accept status, cursor or limit")
    expect(requests).toHaveLength(2)
  })

  test("rejects unknown Session response statuses", async () => {
    const sessionId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33"
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async () => Response.json({
        ...session,
        computer_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
        status: "invalid-status",
      })) as typeof fetch,
    })

    await expect(client.sessions.get(sessionId)).rejects.toThrow(
      "Session.status is invalid",
    )
  })

})

test("unsupported list query keys fail before transport instead of widening a list", async () => {
  let calls = 0
  const client = new HelmrClient({ apiKey: "key", fetch: (async () => { calls++; throw new Error("unexpected request") }) as typeof fetch })
  await expect(client.sessions.list({ owner: "someone" } as never)).rejects.toThrow("Unsupported Session list query")
  await expect(client.triggers.list({ owner: "someone" } as never)).rejects.toThrow("Unsupported Trigger list query")
  expect(calls).toBe(0)
})
