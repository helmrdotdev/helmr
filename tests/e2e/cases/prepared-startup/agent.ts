import { agent } from "@helmr/sdk"
import { createHash, randomUUID } from "node:crypto"
import { writeFile } from "node:fs/promises"
import { runtimeSmokeComputer } from "../runtime/agent"
import { fixtureValue } from "../../support/runtime-mcp"

export const preparedStartup = agent({
  id: "prepared-startup",
  template: runtimeSmokeComputer,
  maxTurnDuration: "2m",
  async init() {
    // Record entry before reading secrets so even a failed preclaim invocation
    // makes the admitted initialization fail the exclusive-create assertion.
    const nonce = randomUUID()
    await writeFile("/workspace/prepared-agent-init.json", JSON.stringify({ nonce }), { flag: "wx" })
    await writeFile("/workspace/prepared-private", nonce, { flag: "wx" })
    const value = process.env.VERIFICATION_SECRET
    if (!value) throw new Error("Fresh Computer secret is missing")
    const state = { nonce, secretHash: createHash("sha256").update(value).digest("hex"), count: 0 }
    await writeFile("/workspace/prepared-agent-init.json", JSON.stringify(state))
    return state
  },
  async turn(turn, ctx) {
    const input = fixtureValue(turn.input)
    if (input !== ctx.initResult.secretHash) throw new Error("Initialization used stale secret material")
    return { ...ctx.initResult, count: ++ctx.initResult.count, computerId: ctx.computer.id, sessionId: ctx.session.id }
  },
})
