import { execFileSync } from "node:child_process"
import { fileURLToPath } from "node:url"
import { test } from "node:test"
import assert from "node:assert/strict"
import { mkdtempSync, writeFileSync, mkdirSync, rmSync, symlinkSync, readFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { checkGuard, checkTarget, requireVersion, managerInterpreter, guards } from "../scripts/check-node-toolchain.mjs"
import { nodeVersion } from "../scripts/node-version.mjs"

test("exact interpreter, guard and generator target reject drift", () => {
  requireVersion(nodeVersion, "fixture")
  assert.throws(() => requireVersion("24.19.0", "interpreter"), /expected Product Node/)
  const pkg = { devEngines: { runtime: { name: "node", version: nodeVersion, onFail: "error" } } }
  checkGuard(pkg, "fixture")
  for (const [key, value] of [["version", "24.19.0"], ["onFail", "warn"], ["name", "bun"]]) {
    const bad = structuredClone(pkg); bad.devEngines.runtime[key] = value
    assert.throws(() => checkGuard(bad, "fixture"))
  }
  checkTarget(`node${nodeVersion}`)
  assert.throws(() => checkTarget("node24.19"), /stale generator/)
})

test("manager interpreter is checked independently of PATH node", () => {
  const directory = mkdtempSync(join(tmpdir(), "helmr-manager-interpreter-"))
  const saved = process.env.PATH
  try {
    mkdirSync(join(directory, "old"))
    // Isolated negative interpreter fixture; real retained-old npm is also exercised in native acceptance.
    writeFileSync(join(directory, "old/node"), '#!/bin/sh\nprintf \'%s\\n\' \'{"version":"24.19.0","execPath":"/old/node"}\'\n', { mode: 0o755 })
    symlinkSync(process.execPath, join(directory, "node"))
    writeFileSync(join(directory, "npm"), `#!${directory}/old/node\n`, { mode: 0o755 })
    process.env.PATH = directory
    assert.throws(() => managerInterpreter("npm"), /npm interpreter.*24.19.0/)
  } finally {
    process.env.PATH = saved
    rmSync(directory, { recursive: true, force: true })
  }
})


test("checked-in guards and executed generator target match Product intent", () => {
  const root = fileURLToPath(new URL("../", import.meta.url))
  for (const name of guards) checkGuard(JSON.parse(readFileSync(join(root, name, "package.json"))), name)
  checkTarget(execFileSync("bun", ["scripts/build-platform-entries.ts", "--node-target"], { cwd: root, encoding: "utf8" }).trim())
})
