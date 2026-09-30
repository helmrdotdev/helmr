import { nodeVersion } from "../../scripts/node-version.mjs"
import { requireVersion } from "../../scripts/check-node-toolchain.mjs"
import { spawnSync } from "node:child_process"
import { mkdtempSync, writeFileSync, readFileSync, openSync, closeSync, rmSync } from "node:fs"
import { tmpdir } from "node:os"
import { resolve } from "node:path"

requireVersion(process.versions.node, "sample compiler interpreter")
const args = process.argv.slice(2)
if (args.length !== 0 && (args.length !== 2 || args[0] !== "--sdk-packages"))
  throw new Error("usage: check-fixture-analysis.mjs [--sdk-packages DIR]")
const output = mkdtempSync(resolve(tmpdir(), "helmr-sample-analysis-"))
try {
  const project = resolve(output, "project")
  const prepared = spawnSync(
    "python3",
    ["tests/e2e/prepare_project.py", project, "--fixtures", "cases", ...args],
    { stdio: "inherit" },
  )
  if (prepared.status !== 0) throw new Error("fixture preparation failed")
  for (const [name, dirs] of [
    ["execution", ["cases"]],
    ["schedule", ["fixtures/schedule/tasks"]],
  ]) {
    const config = resolve(output, `${name}-config.json`)
    writeFileSync(config, JSON.stringify({ dirs, ignorePatterns: ["**/run.ts", "**/*.run.ts", "**/*.test.*"], external: [], assets: [] }))
    const bundle = resolve(output, `${name}-bundle`)
    const bundled = spawnSync(process.execPath, [
      "--no-strip-types", "--no-global-search-paths", "internal/compiler/program-compiler.mjs",
      "--bundle", project, config, nodeVersion, bundle,
    ], { stdio: "inherit", env: { PATH: process.env.PATH } })
    if (bundled.status !== 0) throw new Error(`sample bundler exited ${bundled.status}`)
    const framePath = resolve(output, `${name}-result`)
    const fd = openSync(framePath, "w")
    let child
    try {
      // This sample check exercises analysis only. Go's preparation/admission tests
      // own the installed input digest; the compiler simply carries that authority.
      child = spawnSync(
        process.execPath,
        [
          "--no-strip-types",
          "--no-global-search-paths",
          "--enable-source-maps",
          "internal/compiler/program-compiler.mjs",
          "--analyze",
          resolve(bundle, "payload"),
          config,
          nodeVersion,
          `sha256:${"0".repeat(64)}`,
          resolve(bundle, "bundle.json"),
          resolve(output, name),
        ],
        { stdio: ["ignore", "inherit", "inherit", fd], env: { PATH: process.env.PATH } },
      )
    } finally {
      closeSync(fd)
    }
    if (child.status !== 0) throw new Error(`sample compiler exited ${child.status}`)
    const frame = readFileSync(framePath)
    if (frame.readUInt32BE(0) !== frame.length - 4)
      throw new Error("invalid sample verification frame")
    const result = JSON.parse(frame.subarray(4))
    if (result.outcome !== "succeeded") throw new Error(result.error.message)
    const plan = JSON.parse(result.files[0].content)
    const tasks = plan.definitions.filter((d) => d.kind === "task")
    if (name === "execution" && tasks.some((d) => d.manifest.schedule !== undefined))
      throw new Error("ordinary dev workflows must be schedule-free")
    if (
      name === "schedule" &&
      (tasks.length !== 1 ||
        tasks[0].manifest.schedule === undefined ||
        tasks[0].declaredId !== "schedule-smoke" ||
        tasks[0].manifest.run.ttlMs !== 300000)
    )
      throw new Error("Schedule fixture must analyze one bounded schedule-smoke Task")
    console.log(`analyzed ${name}: ${plan.definitions.length} definitions`)
  }
} finally {
  rmSync(output, { recursive: true, force: true })
}
