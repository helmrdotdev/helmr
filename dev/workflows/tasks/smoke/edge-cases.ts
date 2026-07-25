import { image, task, tokens, workspace, type HelmrError, type JsonValue } from "@helmr/sdk"
import { appendFile, mkdir, readFile, writeFile } from "node:fs/promises"
import { z } from "zod"

const base = image("helmr-edge-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .workdir("/workspace")

export const edgeSmokeWorkspace = workspace("helmr-edge-smoke")
  .image(base)
  .resources({ cpu: 1, memory: "1GiB" })

const payload = z.object({
  mode: z.enum(["concurrent-wait", "workspace-overwrite", "expected-error"]),
  marker: z.string().optional(),
  waitTimeout: z.number().int().positive().max(120).default(30),
}).strict()

type Payload = z.infer<typeof payload>

const approvalDecision = z.object({
  approved: z.boolean(),
})

export const edgeSmoke = task({
  id: "edge-smoke",
  maxDuration: "5m",
  payload,
  run: async (input: Payload, ctx): Promise<JsonValue> => {
    const marker = input.marker?.trim() || `edge-${ctx.run.id}`
    console.info({ phase: "edge-smoke", mode: input.mode, marker })

    switch (input.mode) {
      case "concurrent-wait":
        return {
          mode: input.mode,
          marker,
          concurrentWaitRejected: await assertConcurrentWaitRejected(input.waitTimeout),
        }
      case "workspace-overwrite":
        return {
          mode: input.mode,
          marker,
          workspace: await exerciseWorkspaceOverwrite(marker),
        }
      case "expected-error":
        throw new Error(`intentional edge-case failure for marker ${marker}`)
    }
  },
})

async function assertConcurrentWaitRejected(timeout: number): Promise<boolean> {
  const duration = `${timeout}s`
  const firstToken = await tokens.create({
    timeout: duration,
    tags: ["smoke", "edge-case"],
    metadata: { subject: "Concurrent wait diagnostic first wait" },
  })
  const first = firstToken.wait({
    schema: approvalDecision,
    timeout: duration,
    tags: ["smoke", "edge-case"],
    metadata: { subject: "Concurrent wait diagnostic first wait" },
  }).unwrap()
  try {
    const secondToken = await tokens.create({
      timeout: duration,
      tags: ["smoke", "edge-case"],
      metadata: { subject: "Concurrent wait diagnostic second wait" },
    })
    await secondToken.wait({
      schema: approvalDecision,
      timeout: duration,
      tags: ["smoke", "edge-case"],
      metadata: { subject: "Concurrent wait diagnostic second wait" },
    }).unwrap()
  } catch (error) {
    if (
      typeof error === "object" &&
      error !== null &&
      "code" in error &&
      (error as HelmrError).code === "concurrent_wait"
    ) {
      return true
    }
    throw error
  } finally {
    first.then(() => undefined, () => undefined)
  }
  throw new Error("second wait unexpectedly started while first wait was active")
}

async function exerciseWorkspaceOverwrite(marker: string): Promise<{ path: string, content: string }> {
  await mkdir("edge", { recursive: true })
  const path = "edge/overwrite.txt"
  await writeFile(path, `first:${marker}\n`)
  await appendFile(path, `second:${marker}\n`)
  await writeFile(path, `final:${marker}\n`)
  const content = await readFile(path, "utf8")
  if (content !== `final:${marker}\n`) {
    throw new Error(`workspace overwrite produced unexpected content: ${content}`)
  }
  return { path, content }
}
