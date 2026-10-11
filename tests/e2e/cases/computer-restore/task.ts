import { callMcp, fixtureInput, fixtureValue, waitManagedTurn, type ManagedAdmission } from "../../support/runtime-mcp"
import { agent, image, computer } from "@helmr/sdk"
import { randomUUID } from "node:crypto"
import { readFile, writeFile } from "node:fs/promises"
import { z } from "zod"

export const restoreComputer = computer({
  id: "helmr-computer-restore", image: image("helmr-computer-restore").from("node:24-bookworm-slim").workdir("/root"),
  resources: { cpu: 1, memory: "1GiB" },
})
const payload = z.object({ marker: z.string().min(1) }).strict()
export const restoreChild = agent({
  id: "computer-restore-child", computer: restoreComputer, maxTurnDuration: "1h",
  setup: () => ({ nonce: randomUUID(), count: 0, marker: "" }),
  turn: async (turn, ctx) => {
    const { marker } = payload.parse(fixtureValue(turn.input))
    const memory = ctx.setupResult, count = ++memory.count
    const path = `/root/child-${ctx.session.id}.json`
    if (count === 1) {
      memory.marker = marker
      await writeFile(path, JSON.stringify({ marker, nonce: memory.nonce }), { flag: "wx" })
    } else if (count !== 2 || memory.marker !== marker) throw new Error("Helper memory changed")
    const stored = JSON.parse(await readFile(path, "utf8"))
    if (stored.marker !== marker || stored.nonce !== memory.nonce) throw new Error("Helper memory/file continuity failed")
    if (count === 2) await writeFile(path, JSON.stringify({ marker, nonce: memory.nonce, resumed: true }))
    return { marker, nonce: memory.nonce, count, childTurnId: turn.id, childSessionId: ctx.session.id, computerId: ctx.computer.id }
  },
})
export const restoreParent = agent({
  id: "computer-restore-parent", computer: restoreComputer, maxTurnDuration: "1h",
  setup: () => ({ nonce: randomUUID(), count: 0, marker: "", childSessionId: "", childNonce: "" }),
  turn: async (turn, ctx) => {
    const { marker } = payload.parse(fixtureValue(turn.input))
    const memory = ctx.setupResult, count = ++memory.count
    const path = `/root/parent-${ctx.session.id}.json`
    let childTurn: string
    if (count === 1) {
      memory.marker = marker
      await writeFile(path, JSON.stringify({ marker, nonce: memory.nonce }), { flag: "wx" })
      const child = await callMcp<ManagedAdmission>(turn.signal, "spawn", {
        agentId: restoreChild.id, input: fixtureInput({ marker }), computerId: ctx.computer.id, idempotencyKey: `${turn.id}:child`,
      })
      memory.childSessionId = child.sessionId
      childTurn = child.turnId
    } else {
      if (count !== 2 || memory.marker !== marker) throw new Error("Parent memory changed")
      childTurn = (await callMcp<ManagedAdmission>(turn.signal, "enqueue", { sessionId: memory.childSessionId, input: fixtureInput({ marker }), idempotencyKey: `${turn.id}:child` })).turnId
    }
    const outcome = await waitManagedTurn(turn.signal, memory.childSessionId, childTurn)
    if (outcome.status !== "completed") throw new Error(`Helper failed: ${JSON.stringify(outcome)}`)
    const child = z.object({ marker: z.string(), nonce: z.string(), count: z.number(), childSessionId: z.string(), childTurnId: z.string(), computerId: z.string() }).strict().parse(outcome.result)
    if (count === 1) memory.childNonce = child.nonce
    const parentFile = JSON.parse(await readFile(path, "utf8"))
    const childFile = JSON.parse(await readFile(`/root/child-${memory.childSessionId}.json`, "utf8"))
    if (parentFile.nonce !== memory.nonce || parentFile.marker !== marker || child.nonce !== memory.childNonce ||
      child.childSessionId !== memory.childSessionId || child.count !== count || child.computerId !== ctx.computer.id ||
      childFile.nonce !== memory.childNonce || childFile.marker !== marker || (count === 2 && childFile.resumed !== true)) {
      throw new Error("Shared Computer continuation changed")
    }
    const result = { marker, count, parentTurnId: turn.id, childTurnId: child.childTurnId, childSessionId: memory.childSessionId,
      computerId: ctx.computer.id, parentNonce: memory.nonce, childNonce: memory.childNonce,
      parentMemoryPreserved: true, childMemoryPreserved: true, childPostRestoreWriteObserved: count === 2 }
    if (count === 2) {
      let finish!: () => void
      const observed = new Promise<void>(resolve => { finish = resolve })
      await turn.onMessage(message => { if (fixtureValue(message) === "finish") finish() })
      await turn.output.write([{ type: "json", value: { ...result, phase: "restored" } }])
      await observed
    }
    return result
  },
})
