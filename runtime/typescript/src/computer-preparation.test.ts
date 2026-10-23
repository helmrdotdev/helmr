import test from "node:test"
import assert from "node:assert/strict"
import { mkdtemp, readFile, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"
import { template, image } from "@helmr/sdk"
import { runComputerPreparation } from "./computer-preparation"

const base = { id: "prepared", image: image("base").from("ubuntu:24.04"), resources: { cpu: 1, memory: "1GiB" } }
test("preparation executes bundled ordinary code with cwd, environment and argv boundaries", async () => {
  const cwd = await mkdtemp(join(tmpdir(), "helmr-prepare-"))
  try {
    await runComputerPreparation(template({ ...base, prepare: async build => {
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
  await runComputerPreparation(template({ ...base, prepare: build => {
    void build.exec([process.execPath, "-e", "setTimeout(()=>{},40)"]).then(() => { complete = true })
  } }), new AbortController().signal)
  assert.equal(complete, true)
  await assert.rejects(runComputerPreparation(template({ ...base, prepare: build => build.exec([process.execPath, "-e", "process.exit(9)"]) }), new AbortController().signal), /status 9/)
})

test("cancellation joins the command even when it ignores graceful termination", async () => {
  const cwd = await mkdtemp(join(tmpdir(), "helmr-prepare-cancel-"))
  const cancellation = new AbortController()
  let pid: number | undefined
  let timeout: ReturnType<typeof setTimeout> | undefined
  const execution = runComputerPreparation(template({ ...base, prepare: build => build.exec([
    process.execPath, "-e",
    'process.on("SIGTERM",()=>{});setInterval(()=>{},1000);const fs=require("node:fs");fs.writeFileSync("ready.tmp",String(process.pid));fs.renameSync("ready.tmp","ready")',
  ], { cwd }) }), cancellation.signal)
  try {
    const readyDeadline = Date.now() + 2000
    for (;;) {
      try { pid = Number(await readFile(join(cwd, "ready"), "utf8")); break }
      catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error }
      assert(Date.now() < readyDeadline, "child did not install its SIGTERM handler")
      await delay(10)
    }
    assert(Number.isInteger(pid) && pid! > 0)
    process.kill(pid!, 0)
    const joined = Promise.race([
      execution,
      new Promise<never>((_, reject) => {
        timeout = setTimeout(() => reject(new Error("cancellation did not join the ready child")), 2000)
      }),
    ])
    cancellation.abort()
    await assert.rejects(joined, error => error instanceof Error && error.name === "AbortError")
    assert.throws(() => process.kill(pid!, 0), { code: "ESRCH" })
  } finally {
    clearTimeout(timeout)
    cancellation.abort()
    if (pid !== undefined) {
      try { process.kill(pid, "SIGKILL") }
      catch (error) { if ((error as NodeJS.ErrnoException).code !== "ESRCH") throw error }
    }
    await execution.catch(() => {})
    await rm(cwd, { recursive: true, force: true })
  }
})

test("already-cancelled preparation does not enter its hook", async () => {
  const already = new AbortController()
  already.abort()
  let started = false
  await assert.rejects(runComputerPreparation(template({ ...base, prepare: () => { started = true } }), already.signal), /abort/i)
  assert.equal(started, false)
})
