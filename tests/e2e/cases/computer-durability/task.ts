import { actor, image, sandbox, task, tokens } from "@helmr/sdk"
import { randomUUID } from "node:crypto"
import { mkdir, readFile, readlink, symlink, writeFile } from "node:fs/promises"
import { z } from "zod"

export const durabilitySandbox = sandbox({ id: "computer-durability" })
  .image(image("computer-durability").from("node:24-bookworm-slim").workdir("/sandbox"))
  .resources({ cpu: 1, memory: "1GiB" })

const inputSchema = z.object({
  marker: z.string().min(1),
  tokenId: z.string().min(1),
  idleTimeout: z.string().default("2s"),
}).strict()

// Keep state outside the working directory, as agent tools commonly do.
const directory = "/root/helmr-durability"
async function prepare(marker: string, identity: string) {
  const path = `${directory}/${identity}`
  await mkdir(path, { recursive: true })
  await writeFile(`${path}/state.json`, JSON.stringify({ marker }))
  await symlink("state.json", `${path}/current`)
  return path
}
async function verify(path: string, marker: string) {
  if (await readlink(`${path}/current`) !== "state.json") throw new Error("symlink changed")
  const state = JSON.parse(await readFile(`${path}/current`, "utf8"))
  if (state.marker !== marker) throw new Error("persisted marker changed")
}

// The driver creates the Token first, observes this nonce in logs, waits for an
// actual checkpoint/VM release, then completes the Token. Compare the returned
// nonce with the logged one: a rerun from the function entry is not continuation.
export const durabilityTask = task({
  id: "computer-durability-task",
  payload: inputSchema,
  maxDuration: "10m",
  retry: { enabled: false },
  async run(input, ctx) {
    const nonce = randomUUID()
    const path = await prepare(input.marker, nonce)
    console.info({ phase: "durability_waiting", runId: ctx.run.id, nonce, path })
    await tokens.ref(input.tokenId).wait({ timeout: "8m", idleTimeout: input.idleTimeout }).unwrap()
    await verify(path, input.marker)
    return { marker: input.marker, nonce, path }
  },
})

// One Session can execute several Turns without a mandatory save per Turn.
// The driver also checks the same runtimeNonce before/after the managed wait.
export const durabilityActor = actor({
  id: "computer-durability-actor",
  idleTimeout: "2s",
  async run(session) {
    const runtimeNonce = randomUUID()
    for (;;) {
      const turn = await session.receive()
      if (turn === null) return
      const input = inputSchema.parse(turn.input)
      const path = await prepare(input.marker, randomUUID())
      await turn.output.write({ type: "durability_waiting", runtimeNonce, path })
      await tokens.ref(input.tokenId).wait({ timeout: "8m", idleTimeout: input.idleTimeout }).unwrap()
      await verify(path, input.marker)
      await turn.complete({ marker: input.marker, runtimeNonce, path })
    }
  },
})
