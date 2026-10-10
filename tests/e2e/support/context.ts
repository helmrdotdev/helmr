import { fixtureInput } from "./runtime-mcp"
import assert from "node:assert/strict"
import { randomUUID } from "node:crypto"
import { mkdir } from "node:fs/promises"
import { writeFileSync } from "node:fs"
import { join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"
import { HelmrClient, type AgentStartRequest, type ClientComputerRef, type ClientSessionRef, type TurnState, type AskState, type Json } from "@helmr/sdk"
import { deadline } from "./deadline"

export { deadline }
export const assertEqual = (actual: unknown, expected: unknown, message: string) =>
  assert.deepEqual(actual, expected, message)
export { assert }
export function errorCode(error: unknown): string | undefined {
  return error !== null &&
    typeof error === "object" &&
    "code" in error &&
    typeof error.code === "string"
    ? error.code
    : undefined
}
export type ClientTurn = ReturnType<ClientSessionRef["turn"]>
export const terminalTurnStatuses = ["completed", "failed", "interrupted", "cancelled"] as const
export async function waitTurn(
  turn: ClientTurn,
  accepted: readonly TurnState["status"][],
  timeoutMs = 20 * 60_000,
): Promise<TurnState> {
  const signal = deadline(timeoutMs)
  for (;;) {
    const state = await turn.retrieve({ signal })
    if (accepted.includes(state.status)) return state
    assert(
      !(terminalTurnStatuses as readonly string[]).includes(state.status),
      `Unexpected terminal Turn ${turn.id}: ${state.status} (${JSON.stringify(state.error)})`,
    )
    await delay(500, undefined, { signal })
  }
}
export async function waitAsk(
  turn: ClientTurn,
  match: (ask: AskState) => boolean = () => true,
  timeoutMs = 5 * 60_000,
): Promise<AskState> {
  const signal = deadline(timeoutMs)
  for (;;) {
    let cursor: string | undefined
    do {
      const page = await turn.asks.list({ cursor, limit: 100 }, { signal })
      const ask = page.asks.find(item => item.status === "pending" && match(item))
      if (ask) return ask
      cursor = page.nextCursor
    } while (cursor !== undefined)
    const state = await turn.retrieve({ signal })
    assert(!(terminalTurnStatuses as readonly string[]).includes(state.status),
      `Turn ${turn.id} ended before the requested question: ${state.status}`)
    await delay(500, undefined, { signal })
  }
}
export async function completedResult(turn: ClientTurn, timeoutMs = 20 * 60_000) {
  const outcome = await turn.wait({ signal: deadline(timeoutMs) })
  assert.equal(outcome.status, "completed", `Turn ${turn.id}: ${JSON.stringify(outcome)}`)
  assert.notEqual(outcome.result, undefined, `Turn ${turn.id}: result expired or absent`)
  return outcome.result!
}
export async function waitOutput(
  session: ClientSessionRef,
  turn: ClientTurn,
  match: (value: Json) => boolean,
  timeoutMs = 5 * 60_000,
): Promise<Json> {
  const signal = deadline(timeoutMs)
  let after = 0
  for (;;) {
    const page = await session.events.list({ after, limit: 100 }, { signal })
    assert(page.retainedAfter <= after, "Required Session output expired during verification")
    for (const event of page.records) {
      if (event.turnId !== turn.id) continue
      assert(!["turn.completed", "turn.failed", "turn.interrupted", "turn.cancelled"].includes(event.kind),
        `Turn ${turn.id} ended before the requested output: ${event.kind}`)
      if (event.kind !== "turn.output") continue
      const data = event.data
      if (!Array.isArray(data)) continue
      for (const part of data) {
        if (part && typeof part === "object" && part.type === "json" && match(part.value)) return part.value
      }
    }
    after = page.nextAfter
    if (page.hasMore) continue
    await delay(500, undefined, { signal })
  }
}
export async function deleteComputer(ref: ClientComputerRef, idempotencyKey: string) {
  const signal = deadline(120_000)
  // Terminal outcomes can precede physical process reconciliation.
  // Retry only that conflict, preserving the delete request's identity.
  try {
    for (;;) {
      signal.throwIfAborted()
      try {
        return await ref.delete({ idempotencyKey }, { signal })
      } catch (error) {
        if (errorCode(error) !== "computer_busy") throw error
      }
      await delay(500, undefined, { signal })
    }
  } catch (error) {
    if (signal.aborted) {
      throw new Error(`Computer ${ref.id}: deletion did not complete within 120 seconds`, {
        cause: error,
      })
    }
    throw error
  }
}
export async function verify(
  name: string,
  body: (context: {
    client: HelmrClient
    marker: string
    objects: Record<
      "turn_ids" | "computer_ids" | "session_ids" | "ask_ids" | "secret_ids" | "deployment_ids" | "schedule_ids",
      string[]
    >
    cleanup: (action: () => Promise<unknown>) => void
    computer: (definition: string, suffix?: string) => Promise<ClientComputerRef>
    startAgent: (definition: string, request: Omit<AgentStartRequest, "input"> & { input: Json }) => ReturnType<HelmrClient["agents"]["start"]>
  }) => Promise<unknown>,
) {
  const url = process.env.HELMR_API_URL,
    apiKey = process.env.HELMR_API_KEY,
    output = process.env.HELMR_EVIDENCE_DIR
  assert(
    url && apiKey && output,
    "HELMR_API_URL, HELMR_API_KEY and HELMR_EVIDENCE_DIR are required",
  )
  await mkdir(output, { recursive: false, mode: 0o700 })
  const client = new HelmrClient({
      url,
      apiKey,
      fetch: ((input, init) =>
        fetch(input, { ...init, signal: init?.signal ?? deadline(30_000) })) as typeof fetch,
    }),
    marker = randomUUID()
  const objects = {
    turn_ids: [] as string[],
    computer_ids: [] as string[],
    session_ids: [] as string[],
    ask_ids: [] as string[],
    secret_ids: [] as string[],
    deployment_ids: [] as string[],
    schedule_ids: [] as string[],
  }
  const cleanup: (() => Promise<unknown>)[] = []
  const evidence: Record<string, unknown> = {
    case: name,
    marker,
    startedAt: new Date().toISOString(),
    passed: false,
    objects,
  }
  // The invoking process owns recovery after interruption. Persist known IDs
  // synchronously before exiting so it can finish cleanup without racing a body
  // that may still be blocked in an API request.
  const writeEvidence = () => writeFileSync(
    join(output, "result.json"), JSON.stringify(evidence, null, 2) + "\n", { mode: 0o600 },
  )
  const interrupted = (signal: "SIGINT" | "SIGTERM", status: number) => {
    evidence.passed = false
    evidence.failure = `Interrupted by ${signal}`
    evidence.cleanup = { status: "interrupted" }
    evidence.finishedAt = new Date().toISOString()
    try { writeEvidence() } finally { process.exit(status) }
  }
  const onInterrupt = () => interrupted("SIGINT", 130)
  const onTerminate = () => interrupted("SIGTERM", 143)
  process.once("SIGINT", onInterrupt)
  process.once("SIGTERM", onTerminate)
  let failure: unknown
  try {
    if (process.env.HELMR_EXPECTED_BUNDLE_DIGEST) {
      const current = await client.deployments.current({ signal: deadline(30_000) })
      assert.equal(
        current?.bundleDigest,
        process.env.HELMR_EXPECTED_BUNDLE_DIGEST,
        "API key does not select the expected deployment",
      )
    }
    evidence.observations = await body({
      client,
      marker,
      objects,
      cleanup: (action) => cleanup.push(action),
      startAgent: async (definition, request) => {
        const admission = await client.agents.start(definition, { ...request, input: fixtureInput(request.input) }, { signal: deadline(30_000) })
        objects.session_ids.push(admission.session.id)
        objects.turn_ids.push(admission.turn.id)
        return admission
      },
      computer: async (definition, suffix = definition) => {
        const ref = await client.computerDefinitions.createComputer(
          definition,
          { key: `${suffix}-${marker}`, idempotencyKey: `create:${suffix}:${marker}` },
          { signal: deadline(30_000) },
        )
        objects.computer_ids.push(ref.id)
        cleanup.push(async () => {
          try {
            await deleteComputer(ref, `delete:${suffix}:${marker}`)
          } catch (error) {
            if (errorCode(error) !== "computer_not_found") throw error
          }
        })
        return ref
      },
    })
  } catch (error) {
    failure = error
    evidence.failure = error instanceof Error ? error.message : String(error)
  }
  const failures: string[] = []
  // A completed Turn leaves an open Session. Stop every owned Session on both
  // success and failure before attempting Computer deletion; its physical owner
  // may still need to reconcile, which deleteComputer observes separately.
  for (const id of new Set(objects.session_ids)) {
    try {
      await client.sessions.get(id).cancel(
        { idempotencyKey: `cleanup:${marker}:${id}` }, { signal: deadline(30_000) },
      )
    } catch (error) {
      failures.push(`Session ${id}: ${error instanceof Error ? error.message : String(error)}`)
      failure ??= error
    }
  }
  for (const action of cleanup.reverse()) {
    try {
      await action()
    } catch (error) {
      failures.push(error instanceof Error ? error.message : String(error))
      failure ??= error
    }
  }
  evidence.cleanup = failures.length
    ? { status: "failed", failures }
    : { status: "requests-accepted" }
  evidence.passed = failure === undefined
  evidence.finishedAt = new Date().toISOString()
  try { writeEvidence() } finally {
    process.removeListener("SIGINT", onInterrupt)
    process.removeListener("SIGTERM", onTerminate)
  }
  if (failure) throw failure
  console.log(`${name}: passed (${output}/result.json)`)
}
