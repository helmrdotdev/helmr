import { agent, image, source, template, type JsonValue } from "@helmr/sdk"
import { execFile } from "node:child_process"
import { randomUUID } from "node:crypto"
import { promisify } from "node:util"
import { z } from "zod"
import { fixtureValue } from "../../support/runtime-mcp"
import { configuration, finiteNetwork } from "./config"
import { waitForProbePermit } from "./permit"

const program = "/opt/helmr/private-execution-policy-probe.mjs"
const base = image("private-execution-policy-probes").from("node:24-bookworm-slim")
  .copy(source.file("cases/private-execution-policy/probe.mjs"), program).workdir("/workspace")
const resources = { cpu: 1, memory: "1GiB" }
export const ordinaryComputer = template({ id: "private-policy-ordinary", image: base, resources })
export const finiteComputer = template({
  id: "private-policy-finite", image: base, resources,
  network: finiteNetwork, prepareNetwork: { blockAll: true },
  async prepare(ctx) { await ctx.exec(["node", program, JSON.stringify(configuration), "prepare", "finite"]) },
})
export const blockedComputer = template({
  id: "private-policy-blocked", image: base, resources,
  network: { blockAll: true }, prepareNetwork: finiteNetwork,
  async prepare(ctx) { await ctx.exec(["node", program, JSON.stringify(configuration), "prepare", "blocked"]) },
})

// Explicit Computer reuse lets the driver verify the actual Computer's policy,
// including when this Agent's ordinary default Template is different.
export const privatePolicyProbe = agent({
  id: "private-policy-probe", template: ordinaryComputer,
  maxTurnDuration: "15m",
  init: async () => ({ nonce: randomUUID(), turns: 0 }),
  async turn(turn, ctx): Promise<JsonValue> {
    const input = z.object({ subject: z.enum(["ordinary", "finite", "blocked"]) }).strict().parse(fixtureValue(turn.input))
    await waitForProbePermit(turn, { phase: "ready-for-positive-control", computerId: ctx.computer.id, subject: input.subject })
    const { stdout } = await promisify(execFile)("node", [program, JSON.stringify(configuration), "runtime", input.subject],
      { timeout: 240_000, signal: turn.signal, maxBuffer: 65536 })
    const network = JSON.parse(stdout)
    return { network, computerId: ctx.computer.id, nonce: ctx.initResult.nonce, count: ++ctx.initResult.turns }
  },
})
