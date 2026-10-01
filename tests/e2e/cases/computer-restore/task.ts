import { image, sandbox, task, tokens } from "@helmr/sdk"
import { randomUUID } from "node:crypto"
import { readFile, writeFile } from "node:fs/promises"
import { z } from "zod"

export const restoreComputer = sandbox({ id: "helmr-computer-restore" })
  .image(image("helmr-computer-restore").from("node:24-bookworm-slim").workdir("/root"))
  .resources({ cpu: 1, memory: "1GiB" })

const payload = z.object({ marker: z.string().min(1) }).strict()
export const restoreChild = task({
  id: "computer-restore-child",
  maxDuration: "1h",
  retry: { enabled: false },
  payload,
  run: async ({ marker }, ctx) => {
    const nonce = randomUUID()
    const path = `/root/child-${ctx.run.id}.json`
    await writeFile(path, JSON.stringify({ marker, nonce }), { flag: "wx" })
    const token = await tokens.create({ timeout: "50m", metadata: { marker } })
    await token.wait({ timeout: "45m", idleTimeout: "2s" }).unwrap()
    const restored = JSON.parse(await readFile(path, "utf8"))
    if (restored.marker !== marker || restored.nonce !== nonce) throw new Error("Child memory/file continuity failed")
    await writeFile(path, JSON.stringify({ marker, nonce, resumed: true }))
    return { marker, nonce, childRunId: ctx.run.id, computerId: ctx.computer.id }
  },
})

export const restoreParent = task({
  id: "computer-restore-parent",
  maxDuration: "1h",
  retry: { enabled: false },
  payload,
  run: async ({ marker }, ctx) => {
    const nonce = randomUUID()
    const path = `/root/parent-${ctx.run.id}.json`
    await writeFile(path, JSON.stringify({ marker, nonce }), { flag: "wx" })
    const child = await restoreChild.call({ marker }, {
      computer: ctx.computer,
      idempotencyKey: `${ctx.run.id}:child`,
    }).unwrap()
    const parentFile = JSON.parse(await readFile(path, "utf8"))
    const childFile = JSON.parse(await readFile(`/root/child-${child.childRunId}.json`, "utf8"))
    if (parentFile.nonce !== nonce || parentFile.marker !== marker ||
        childFile.nonce !== child.nonce || childFile.marker !== marker || childFile.resumed !== true ||
        child.computerId !== ctx.computer.id) throw new Error("Shared Computer restore continuity failed")
    return { marker, parentRunId: ctx.run.id, childRunId: child.childRunId,
      computerId: ctx.computer.id, parentNonce: nonce, childNonce: child.nonce,
      parentMemoryPreserved: true, childMemoryPreserved: true, childPostRestoreWriteObserved: true }
  },
})
