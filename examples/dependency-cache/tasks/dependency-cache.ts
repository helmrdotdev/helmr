import { cache, image, sandbox, source, task } from "@helmr/sdk"

const deps = image("dependency-cache-deps")
  .from("oven/bun:1.3.10-debian")
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
    const appPackage = await Bun.file("/opt/app/package.json").json()
    const workspaceConfig = await Bun.file("helmr.config.ts").text()
    const report = {
      dependencyPackage: appPackage.name,
      hasWorkspaceConfig: workspaceConfig.includes("defineConfig"),
      runId: ctx.run.id,
    }
    await Bun.write("dependency-cache-report.json", `${JSON.stringify(report, null, 2)}\n`)
    ctx.log.info({ report: "dependency-cache-report.json" })
    return report
  },
})
