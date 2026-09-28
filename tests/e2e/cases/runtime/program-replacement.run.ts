import { writeFile } from "node:fs/promises"
import { join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"
import { verify, assert, assertEqual, deadline } from "../../support/context"
import type { runtimeSmoke } from "./task"

// Deploy the original fixture first. While this driver waits, promote a bundle
// changing only runtimeSmoke's report to include programRevision: "next".
await verify("program-replacement", async ({ client, marker, objects, computer }) => {
  const nextDigest = process.env.HELMR_NEXT_BUNDLE_DIGEST
  assert(nextDigest?.startsWith("sha256:"), "HELMR_NEXT_BUNDLE_DIGEST is required")
  const previous = await client.deployments.current()
  assert(previous && previous.bundleDigest !== nextDigest, "Start with the original fixture deployment")
  const shared = await computer("helmr-runtime-smoke", "program-replacement")
  const start = async (suffix: string, expectedComputerMarker?: string) => {
    const run = await client.tasks.start<typeof runtimeSmoke>("runtime-smoke", {
      computer: shared,
      payload: { marker: `${marker}:${suffix}`, expectedComputerMarker, expectedEnvironment: "unknown", largeFileKiB: 256 },
      idempotencyKey: `${marker}:${suffix}`,
    })
    objects.run_ids.push(run.id)
    const output = await client.runs.wait(run, { signal: deadline(10 * 60_000) }).unwrap()
    const record = await client.runs.retrieve(run.id)
    assertEqual(record.computerId, shared.id, "Run moved to a different Computer")
    assert(output !== null && typeof output === "object" && !Array.isArray(output), "Program result must be an object")
    const report = output as Record<string, unknown>
    assertEqual(report.ok, true, "Program failed its filesystem checks")
    return { runId: run.id, deploymentId: record.deployment.id, output: report }
  }
  const first = await start("first")
  assertEqual(first.deploymentId, previous.id, "First Run used a different deployment")
  assert(!("programRevision" in first.output), "First Run already used the replacement code")
  const transition = { computerId: shared.id, first, nextDigest }
  await writeFile(join(process.env.HELMR_EVIDENCE_DIR!, "ready-for-deploy.json"), JSON.stringify(transition, null, 2) + "\n", { mode: 0o600 })
  console.log("Prepared Computer is ready for the next fixture deployment")
  const signal = deadline(10 * 60_000)
  let next
  for (;;) {
    signal.throwIfAborted()
    next = await client.deployments.current({ signal })
    if (next?.bundleDigest === nextDigest) break
    await delay(500, undefined, { signal })
  }
  assert(next, "Replacement deployment is missing")
  const second = await start("second", `${marker}:first`)
  assertEqual(second.deploymentId, next.id, "Second Run did not use the promoted deployment")
  assert("programRevision" in second.output, "Second Run executed the old Program")
  assertEqual(second.output.programRevision, "next", "Replacement Program output mismatch")
  objects.deployment_ids.push(previous.id, next.id)
  return { ...transition, second }
})
