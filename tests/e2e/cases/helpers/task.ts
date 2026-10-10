import { callMcp, fixtureInput, fixtureValue, waitManagedTurn, type ManagedAdmission } from "../../support/runtime-mcp"
import { agent, computer, image, type Json } from "@helmr/sdk"
import { readFile, writeFile } from "node:fs/promises"
import { setTimeout as sleep } from "node:timers/promises"
import { z } from "zod"

const base = image("helmr-helper-smoke").from("node:24-bookworm-slim").workdir("/workspace")
export const helperCallerComputer = computer({
  id: "helmr-helper-caller-smoke", image: base, resources: { cpu: 1, memory: "1GiB" },
})
export const helperTargetComputer = computer({
  id: "helmr-helper-target-smoke", image: base, resources: { cpu: 1, memory: "1GiB" },
})
const helperInput = z.object({
  marker: z.string().min(1), fail: z.boolean().default(false), waitForAnswer: z.boolean().default(false),
  holdSeconds: z.number().int().min(0).max(240).default(0),
}).strict()
export const helperChild = agent({
  id: "helper-smoke-child", computer: helperTargetComputer, maxTurnDuration: "5m",
  turn: async (turn, ctx) => {
    const input = helperInput.parse(fixtureValue(turn.input))
    await writeFile("helper-smoke.json", JSON.stringify({ marker: input.marker, turnId: turn.id, sessionId: ctx.session.id }))
    await turn.output.write([{ type: "json", value: { phase: "helper-started", marker: input.marker } }])
    if (input.waitForAnswer) await turn.ask({ prompt: [{ type: "text", text: "Finish independent helper" }], answer: { type: "text" } })
    if (input.holdSeconds > 0) await sleep(input.holdSeconds * 1000, undefined, { signal: turn.signal })
    if (input.fail) throw new Error(`intentional helper failure for ${input.marker}`)
    return { marker: input.marker, childTurnId: turn.id, childSessionId: ctx.session.id }
  },
})
const callerInput = z.object({
  mode: z.enum(["spawn-success", "spawn-failure", "same-computer", "start-independent"]),
  marker: z.string().min(1), childComputerId: z.string().min(1),
  holdSeconds: z.number().int().min(0).max(240).default(0),
}).strict()
export const helperCaller = agent({
  id: "helper-smoke", computer: helperCallerComputer, maxTurnDuration: "10m",
  turn: async (turn, ctx): Promise<Json> => {
    const input = callerInput.parse(fixtureValue(turn.input))
    const options = {
      agentId: helperChild.id, computerId: input.mode === "same-computer" ? ctx.computer.id : input.childComputerId,
      input: fixtureInput({ marker: input.marker, fail: input.mode === "spawn-failure", waitForAnswer: input.mode === "start-independent", holdSeconds: input.holdSeconds }),
      idempotencyKey: `${turn.id}:${input.mode}`,
    }
    const child = await callMcp<ManagedAdmission>(turn.signal, input.mode === "start-independent" ? "start" : "spawn", options)
    const identity = { mode: input.mode, marker: input.marker, childTurnId: child.turnId, childSessionId: child.sessionId }
    if (input.mode === "start-independent") return identity
    const outcome = await waitManagedTurn(turn.signal, child.sessionId, child.turnId)
    if (input.mode === "spawn-failure") {
      if (outcome.status !== "failed" || outcome.error?.code !== "handler_failed") throw new Error(`Unexpected helper failure: ${JSON.stringify(outcome)}`)
      return { ...identity, childFailure: outcome.error }
    }
    if (outcome.status !== "completed" || (outcome.result as {marker?:string} | undefined)?.marker !== input.marker || (outcome.result as {childTurnId:string}).childTurnId !== child.turnId) {
      throw new Error(`Unexpected helper outcome: ${JSON.stringify(outcome)}`)
    }
    let sameComputerMarkerObserved = false
    if (input.mode === "same-computer") {
      const written = JSON.parse(await readFile("helper-smoke.json", "utf8"))
      if (written.marker !== input.marker || written.turnId !== child.turnId) throw new Error("Parent did not observe helper Computer writes")
      sameComputerMarkerObserved = true
    }
    return { ...identity, sameComputerMarkerObserved }
  },
})

const continuityInput = z.object({ marker: z.string().min(1), childComputerId: z.string().min(1) }).strict()
export const helperContinuity = agent({
  id: "helper-continuity", computer: helperCallerComputer, maxTurnDuration: "10m",
  setup: () => ({ count: 0 }),
  turn: async (turn, ctx) => {
    const input = continuityInput.parse(fixtureValue(turn.input))
    const child = await callMcp<ManagedAdmission>(turn.signal, "spawn", {
      agentId: helperChild.id, input: fixtureInput({ marker: input.marker }), computerId: input.childComputerId, idempotencyKey: `${turn.id}:helper`,
    })
    const outcome = await waitManagedTurn(turn.signal, child.sessionId, child.turnId)
    if (outcome.status !== "completed" || !outcome.result) throw new Error(`Helper did not complete: ${JSON.stringify(outcome)}`)
    const result = { marker: (outcome.result as {marker:string}).marker, childTurnId: child.turnId, childSessionId: child.sessionId, turnId: turn.id, count: ++ctx.setupResult.count }
    await turn.output.write([{ type: "json", value: result }])
    return result
  },
})
