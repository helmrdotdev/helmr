import { execFileSync } from "node:child_process"
import { mkdir, mkdtemp, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { resolve } from "node:path"
import { chdir, cwd } from "node:process"
import { describe, expect, test } from "bun:test"

import { workingTreeDiff } from "../tasks/implement/repo"

describe("implementation review diff", () => {
  test("includes untracked implementation files and excludes workflow artifacts", async () => {
    const previousCwd = cwd()
    const repo = await mkdtemp(resolve(tmpdir(), "helmr-workflow-review-diff-"))
    try {
      git(repo, ["init"])
      git(repo, ["config", "user.email", "test@example.com"])
      git(repo, ["config", "user.name", "Test User"])
      await mkdir(resolve(repo, "src"), { recursive: true })
      await mkdir(resolve(repo, ".helmr-workflow-artifacts"), { recursive: true })
      await mkdir(resolve(repo, ".helmr/task-source"), { recursive: true })
      await writeFile(resolve(repo, "src/app.ts"), "export const value = 1\n")
      await writeFile(resolve(repo, ".helmr-workflow-artifacts/log.txt"), "original artifact\n")
      await writeFile(resolve(repo, ".helmr/task-source/task.ts"), "original task source\n")
      git(repo, ["add", "."])
      git(repo, ["-c", "commit.gpgsign=false", "commit", "-m", "base"])
      const baseSha = git(repo, ["rev-parse", "HEAD"]).trim()

      await writeFile(resolve(repo, "src/app.ts"), "export const value = 2\n")
      await writeFile(resolve(repo, "src/new.ts"), "export const created = true\n")
      await writeFile(resolve(repo, ".helmr-workflow-artifacts/log.txt"), "changed artifact\n")
      await writeFile(resolve(repo, ".helmr/task-source/task.ts"), "changed task source\n")

      chdir(repo)
      const diff = await workingTreeDiff(baseSha)

      expect(diff).toContain("src/app.ts")
      expect(diff).toContain("src/new.ts")
      expect(diff).toContain("export const created = true")
      expect(diff).not.toContain(".helmr-workflow-artifacts")
      expect(diff).not.toContain(".helmr/task-source")
    } finally {
      chdir(previousCwd)
    }
  })
})

function git(cwd: string, args: readonly string[]): string {
  return execFileSync("git", args, {
    cwd,
    encoding: "utf8",
    env: {
      HOME: process.env.HOME,
      LANG: process.env.LANG,
      LC_ALL: process.env.LC_ALL,
      PATH: process.env.PATH,
      TMPDIR: process.env.TMPDIR,
    },
  })
}
