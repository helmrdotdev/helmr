import test from "node:test"
import assert from "node:assert/strict"
import { mkdtemp, readFile, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { computer, image } from "@helmr/sdk"
import { runComputerPreparation } from "./computer-preparation"

const base = { id: "prepared", image: image("base").from("ubuntu:24.04"), resources: { cpu: 1, memory: "1GiB" } }
test("preparation executes bundled ordinary code with cwd, environment and argv boundaries", async () => {
  const cwd = await mkdtemp(join(tmpdir(), "helmr-prepare-"))
  try {
    await runComputerPreparation(computer({ ...base, prepare: async build => {
      await build.exec('printf "%s" "$BUILD_VALUE" > shell-result', { cwd, env: { BUILD_VALUE: "prepared value" } })
      await build.exec([process.execPath, "-e", 'require("node:fs").writeFileSync("argv-result", process.argv[1])', "literal ; $(not-a-command)"], { cwd })
      await assert.rejects(build.exec([process.execPath, "-e", "process.exit(7)"]), /status 7/)
    } }), new AbortController().signal)
    assert.equal(await readFile(join(cwd, "shell-result"), "utf8"), "prepared value")
    assert.equal(await readFile(join(cwd, "argv-result"), "utf8"), "literal ; $(not-a-command)")
  } finally { await rm(cwd, { recursive: true, force: true }) }
})

test("preparation propagates failure and waits for in-flight commands before success", async () => {
  let complete = false
  await runComputerPreparation(computer({ ...base, prepare: build => {
    void build.exec([process.execPath, "-e", "setTimeout(()=>{},40)"]).then(() => { complete = true })
  } }), new AbortController().signal)
  assert.equal(complete, true)
  await assert.rejects(runComputerPreparation(computer({ ...base, prepare: build => build.exec([process.execPath, "-e", "process.exit(9)"]) }), new AbortController().signal), /status 9/)
})

test("cancellation joins the command even when it ignores graceful termination", async () => {
  const cancellation = new AbortController()
  const execution = runComputerPreparation(computer({ ...base, prepare: build => build.exec([process.execPath, "-e", 'process.on("SIGTERM",()=>{});setInterval(()=>{},1000)']) }), cancellation.signal)
  const cancelled = assert.rejects(execution, error => error instanceof Error && error.name === "AbortError")
  cancellation.abort()
  await cancelled
  const already = new AbortController()
  already.abort()
  let started = false
  await assert.rejects(runComputerPreparation(computer({ ...base, prepare: () => { started = true } }), already.signal), /abort/i)
  assert.equal(started, false)
})
