import type { ComputerRef } from "@helmr/sdk"
import { verify, assert, assertEqual, deadline, deleteComputer } from "../../support/context"
import { hostObservation } from "../../support/host-observation"

// Requires the runtime fixture and two VM slots on the dedicated host. A fresh
// seed is needed to measure conversion; an existing seed is recorded as adoption.
await verify("computer-seed-reuse", async ({ client, marker, computer }) => {
  const samples: unknown[] = []
  async function command(target: ComputerRef, suffix: string, script: string, value: string) {
    const requestAt = performance.now()
    const ref = await client.computers.ref(target.id).exec({ command: ["sh", "-ceu", script], env: { VALUE: value },
      idempotencyKey: `${marker}:${suffix}`, timeout: "2m" }, { signal: deadline(900_000) })
    const acceptedAt = performance.now()
    const outcome = await ref.wait({ signal: deadline(900_000) })
    assert.equal(outcome.kind, "exited", "command did not finish normally")
    assert.equal(outcome.exitCode, 0, "private filesystem assertion failed")
    return { commandId: ref.id, acceptanceMs: acceptedAt - requestAt,
      completionMs: performance.now() - requestAt }
  }
  const createFirstAt = performance.now()
  const first = await computer("helmr-runtime-smoke", "seed-first")
  const firstAcceptedMs = performance.now() - createFirstAt
  const firstCommand = await command(first, "first-write",
    'test ! -e /workspace/seed-owner; printf %s "$VALUE" > /workspace/seed-owner', `${marker}:first`)
  const firstWorkMs = performance.now() - createFirstAt
  const firstPath = await hostObservation("computer-path", { computer_id: first.id })
  samples.push({ computerId: first.id, createAcceptanceMs: firstAcceptedMs,
    createToFirstWorkMs: firstWorkMs, ...firstCommand, path: firstPath })

  const createSecondAt = performance.now()
  const second = await computer("helmr-runtime-smoke", "seed-second")
  const secondAcceptedMs = performance.now() - createSecondAt
  const secondCommand = await command(second, "second-write",
    'test ! -e /workspace/seed-owner; printf %s "$VALUE" > /workspace/seed-owner', `${marker}:second`)
  const secondWorkMs = performance.now() - createSecondAt
  const secondPath = await hostObservation("computer-path", { computer_id: second.id })
  samples.push({ computerId: second.id, createAcceptanceMs: secondAcceptedMs,
    createToFirstWorkMs: secondWorkMs, ...secondCommand, path: secondPath })
  // Immutable receipts survive periodic saves and initial-payload retirement.
  // Equal canonical fingerprints bind the exact same root and launch config.
  const firstInitial = firstPath.instances.filter((instance: any) => instance.initial_disk_version_id !== null)
  const secondInitial = secondPath.instances.filter((instance: any) => instance.initial_disk_version_id !== null)
  assert.equal(firstInitial.length, 1, "first initial receipt missing")
  assert.equal(secondInitial.length, 1, "second initial receipt missing")
  assert(/^[0-9a-f]{64}$/.test(firstInitial[0].initial_publication_fingerprint), "invalid initial fingerprint")
  assertEqual(firstInitial[0].initial_publication_fingerprint, secondInitial[0].initial_publication_fingerprint,
    "same seed was converted into separate roots")
  assert(firstInitial[0].initial_disk_version_id !== secondInitial[0].initial_disk_version_id,
    "Computers shared mutable version history")
  assert(secondPath.instances.every((instance: any) => instance.seed_id === null),
    "second Computer started another conversion")
  await command(first, "first-read", 'test "$(cat /workspace/seed-owner)" = "$VALUE"', `${marker}:first`)
  await deleteComputer(first, `delete:seed-first:${marker}`)
  await command(second, "surviving-read", 'test "$(cat /workspace/seed-owner)" = "$VALUE"', `${marker}:second`)
  return { samples, seedReused: true, independentWrites: true, sourceDeletionPreservedSibling: true,
    timingScope: "API acceptance and command completion; host seed phases are in the Worker journal" }
})
