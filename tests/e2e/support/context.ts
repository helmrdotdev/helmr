import assert from "node:assert/strict"
import { randomUUID } from "node:crypto"
import { mkdir } from "node:fs/promises"
import { writeFileSync } from "node:fs"
import { join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"
import { HelmrClient, type Run, type ComputerRef } from "@helmr/sdk"
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
export async function waitRun(
  client: HelmrClient,
  id: string,
  accepted: readonly Run["status"][],
  timeoutMs = 20 * 60_000,
): Promise<Run> {
  const signal = deadline(timeoutMs)
  for (;;) {
    signal.throwIfAborted()
    const run = await client.runs.retrieve(id, { signal })
    if (accepted.includes(run.status)) return run
    assert(
      !["succeeded", "failed", "cancelled", "expired", "system_failed"].includes(run.status),
      `Unexpected terminal Run ${id}: ${run.status}`,
    )
    await new Promise((resolve) => setTimeout(resolve, 500))
  }
}
export async function readTelemetry<T>(read: () => Promise<T>): Promise<T> {
  const signal = deadline(5 * 60_000)
  for (;;) {
    signal.throwIfAborted()
    try {
      return await read()
    } catch (error) {
      if (!["telemetry_lagging", "telemetry_unavailable"].includes(errorCode(error) ?? ""))
        throw error
      await new Promise((resolve) => setTimeout(resolve, 500))
    }
  }
}
export async function deleteComputer(ref: ComputerRef, idempotencyKey: string) {
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
      "run_ids" | "computer_ids" | "session_ids" | "token_ids" | "secret_ids" | "deployment_ids" | "schedule_ids",
      string[]
    >
    cleanup: (action: () => Promise<unknown>) => void
    computer: (sandbox: string, suffix?: string) => Promise<ComputerRef>
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
    run_ids: [] as string[],
    computer_ids: [] as string[],
    session_ids: [] as string[],
    token_ids: [] as string[],
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
      computer: async (sandbox, suffix = sandbox) => {
        const ref = await client.sandboxes.createComputer(
          sandbox,
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
  if (failure !== undefined) {
    for (const id of objects.run_ids) {
      try {
        const run = await client.runs.retrieve(id, { signal: deadline(30_000) })
        if (
          !["succeeded", "failed", "system_failed", "cancelled", "expired"].includes(run.status)
        ) {
          await client.runs.cancel(id, {}, { signal: deadline(30_000) })
          await waitRun(
            client,
            id,
            ["succeeded", "failed", "system_failed", "cancelled", "expired"],
            120_000,
          )
        }
      } catch (error) {
        failures.push(`Run ${id}: ${error instanceof Error ? error.message : String(error)}`)
      }
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
