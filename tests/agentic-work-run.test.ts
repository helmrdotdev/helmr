// The agentic-work fixture's subprocess helper runs under the managed Node in
// real Workspaces, so each case drives the helper inside a Node process: stream
// error handling is the runtime's, not the test runner's.
import { expect, test } from "bun:test"
import { spawnSync } from "node:child_process"
import { fileURLToPath } from "node:url"

const helper = fileURLToPath(new URL("./fixtures/agentic-work/work/run.ts", import.meta.url))

// Evaluates `body` (an async expression using run/succeed) under Node and
// returns its JSON value, or the failed Node process for inspection.
function underNode(body: string) {
  const source = `import { run, succeed } from ${JSON.stringify(helper)}
const node = process.execPath
try { console.log(JSON.stringify({ value: await (${body}) })) } catch (error) { console.log(JSON.stringify({ rejected: String(error.message) })) }`
  const child = spawnSync("node", ["--input-type=module", "-e", source], { encoding: "utf8" })
  if (child.status !== 0) {
    throw new Error(`helper crashed the Node process (status ${child.status}):\n${child.stderr}`)
  }
  return JSON.parse(child.stdout.trim().split("\n").at(-1)!)
}

test("a command without input gets no stdin pipe to race against", () => {
  // A quick tool such as `git config` can exit before its parent writes to a
  // stdin pipe; with nothing to send there must be no pipe to write to.
  const probe = 'const s = require("node:fs").fstatSync(0); s.isFIFO() ? "pipe" : s.isCharacterDevice() ? "device" : "other"'
  expect(underNode(`succeed(node, ["-p", ${JSON.stringify(probe)}])`)).toEqual({ value: "device\n" })
})

test("input is delivered and the reply collected", () => {
  const echo = 'let s = ""; process.stdin.on("data", c => s += c).on("end", () => console.log(s.toUpperCase()))'
  expect(underNode(`run(node, ["-e", ${JSON.stringify(echo)}], { input: "payload" })`))
    .toEqual({ value: { code: 0, signal: null, stdout: "PAYLOAD\n", stderr: "", stdinError: null } })
})

test("a child that exits without reading its input is reported, not crashed on or hidden", () => {
  // More than a pipe buffer, so the write is still pending when the reader goes away.
  const refuse = 'process.stderr.write("refusing input\\n"); process.exit(3)'
  const result = underNode(`run(node, ["-e", ${JSON.stringify(refuse)}], { input: "x".repeat(4 << 20) })`)
  expect(result.value.code).toBe(3)
  expect(result.value.stderr).toBe("refusing input\n")
  expect(result.value.stdinError).toMatch(/EPIPE|ECONNRESET/)
})

test("a command that cannot start still rejects", () => {
  expect(underNode('run("/nonexistent/helmr-fixture-tool", [])').rejected).toContain("ENOENT")
})

test("succeed reports the failing command with its exit code and stderr", () => {
  const fail = 'console.error("boom"); process.exit(2)'
  expect(underNode(`succeed(node, ["-e", ${JSON.stringify(fail)}])`).rejected).toMatch(/exited 2: boom/)
})
