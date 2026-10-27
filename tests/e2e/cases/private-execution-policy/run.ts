import { createHash } from "node:crypto"
import { readFile, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"
import { z } from "zod"
import { assert, assertEqual, completedResult, deadline, errorCode, verify, waitOutput, type ClientTurn } from "../../support/context"
import { capacityAwareStartAgent } from "../../support/capacity-admission"
import { fixtureInput } from "../../support/runtime-mcp"
import { configuration, finiteNetwork } from "./config"
import { verifyDeploymentPins } from "./deployment"
import { positiveTargets, requireConfigured, validatePositiveReceipt, type PositiveRequest } from "./positive-control"

assert(process.env.HELMR_PRIVATE_POLICY_PROBES === "1", "Set HELMR_PRIVATE_POLICY_PROBES=1 after establishing the configured network prerequisites")
assert(process.env.HELMR_EXPECTED_BUNDLE_DIGEST, "HELMR_EXPECTED_BUNDLE_DIGEST is required")
const initialHostId = z.uuid().parse(process.env.HELMR_PRIVATE_POLICY_WORKER_HOST_ID)
requireConfigured(configuration)
const subject = z.enum(["ordinary", "finite", "blocked"]).parse(process.env.HELMR_PRIVATE_POLICY_SUBJECT)
const lifecycle = z.enum(["hibernate", "replacement"]).parse(process.env.HELMR_PRIVATE_POLICY_LIFECYCLE ?? "hibernate")
const digest = createHash("sha256").update(JSON.stringify(configuration)).digest("hex")
const network = z.object({
  phase: z.literal("runtime"), subject: z.literal(subject), allowed: z.literal(subject !== "blocked"),
  finite: z.literal(subject === "finite"), configurationDigest: z.literal(digest),
  decisions: z.array(z.object({ label: z.string(), outcome: z.enum(["allowed", "denied", "resolved", "absent"]) }).passthrough()).min(1),
  preparation: z.object({ subject: z.literal(subject), allowed: z.literal(subject === "blocked"),
    configurationDigest: z.literal(digest), decisions: z.array(z.unknown()).min(1) }).optional(),
})
const report = z.object({ network, computerId: z.uuid(), nonce: z.uuid(), count: z.number().int() })

await verify(`private-policy-${subject}-${lifecycle}`, async ({ client, marker, computer, startAgent: startOnce, objects }) => {
  const record = (name: string, value: unknown) => writeFile(join(process.env.HELMR_EVIDENCE_DIR!, name), JSON.stringify(value, null, 2) + "\n", { flag: "wx", mode: 0o600 })
  const ref = await computer(`private-policy-${subject}`)
  const started = await capacityAwareStartAgent(startOnce)("private-policy-probe", {
    computer: ref, input: { subject }, idempotencyKey: `${marker}:first`,
  })
  const original = await ref.get()
  const sessionPin = await started.session.get()
  const deploymentPins = await verifyDeploymentPins(client.deployments, original.deploymentId, sessionPin.deploymentId, process.env.HELMR_EXPECTED_BUNDLE_DIGEST!)
  await record("deployment-pins.json", { marker, computerId: ref.id, sessionId: started.session.id, ...deploymentPins })
  async function positiveControl(phase: "first" | "continued", turn: ClientTurn, workerHostId: string) {
    await waitOutput(started.session, turn, value => value !== null && typeof value === "object" && !Array.isArray(value)
      && "phase" in value && "computerId" in value && "subject" in value && value.phase === "ready-for-positive-control" && value.computerId === ref.id && value.subject === subject, 300_000)
    const request: PositiveRequest = { marker, phase, computerId: ref.id, turnId: turn.id, workerHostId,
      configurationDigest: digest, requestedAt: new Date().toISOString(), targets: positiveTargets(configuration, subject), ipv6: "guest-disabled-topology" }
    await record(`positive-control-${phase}-request.json`, request)
    const signal = deadline(10 * 60_000)
    for (;;) {
      try {
        const receipt = validatePositiveReceipt(JSON.parse(await readFile(join(process.env.HELMR_EVIDENCE_DIR!, `positive-control-${phase}.json`), "utf8")), request)
        await turn.send(fixtureInput("probe"), { idempotencyKey: `${marker}:${phase}:permit-probe` })
        return receipt
      } catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error }
      await delay(500, undefined, { signal })
    }
  }
  const firstPositiveControl = await positiveControl("first", started.turn, initialHostId)
  const first = report.parse(await completedResult(started.turn, 300_000))
  assertEqual(first.computerId, ref.id, "Probe used another Computer")
  if (subject !== "ordinary") assert(first.network.preparation, "Missing the opposite preparation-policy evidence")
  const definition = await client.templates.get(original.template, { deploymentId: original.deploymentId })
  const normalized = (value: unknown) => JSON.parse(JSON.stringify(value), (key, item) => key === "allowDomains" && Array.isArray(item)
    ? item.sort((a, b) => (typeof a === "string" ? a : a.domain).localeCompare(typeof b === "string" ? b : b.domain)) : item)
  assertEqual(normalized(definition.network), normalized(subject === "ordinary" ? { blockAll: false } : subject === "finite" ? finiteNetwork : { blockAll: true }), "Published runtime policy changed")
  assertEqual(normalized(definition.prepareNetwork), normalized(subject === "blocked" ? finiteNetwork : { blockAll: true }), "Published preparation policy changed")
  await record("before.json", { marker, original, definition, first })
  async function residency(expected: "parked" | "running") {
    const signal = deadline(10 * 60_000)
    for (;;) {
      const state = await ref.get({ signal })
      assert(state.executionControl?.status !== "failed", `Execution control failed: ${JSON.stringify(state.executionControl)}`)
      if (state.residency === expected && state.executionControl?.status === "completed") return state
      await delay(500, undefined, { signal })
    }
  }
  // Services and provider operations remain operator-owned. The marker is a gate,
  // not proof of a physical checkpoint, source reclamation or replacement Host.
  await ref.hibernate({ idempotencyKey: `${marker}:hibernate` })
  const parked = await residency("parked")
  await record("parked.json", { marker, computerId: ref.id, sessionId: started.session.id, parked })
  let replacement: { replacementHostId: string } | undefined
  if (lifecycle === "replacement") {
    const signal = deadline(20 * 60_000)
    for (;;) {
      try {
        const acknowledgment = JSON.parse(await readFile(join(process.env.HELMR_EVIDENCE_DIR!, "worker-replaced.json"), "utf8"))
        const gate = z.object({ marker: z.literal(marker), computerId: z.literal(ref.id), sourceHostId: z.uuid(), replacementHostId: z.uuid(), evidencePath: z.string().min(1) }).strict().parse(acknowledgment)
        assertEqual(gate.sourceHostId, initialHostId, "Replacement marker identifies another source Host")
        replacement = gate
        assert.notEqual(gate.sourceHostId, gate.replacementHostId, "Replacement marker reused its source Host")
        break
      } catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error }
      await delay(500, undefined, { signal })
    }
  }
  const turns = (await started.session.turns.list()).items.map(turn => turn.id)
  const signal = deadline(180_000)
  for (;;) {
    try { await ref.wake({ idempotencyKey: `${marker}:wake` }, { signal }); break }
    catch (error) { signal.throwIfAborted(); if (errorCode(error) !== "execution_capacity_unavailable") throw error }
    await delay(500, undefined, { signal })
  }
  const awake = await residency("running")
  assertEqual((await started.session.turns.list()).items.map(turn => turn.id), turns, "Wake fabricated a Turn")
  for (const key of ["id", "template", "deploymentId"] as const) assertEqual(awake[key], original[key], `Wake changed Computer ${key}`)
  const second = await started.session.enqueue(fixtureInput({ subject }), { idempotencyKey: `${marker}:continued` })
  objects.turn_ids.push(second.id)
  const continuedPositiveControl = await positiveControl("continued", second, replacement?.replacementHostId ?? initialHostId)
  const continued = report.parse(await completedResult(second, 300_000))
  assertEqual(continued.nonce, first.nonce, "Wake reconstructed the Session init state")
  assertEqual([first.count, continued.count], [1, 2], "Session counter did not continue")
  assertEqual(continued.computerId, ref.id, "Continuation changed Computer")
  const decisions = (value: z.infer<typeof network>) => ({ ...value,
    decisions: value.decisions.map(({ label, outcome }) => ({ label, outcome })),
    preparation: value.preparation && { ...value.preparation, decisions: undefined },
  })
  assertEqual(decisions(continued.network), decisions(first.network), "Continuation changed network decisions or preparation evidence")
  return { subject, lifecycle, configurationDigest: digest, first, original, definition, parked, awake, continued,
    deploymentPins, firstPositiveControl, continuedPositiveControl, operatorReplacement: replacement ?? null, physicalLineageVerified: false, preparationEnforcementVerified: false }
})
