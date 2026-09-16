import { execFileSync } from "node:child_process"
import { mkdtempSync, readFileSync, realpathSync, rmSync } from "node:fs"
import { tmpdir } from "node:os"
import { delimiter, join, resolve } from "node:path"
import { fileURLToPath } from "node:url"
import { nodeVersion } from "./node-version.mjs"

const root = fileURLToPath(new URL("../", import.meta.url))
export const guards = ["dev/workflows", "examples/cli-tooling", "examples/github-pr-review", "examples/hello-world", "examples/task-secrets"]
export function requireVersion(actual, label) {
  if (actual !== nodeVersion) throw new Error(`${label}: expected Product Node ${nodeVersion}, got ${actual}`)
}
export function checkGuard(pkg, label) {
  const guard = pkg.devEngines?.runtime
  if (guard?.name !== "node" || guard?.onFail !== "error") throw new Error(`${label}: exact Node error guard required`)
  requireVersion(guard.version, label)
}
export function checkTarget(target) {
  if (target !== `node${nodeVersion}`) throw new Error(`stale generator Node target: ${target}`)
}
export function managerInterpreter(command) {
  const path = process.env.PATH.split(delimiter).map(dir => join(dir, command)).find(path => {
    try { realpathSync(path); return true } catch { return false }
  })
  if (!path) throw new Error(`missing ${command}`)
  const script = realpathSync(path)
  const shebang = readFileSync(script, "utf8").split("\n")[0]
  // These are the native Nix npm/npx/Corepack JS entrypoints, not arbitrary wrappers.
  const interpreter = shebang === "#!/usr/bin/env node" ? "node" : shebang.match(/^#!(\/\S*\/node)$/)?.[1]
  if (!interpreter) throw new Error(`unsupported ${command} interpreter: ${shebang}`)
  const identity = JSON.parse(execFileSync(interpreter, ["-p", "JSON.stringify({version:process.versions.node,execPath:process.execPath})"], { encoding: "utf8" }))
  requireVersion(identity.version, `${command} interpreter ${identity.execPath}`)
  return { command, script, ...identity }
}
export function checkNodeToolchain(cli) {
  requireVersion(process.versions.node, `running interpreter ${process.execPath}`)
  for (const name of guards) checkGuard(JSON.parse(readFileSync(join(root, name, "package.json"))), name)
  checkTarget(execFileSync("bun", [join(root, "scripts/build-platform-entries.ts"), "--node-target"], { encoding: "utf8", cwd: root }).trim())
  for (const command of ["npm", "npx", "corepack"]) {
    console.log(JSON.stringify(managerInterpreter(command)))
    execFileSync(command, ["--version"], { stdio: "inherit" })
  }
  if (cli) {
    const work = mkdtempSync(join(tmpdir(), "helmr-node-init-"))
    try {
      execFileSync(resolve(cli), ["init", "--dir", work], { stdio: "inherit" })
      checkGuard(JSON.parse(readFileSync(join(work, "package.json"))), "helmr init")
    } finally { rmSync(work, { recursive: true, force: true }) }
  }
  console.log(`Product Node ${nodeVersion}: interpreter, managers and source projections match`)
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) checkNodeToolchain(process.argv[2])
