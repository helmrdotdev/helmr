import { mkdtemp, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { run, succeed } from "./run.ts"

// A coding step: reproduce a failing test in a repository, edit the source,
// rerun the tests and report the resulting diff and history.
export async function gitWork() {
  const repo = await mkdtemp(join(tmpdir(), "agentic-git-"))
  try {
    const git = (...args: string[]) => succeed("git", args, repo)
    await git("init", "--quiet", "--initial-branch=main")
    await git("config", "user.name", "Agentic Fixture")
    await git("config", "user.email", "fixture@example.invalid")
    await writeFile(join(repo, "total.mjs"), "export const total = values => values.reduce((sum, value) => sum - value, 0)\n")
    await writeFile(join(repo, "total.test.mjs"), [
      'import assert from "node:assert/strict"',
      'import test from "node:test"',
      'import { total } from "./total.mjs"',
      'test("adds", () => assert.equal(total([2, 3, 5]), 10))',
      "",
    ].join("\n"))
    await git("add", ".")
    await git("commit", "--quiet", "-m", "add total")

    const before = await run(process.execPath, ["--test"], { cwd: repo })
    await writeFile(join(repo, "total.mjs"), "export const total = values => values.reduce((sum, value) => sum + value, 0)\n")
    const after = await run(process.execPath, ["--test"], { cwd: repo })
    const diff = await git("diff", "--unified=0")
    await git("commit", "--quiet", "-am", "fix total")
    const log = (await git("log", "--format=%s")).trim().split("\n")
    const status = await git("status", "--porcelain")
    return { failedBefore: before.code, passedAfter: after.code, diff, log, clean: status === "" }
  } finally {
    await rm(repo, { recursive: true, force: true })
  }
}
