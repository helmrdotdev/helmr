import assert from "node:assert/strict"
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"
import { test } from "node:test"
import { checkRepository } from "../agents/issue-fixer/checks"

const alive = (pid: number) => {
  try { process.kill(pid, 0); return true } catch { return false }
}
async function executing(pid: number): Promise<boolean> {
  if (process.platform !== "linux") return alive(pid)
  try {
    const stat = await readFile(`/proc/${pid}/stat`, "utf8")
    return !["Z", "X"].includes(stat.slice(stat.lastIndexOf(")") + 2).split(" ")[0]!)
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return false
    throw error
  }
}
async function waitFor<T>(read: () => Promise<T>): Promise<T> {
  let last: unknown
  for (let attempt = 0; attempt < 200; attempt++) {
    try { return await read() } catch (error) { last = error; await delay(25) }
  }
  throw last
}

for (const ignoresStop of [false, true]) {
  test(`npm waits for ${ignoresStop ? "uncooperative" : "delayed"} work to cease`, { timeout: 15000, skip: process.platform !== "linux" }, async () => {
    const cwd = await mkdtemp(join(tmpdir(), "helmr-command-test-"))
    const controller = new AbortController()
    let pid: number | undefined
    let work: Promise<unknown> | undefined
    try {
      const runner = `
        const fs = require("node:fs");
        // Publish via rename so concurrent readers never observe a truncated file.
        const publish = (name, value) => { fs.writeFileSync(name + ".tmp", value); fs.renameSync(name + ".tmp", name) };
        process.on("SIGTERM", () => {
          publish("stopping", "1");
          ${ignoresStop ? "" : 'setInterval(() => fs.existsSync("release") && process.exit(0), 10)'}
        });
        publish("ready", String(process.pid));
        let n = 0; setInterval(() => publish("heartbeat", String(++n)), 20);
      `
      await writeFile(join(cwd, "package.json"), JSON.stringify({ scripts: { test: "node runner.cjs" } }))
      await writeFile(join(cwd, "runner.cjs"), runner)
      work = checkRepository(cwd, controller.signal)
      let settled = false
      const outcome = work.then(() => { settled = true; return undefined }, (error: unknown) => { settled = true; return error })
      pid = await waitFor(async () => Number(await readFile(join(cwd, "ready"), "utf8")))
      const heartbeat = async () => Number(await readFile(join(cwd, "heartbeat"), "utf8"))
      await waitFor(heartbeat)
      controller.abort()
      await waitFor(() => readFile(join(cwd, "stopping"), "utf8"))
      await delay(150)
      assert.equal(settled, false, "AbortError must not settle the handler while work is alive")
      assert.equal(alive(pid), true)
      const before = await heartbeat()
      await waitFor(async () => assert.ok(await heartbeat() > before, "work must keep running after the stop request"))
      assert.equal(settled, false)
      if (ignoresStop) process.kill(pid, "SIGKILL")
      else await writeFile(join(cwd, "release"), "")
      const error = await outcome
      assert.equal((error as Error).name, "AbortError")
      assert.equal(await executing(pid), false, "work must have ceased before rejection")
      const after = await readFile(join(cwd, "heartbeat"), "utf8")
      await delay(80)
      assert.equal(await readFile(join(cwd, "heartbeat"), "utf8"), after)
    } finally {
      controller.abort()
      if (pid && alive(pid)) process.kill(pid, "SIGKILL")
      if (work) await work.catch(() => {})
      await rm(cwd, { recursive: true, force: true })
    }
  })
}

test("repository checks preserve success, failure, missing executable and pre-abort", async () => {
  const cwd = await mkdtemp(join(tmpdir(), "helmr-check-test-"))
  const previousPath = process.env.PATH
  try {
    for (const code of [0, 7]) {
      await writeFile(join(cwd, "package.json"), JSON.stringify({ scripts: { test: "node runner.cjs" } }))
      await writeFile(join(cwd, "runner.cjs"), `process.exit(${code})`)
      const work = checkRepository(cwd, new AbortController().signal)
      if (code === 0) await work
      else await assert.rejects(work, /Repository checks failed/)
    }
    process.env.PATH = cwd
    await assert.rejects(checkRepository(cwd, new AbortController().signal), { code: "ENOENT" })
    await assert.rejects(checkRepository(cwd, AbortSignal.abort()), { name: "AbortError" })
  } finally {
    process.env.PATH = previousPath
    await rm(cwd, { recursive: true, force: true })
  }
})
