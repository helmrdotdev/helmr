import assert from "node:assert/strict"
import { writeFile } from "node:fs/promises"
import { setTimeout as delay } from "node:timers/promises"
import type { HelmrClient } from "../../../sdk/typescript/src/client"
import type { CommandRef } from "../../../sdk/typescript/src/command"

async function waitForReconciliation(command: CommandRef, parentSignal: AbortSignal) {
  // Logical command completion can precede physical process reconciliation.
  const signal = AbortSignal.any([parentSignal, AbortSignal.timeout(60_000)])
  try {
    while (!(await command.get({ signal })).processReconciled) await delay(250, undefined, { signal })
    return { processReconciled: true }
  } catch (error) {
    return { processReconciled: false, reconciliationError: error instanceof Error ? error.message : String(error) }
  }
}

// Runs the selected recovery policies over native SDK primitives in a real guest.
// These are SDK cases, not Helmr Turn-adapter acceptance assertions.
export async function runNativeSDK(client: HelmrClient, computerId: string, evidence: string, signal: AbortSignal) {
  // Keep startup observation serial and separate from the unchanged behavior
  // cases. The resulting case evidence is explicitly after package pre-reading.
  await writeFile(`${evidence}/native-sdk-startup.json`, JSON.stringify({ computerId, status: "starting" }), { mode: 0o600 })
  const startup = await client.computers.ref(computerId).exec({
    command: ["bash", "/workspace/native-sdk/startup.sh"],
    timeout: "9m",
    idempotencyKey: "native-sdk-startup-diagnostic",
  }, { signal }).catch(async error => {
    await writeFile(`${evidence}/native-sdk-startup.json`, JSON.stringify({ computerId, status: "exec-failed", error: error instanceof Error ? error.message : String(error) }), { mode: 0o600 })
    throw error
  })
  await writeFile(`${evidence}/native-sdk-startup.json`, JSON.stringify({ computerId, commandId: startup.id, status: "running" }), { mode: 0o600 })
  const startupOutcome = await startup.wait({ signal }).catch(async error => {
    await writeFile(`${evidence}/native-sdk-startup.json`, JSON.stringify({ computerId, commandId: startup.id, status: "wait-failed", error: error instanceof Error ? error.message : String(error) }), { mode: 0o600 })
    throw error
  })
  const startupOutput: Uint8Array[] = []
  let startupGap = false
  let startupLogError: unknown
  try {
    for await (const record of startup.logs.stream({}, { signal })) {
      if (record.kind === "gap") startupGap = true
      else startupOutput.push(record.content)
    }
  } catch (error) { startupLogError = error }
  await writeFile(`${evidence}/native-sdk-startup.log`, Buffer.concat(startupOutput), { mode: 0o600 })
  const startupReconciliation = await waitForReconciliation(startup, signal)
  await writeFile(`${evidence}/native-sdk-startup.json`, JSON.stringify({ computerId, commandId: startup.id, outcome: startupOutcome, logGap: startupGap, logError: startupLogError instanceof Error ? startupLogError.message : startupLogError, ...startupReconciliation }), { mode: 0o600 })
  if (startupLogError) throw startupLogError
  assert.equal(startupGap, false, "startup diagnostic evidence has a log gap")
  assert.equal(startupReconciliation.processReconciled, true, JSON.stringify(startupReconciliation))
  const startedAt = new Date().toISOString()
  await writeFile(`${evidence}/native-sdk.json`, JSON.stringify({ computerId, startedAt, status: "starting" }), { mode: 0o600 })
  const command = await client.computers.ref(computerId).exec({
    command: ["bash", "/workspace/native-sdk/run.sh"],
    timeout: "15m",
    idempotencyKey: "native-sdk-qualification",
  }, { signal }).catch(async error => {
    await writeFile(`${evidence}/native-sdk.json`, JSON.stringify({ computerId, startedAt, status: "exec-failed", error: error instanceof Error ? error.message : String(error) }), { mode: 0o600 })
    throw error
  })
  await writeFile(`${evidence}/native-sdk.json`, JSON.stringify({ computerId, commandId: command.id, startedAt, status: "running" }), { mode: 0o600 })
  let outcome
  try {
    outcome = await command.wait({ signal })
  } catch (error) {
    await writeFile(`${evidence}/native-sdk.json`, JSON.stringify({ computerId, commandId: command.id, startedAt, status: "wait-failed", error: error instanceof Error ? error.message : String(error) }), { mode: 0o600 })
    throw error
  }
  const output: Uint8Array[] = []
  let logGap = false
  let logError: unknown
  try {
    for await (const record of command.logs.stream({}, { signal })) {
      if (record.kind === "gap") logGap = true
      else output.push(record.content)
    }
  } catch (error) { logError = error }
  const text = Buffer.concat(output).toString("utf8")
  await writeFile(`${evidence}/native-sdk.log`, text, { mode: 0o600 })
  const passedCases = [...text.matchAll(/^PASS (\S+)$/gm)].map(match => match[1])
  const failedCases = [...text.matchAll(/^FAIL (\S+)$/gm)].map(match => match[1])
  const reconciliation = await waitForReconciliation(command, signal)
  const receipt = { computerId, commandId: command.id, startedAt, afterStartupDiagnostic: true, outcome, passedCases, failedCases, logGap, logError: logError instanceof Error ? logError.message : logError, ...reconciliation }
  await writeFile(`${evidence}/native-sdk.json`, JSON.stringify(receipt), { mode: 0o600 })
  if (logError) throw logError
  assert.equal(logGap, false, "SDK evidence has a log gap")
  assert.equal(outcome.kind, "exited", JSON.stringify(outcome))
  assert.equal(outcome.exitCode, 0, "native SDK guest case failed; see native-sdk.log")
  assert.equal(new Set(passedCases).size, 14)
  assert.equal(failedCases.length, 0)
  assert.equal(reconciliation.processReconciled, true, JSON.stringify(reconciliation))
  return receipt
}
