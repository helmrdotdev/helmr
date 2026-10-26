import { setTimeout as delay } from "node:timers/promises"
import { z } from "zod"
import { assert, assertEqual, completedResult, deadline, deleteComputer, verify, waitOutput, type ClientTurn } from "../../support/context"
import { fixtureInput } from "../../support/runtime-mcp"

assert(process.env.HELMR_EXECUTION_POLICY_PUBLIC_PROBES === "1", "Set HELMR_EXECUTION_POLICY_PUBLIC_PROBES=1 to opt in to checkip.amazonaws.com, example.com and 1.1.1.1 probes")
const networkResult = z.object({
  expected: z.enum(["ordinary", "finite", "blocked"]), destinationIPv4: z.ipv4(), preparation: z.string().nullable(),
  ordinaryPrerequisites: z.boolean(), deniedHost: z.boolean(), dnsBypassDenied: z.boolean(),
  directIPBypassDenied: z.boolean(), httpBypassDenied: z.boolean(),
}).passthrough()
const restrictedResult = networkResult.extend({ ordinaryPrerequisites: z.literal(false), deniedHost: z.literal(true), dnsBypassDenied: z.literal(true), directIPBypassDenied: z.literal(true), httpBypassDenied: z.literal(true) })
const admission = z.object({ sessionId: z.uuid(), turnId: z.uuid(), sequence: z.number().int() })
const coordinatorResult = z.object({
  network: restrictedResult.extend({ expected: z.literal("finite"), preparation: z.literal("prepare-blocked") }), nonce: z.uuid(), count: z.number().int(), child: admission,
  childResult: restrictedResult.extend({ expected: z.literal("finite"), preparation: z.literal("prepare-blocked"), computerId: z.uuid(), marker: z.string(), emptyMaximum: z.literal(true) }),
  inheritedFiniteMaximum: z.literal(true), allowedScopedRetry: z.literal(true),
  deniedDirectAliasDiscovery: z.literal(true), deniedCurrentReceipt: z.literal(true), emptyScope: z.literal(true),
})

await verify("execution-policies", async ({ client, marker, objects, computer, startAgent }) => {
  const normal = await computer("execution-policy-ordinary", "ordinary")
  const ordinary = await startAgent("execution-policy-ordinary-probe", {
    computer: normal, input: { marker, expected: "ordinary" }, idempotencyKey: `${marker}:ordinary`,
  })
  const baseline = networkResult.parse(await completedResult(ordinary.turn, 180_000))
  assertEqual(baseline.expected, "ordinary", "Ordinary connectivity baseline did not execute")
  assertEqual(baseline["ordinaryPrerequisites"], true, "The external endpoints were not positively established")
  const ordinaryPin = await normal.get()
  const ordinaryDefinition = await client.templates.get(ordinaryPin.template, { deploymentId: ordinaryPin.deploymentId })
  assertEqual(ordinaryDefinition.network, { blockAll: false }, "Omitted network did not project ordinary authority")
  assertEqual(ordinaryDefinition.prepareNetwork, { blockAll: true }, "Seed-only preparation did not project blocking")
  // Release the baseline VM before the shared-Computer continuation portion.
  await ordinary.session.cancel({ idempotencyKey: `${marker}:ordinary-finished` })
  await deleteComputer(normal, `${marker}:ordinary-delete`)

  const finite = await computer("execution-policy-finite", "finite")
  const input = { marker, expected: "finite", destinationIPv4: baseline.destinationIPv4 }
  const started = await startAgent("execution-policy-coordinator", {
    computer: finite, input, idempotencyKey: `${marker}:finite`,
  })
  const observe = async (turn: ClientTurn) => {
    const event = await waitOutput(started.session, turn, value => value !== null && typeof value === "object" && !Array.isArray(value) && "phase" in value && value.phase === "child-admitted", 240_000)
    const child = admission.parse(event)
    objects.session_ids.push(child.sessionId)
    objects.turn_ids.push(child.turnId)
    const result = coordinatorResult.parse(await completedResult(turn, 300_000))
    assertEqual(result.child, child, "Observed child receipt changed")
    assertEqual(result.network.preparation, "prepare-blocked", "Finite runtime lost the blocked preparation marker")
    assertEqual(result.childResult.computerId, finite.id, "Managed reuse selected a different Computer")
    assertEqual(result.childResult.marker, marker, "Child input changed")
    assertEqual(result.childResult.preparation, "prepare-blocked", "Child's ordinary Template replaced its actual Computer policy")
    return result
  }
  const first = await observe(started.turn)
  const finitePin = await finite.get()
  const finiteDefinition = await client.templates.get(finitePin.template, { deploymentId: finitePin.deploymentId })
  assertEqual(finiteDefinition.network, { allowDomains: ["checkip.amazonaws.com"] }, "Finite network readback changed")
  assertEqual(finiteDefinition.prepareNetwork, { blockAll: true }, "Resolved finite preparation policy changed")
  const maximum = await client.agents.get("execution-policy-coordinator", { deploymentId: (await started.session.get()).deploymentId })
  assertEqual(maximum.runtimeMcp, { allow: [
    { operation: "inspect_turn", agents: ["execution-policy-target"] },
    { operation: "spawn", agents: ["execution-policy-other-target", "execution-policy-target"], computer: "caller" },
  ] }, "Finite Agent maximum readback changed")
  const childPin = await client.sessions.get(first.child.sessionId)
  assertEqual(childPin.template, "execution-policy-ordinary", "Fixture target did not retain its different default Template")
  assertEqual(childPin.computerId, finite.id, "Child did not retain the caller Computer")

  const waitResidency = async (residency: "parked" | "running") => {
    const signal = deadline(180_000)
    for (;;) {
      const state = await finite.get({ signal })
      assert(state.executionControl?.status !== "failed", `Execution control failed: ${JSON.stringify(state.executionControl)}`)
      if (state.residency === residency && state.executionControl?.status === "completed") return state
      await delay(250, undefined, { signal })
    }
  }
  const sleep = await finite.hibernate({ idempotencyKey: `${marker}:sleep` })
  const sleeping = await waitResidency("parked")
  const beforeWake = (await started.session.turns.list()).items.map(turn => turn.id)
  const wake = await finite.wake({ idempotencyKey: `${marker}:wake` })
  const awake = await waitResidency("running")
  assertEqual((await started.session.turns.list()).items.map(turn => turn.id), beforeWake, "Wake fabricated a Turn")
  const second = await started.session.enqueue(fixtureInput(input), { idempotencyKey: `${marker}:after-restore` })
  objects.turn_ids.push(second.id)
  const restored = await observe(second)
  assertEqual(restored.nonce, first.nonce, "Restore reconstructed the coordinator instead of continuing it")
  assertEqual([first.count, restored.count], [1, 2], "Session setup state did not survive restore")
  assertEqual(restored.child, first.child, "Restored descriptor retry replaced already admitted child work")

  const blocked = await computer("execution-policy-blocked", "blocked")
  const reused = await startAgent("execution-policy-ordinary-probe", {
    computer: blocked, input: { marker, expected: "blocked", destinationIPv4: baseline.destinationIPv4 }, idempotencyKey: `${marker}:blocked-reuse`,
  })
  const blockedResult = restrictedResult.extend({ expected: z.literal("blocked"), preparation: z.literal("prepare-allowed") }).parse(await completedResult(reused.turn, 180_000))
  assertEqual(blockedResult.preparation, "prepare-allowed", "Preparation could not use its distinct allowed policy")
  const blockedPin = await blocked.get()
  const reusedPin = await reused.session.get()
  assertEqual(reusedPin.template, "execution-policy-ordinary", "Fixture Agent did not retain its ordinary default")
  assertEqual(reusedPin.computerId, blocked.id, "Explicit reuse did not retain the requested Computer")
  const blockedDefinition = await client.templates.get(blockedPin.template, { deploymentId: blockedPin.deploymentId })
  assertEqual(blockedDefinition.network, { blockAll: true }, "Blocked Computer pin was substituted")
  assertEqual(blockedDefinition.prepareNetwork, { allowDomains: ["checkip.amazonaws.com"] }, "Blocked runtime lost its distinct preparation allowance")
  assertEqual((await client.agents.get("execution-policy-ordinary-probe", { deploymentId: reusedPin.deploymentId })).runtimeMcp, undefined, "Authored MCP omission was not retained in readback")
  return { baseline, ordinaryPin, finitePin, first, sleep, sleeping, wake, awake, restored, blockedPin, reusedPin, blockedResult }
})
