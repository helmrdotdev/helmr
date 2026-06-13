import { cache, ConcurrentWaitError, image, sandbox, source, task, type TaskContext } from "@helmr/sdk"
import { appendFile, mkdir, readFile, writeFile } from "node:fs/promises"
import { z } from "zod"

const dependencyInputs = source.directory(".", {
  ignore: ["*", "!package.json", "!bun.lock", "!tsconfig.json"],
})

const base = image("helmr-edge-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .copy("/workspace", dependencyInputs)
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .run(["bun", "install", "--frozen-lockfile"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("edge-smoke-bun") }],
  })

const sbx = sandbox("helmr-edge-smoke")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi", disk: "8Gi" })

const payload = z.object({
  mode: z.enum(["concurrent-wait", "workspace-overwrite", "expected-error"]),
  marker: z.string().optional(),
  waitTimeout: z.number().int().positive().max(120).default(30),
}).strict()

type Payload = z.infer<typeof payload>

export const edgeSmoke = task({
  id: "edge-smoke",
  sandbox: sbx,
  maxDuration: 300,
  payload,
  run: async (input: Payload, ctx) => {
    const marker = input.marker?.trim() || `edge-${ctx.run.id}`
    ctx.log.info({ phase: "edge-smoke", mode: input.mode, marker })

    switch (input.mode) {
      case "concurrent-wait":
        return {
          mode: input.mode,
          marker,
          concurrentWaitRejected: await assertConcurrentWaitRejected(ctx, input.waitTimeout),
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

async function assertConcurrentWaitRejected(ctx: TaskContext, timeout: number): Promise<boolean> {
  const first = ctx.wait.human<{ approved: boolean }>({
    displayText: "Concurrent wait diagnostic first wait",
    timeout,
  })
  try {
    await ctx.wait.human<{ approved: boolean }>({
      displayText: "Concurrent wait diagnostic second wait",
      timeout,
    })
  } catch (error) {
    if (error instanceof ConcurrentWaitError || String(error).includes("ConcurrentWaitError")) {
      return true
    }
    throw error
  } finally {
    first.catch(() => undefined)
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
