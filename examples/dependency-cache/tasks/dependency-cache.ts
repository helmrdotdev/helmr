import { cache, image, sandbox, source, task } from "@helmr/sdk"
import { readFile, writeFile } from "node:fs/promises"

const deps = image("dependency-cache-deps")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .copy("/workspace/package.json", source.file("package.json"))
  .run(["bun", "install"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("dependency-cache-task-bun") }],
  })
  .workdir("/opt/app")
  .copy("/opt/app/package.json", source.file("app/package.json"))
  .copy("/opt/app/bun.lock", source.file("app/bun.lock"))
  .run(["bun", "install", "--frozen-lockfile"], {
    cache: [{ mountPath: "/root/.bun", cache: cache("bun-global") }],
  })

const sbx = sandbox("dependency-cache")
  .image(deps)
  .resources({ cpu: 2, memory: "2Gi" })

export const dependencyCache = task({
  id: "dependency-cache",
  sandbox: sbx,
  maxDuration: 600,
  run: async (_payload: unknown, ctx) => {
    const appPackage = JSON.parse(await readFile("/opt/app/package.json", "utf8")) as { readonly name?: string }
    const workspaceConfig = await readFile("helmr.config.ts", "utf8")
    const report = {
      dependencyPackage: appPackage.name,
      hasWorkspaceConfig: workspaceConfig.includes("defineConfig"),
      runId: ctx.run.id,
    }
    await writeFile("dependency-cache-report.json", `${JSON.stringify(report, null, 2)}\n`)
    ctx.log.info({ report: "dependency-cache-report.json" })
    return report
  },
})
