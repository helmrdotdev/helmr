import { fixtureInput } from "../../support/runtime-mcp"
import { writeFile } from "node:fs/promises"
import { join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"
import { verify, assert, assertEqual, deadline, completedResult } from "../../support/context"

// Deploy the original fixture first. While this driver waits, promote a bundle
// changing only runtimeSmoke's report to include programRevision: "next".
await verify("program-replacement", async ({ client, marker, objects, computer, startAgent }) => {
  const nextDigest = process.env.HELMR_NEXT_BUNDLE_DIGEST
  assert(nextDigest?.startsWith("sha256:"), "HELMR_NEXT_BUNDLE_DIGEST is required")
  const previous = await client.deployments.current()
  assert(previous && previous.bundleDigest !== nextDigest, "Start with the original fixture deployment")
  const shared = await computer("helmr-runtime-smoke", "program-replacement")
  const input = (suffix: string, expectedComputerMarker?: string) => ({
    marker: `${marker}:${suffix}`, expectedEnvironment: "unknown", largeFileKiB: 256,
    ...(expectedComputerMarker === undefined ? {} : { expectedComputerMarker }),
  })
  const first = await startAgent("runtime-smoke", {
    computer: shared, input: input("first"), idempotencyKey: `${marker}:first`,
  })
  const firstOutput = await completedResult(first.turn)
  assert(firstOutput !== null && typeof firstOutput === "object" && !Array.isArray(firstOutput))
  assert("ok" in firstOutput)
  assertEqual(firstOutput.ok, true, "Initial filesystem checks failed")
  assertEqual((await first.session.retrieve()).deploymentId, previous.id, "Initial Session used a different deployment")
  assert(!("programRevision" in firstOutput), "Initial Turn already used replacement code")
  const transition = { computerId: shared.id, firstSessionId: first.session.id, firstTurnId: first.turn.id, firstOutput, nextDigest }
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
  const followup = await first.session.enqueue(fixtureInput(input("followup", `${marker}:first`)), { idempotencyKey: `${marker}:followup` })
  objects.turn_ids.push(followup.id)
  const followupOutput = await completedResult(followup)
  assert(followupOutput !== null && typeof followupOutput === "object" && !Array.isArray(followupOutput))
  assert("ok" in followupOutput)
  assertEqual(followupOutput.ok, true, "Pinned Session lost Computer writes")
  assert(!("programRevision" in followupOutput), "Existing Session changed its pinned code")
  assertEqual((await first.session.retrieve()).deploymentId, previous.id, "Existing Session changed its deployment")
  const second = await startAgent("runtime-smoke", {
    computer: shared, input: input("second", `${marker}:followup`), idempotencyKey: `${marker}:second`,
  })
  const secondOutput = await completedResult(second.turn)
  assert(secondOutput !== null && typeof secondOutput === "object" && !Array.isArray(secondOutput))
  assert("ok" in secondOutput)
  assertEqual(secondOutput.ok, true, "New Session lost Computer writes")
  assertEqual((await second.session.retrieve()).deploymentId, next.id, "New Session did not use the promoted deployment")
  assert("programRevision" in secondOutput)
  assertEqual(secondOutput.programRevision, "next", "New Session executed old code")
  objects.deployment_ids.push(previous.id, next.id)
  return { ...transition, followupTurnId: followup.id, followupOutput, secondSessionId: second.session.id, secondTurnId: second.turn.id, secondOutput }
})
