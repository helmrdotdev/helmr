import { writeFile } from "node:fs/promises"
import type { GitHubTaskSource, TaskContext } from "@helmr/sdk"
import { run } from "./shell"
import { requireGitHubSource, type Input, type RepoSnapshot } from "./types"

const gitWorkPathspec = [".", ":(exclude).helmr-workflow-artifacts", ":(exclude).helmr/task-source"] as const
const gitWorkPathspecShell = ". ':(exclude).helmr-workflow-artifacts' ':(exclude).helmr/task-source'"

export async function repoSnapshot(baseSha: string): Promise<RepoSnapshot> {
  assertSha(baseSha)
  const head = (await run(["git", "rev-parse", "HEAD"])).trim()
  if (head !== baseSha) {
    throw new Error(`workspace HEAD ${head} does not match source resolvedSha ${baseSha}`)
  }
  const [shortHead, branch, status] = await Promise.all([
    run(["git", "rev-parse", "--short", "HEAD"]),
    readCurrentBranch(),
    gitStatusForCommit(),
  ])
  return {
    head: shortHead.trim(),
    baseSha,
    branch,
    status: status.trim(),
  }
}

export function assertCleanSnapshot(repo: RepoSnapshot, phase: string): void {
  if (repo.status) {
    throw new Error(`${phase} requires a clean workspace before agent work starts:\n${repo.status}`)
  }
}

export async function assertCleanWorkspace(phase: string): Promise<void> {
  const status = await gitStatusForCommit()
  if (status) {
    throw new Error(`${phase} must not change the workspace:\n${status}`)
  }
}

export async function currentBranch(opts: { readonly previousBranch?: string } = {}): Promise<string> {
  const branch = await readCurrentBranch()
  if (!branch) {
    throw new Error("implementation agent must checkout a named branch before committing")
  }
  assertBranchName(branch)
  if (opts.previousBranch && opts.previousBranch !== "detached" && branch === opts.previousBranch) {
    throw new Error(`implementation agent must checkout a new branch before editing; still on ${branch}`)
  }
  return branch
}

export async function assertCurrentBranch(expectedBranch: string, phase: string): Promise<void> {
  const branch = await readCurrentBranch()
  if (branch !== expectedBranch) {
    throw new Error(`${phase} changed branch from ${expectedBranch} to ${branch}`)
  }
}

export async function assertHeadContainsBase(baseSha: string, phase: string): Promise<void> {
  assertSha(baseSha)
  try {
    await run(["git", "merge-base", "--is-ancestor", baseSha, "HEAD"], {
      label: `git merge-base --is-ancestor ${baseSha} HEAD`,
    })
  } catch (error: unknown) {
    throw new Error(`${phase} must keep base commit ${baseSha} as an ancestor of HEAD`)
  }
}

export async function assertHeadEqualsBase(baseSha: string, phase: string): Promise<void> {
  assertSha(baseSha)
  const head = (await run(["git", "rev-parse", "HEAD"])).trim()
  if (head !== baseSha) {
    throw new Error(`${phase} must not create commits before the workflow-owned commit; expected HEAD ${baseSha}, found ${head}`)
  }
}

export async function prepareGitWorkspace(ctx: TaskContext, githubToken: string): Promise<GitHubTaskSource> {
  const source = requireGitHubSource(ctx)
  process.chdir(ctx.workspace.projectPath)

  if (await hasGitWorkspace()) {
    await withGitAskpass(githubToken, async (env) => {
      await configureOrigin(source.repository, env)
      await fetchResolvedSha(source.resolvedSha, env)
      await checkoutResolvedSha(source.resolvedSha, env)
    })
    return source
  }

  const checkoutPath = ctx.workspace.projectPath
  await withGitAskpass(githubToken, async (env) => {
    await run(["git", "init"], { env, label: "git init" })
    await configureOrigin(source.repository, env)
    await fetchResolvedSha(source.resolvedSha, env)
    await checkoutResolvedSha(source.resolvedSha, env)
  })
  return source
}

export async function workingTreeDiff(baseSha: string): Promise<string> {
  assertSha(baseSha)
  await assertNoSecretLikeChanges("review")
  const status = await gitStatusForCommit()
  const reviewIndex = await reviewIndexEnv(baseSha)
  const maxDiffChars = 60000
  try {
    await addUntrackedFilesForReviewDiff(reviewIndex.env)
    const [stat, files, diffSize, diff] = await Promise.all([
      run(["git", "diff", "--no-ext-diff", "--stat", baseSha, "--", ...gitWorkPathspec], {
        env: reviewIndex.env,
      }),
      run(["git", "diff", "--no-ext-diff", "--name-only", baseSha, "--", ...gitWorkPathspec], {
        env: reviewIndex.env,
      }),
      limitedGitDiffSize(baseSha, reviewIndex.env),
      limitedGitDiff(baseSha, maxDiffChars, reviewIndex.env),
    ])
    const truncated = diffSize > maxDiffChars
    return [
      "## Git Status",
      "```text",
      status.trim() || "clean",
      "```",
      "",
      "## Diff Stat",
      "```text",
      stat.trim() || "no changes since base",
      "```",
      "",
      "## Changed Files",
      "```text",
      files.trim() || "no files changed since base",
      "```",
      "",
      "## Diff",
      truncated
        ? [
            `Diff is ${diffSize} characters, exceeding the inline review budget ${maxDiffChars}.`,
            "The diff below is truncated. Reviewers must use the changed-file list and inspect repository files directly when needed.",
          ].join("\n")
        : "",
      "```diff",
      diff,
      truncated ? "\n... diff truncated ..." : "",
      "```",
    ].join("\n")
  } finally {
    await run(["rm", "-f", reviewIndex.path, `${reviewIndex.path}.lock`])
  }
}

async function limitedGitDiffSize(baseSha: string, env: Record<string, string>): Promise<number> {
  const output = await run([
    "sh",
    "-ceu",
    `git diff --no-ext-diff "$1" -- ${gitWorkPathspecShell} | wc -c`,
    "sh",
    baseSha,
  ], {
    label: "git diff byte count",
    env,
  })
  const size = Number.parseInt(output.trim(), 10)
  if (!Number.isFinite(size)) {
    throw new Error(`failed to parse git diff byte count: ${output.trim()}`)
  }
  return size
}

async function limitedGitDiff(baseSha: string, maxDiffChars: number, env: Record<string, string>): Promise<string> {
  return run([
    "sh",
    "-ceu",
    `git diff --no-ext-diff "$1" -- ${gitWorkPathspecShell} | head -c "$2"`,
    "sh",
    baseSha,
    String(maxDiffChars),
  ], {
    label: "limited git diff",
    env,
  })
}

async function addUntrackedFilesForReviewDiff(env: Record<string, string>): Promise<void> {
  const output = await run([
    "git",
    "ls-files",
    "--others",
    "--exclude-standard",
    "-z",
    "--",
    ...gitWorkPathspec,
  ])
  const paths = output.split("\0").filter(Boolean)
  if (paths.length === 0) return

  await run(["git", "add", "--intent-to-add", "--", ...paths], {
    label: "git add --intent-to-add untracked files for review diff",
    env,
  })
}

export async function commitChanges(input: Input): Promise<void> {
  const status = await gitStatusForCommit()
  if (!status) {
    throw new Error("Cursor completed but the working tree has no changes to commit")
  }
  await assertNoSecretLikeChanges("commit")
  const env = gitOperationEnv()
  await run(["git", "config", "user.name", "helmr-workflow"], { env })
  await run(["git", "config", "user.email", "workflow@helmr.dev"], { env })
  await stageChangesForCommit(env)
  await run(["git", "-c", "core.hooksPath=/dev/null", "commit", "-m", input.prTitle, "-m", input.prBody], {
    env,
  })
}

async function stageChangesForCommit(env: Record<string, string>): Promise<void> {
  await run(["git", "add", "-u", "--", ...gitWorkPathspec], {
    label: "git add tracked changes for commit",
    env,
  })

  const output = await run([
    "git",
    "ls-files",
    "--others",
    "--exclude-standard",
    "-z",
    "--",
    ...gitWorkPathspec,
  ])
  const paths = output.split("\0").filter(Boolean)
  if (paths.length === 0) return

  await run(["git", "add", "--", ...paths], {
    label: "git add untracked files for commit",
    env,
  })
}

export async function pushBranch(repository: string, headBranch: string, githubToken: string): Promise<void> {
  assertRepositoryName(repository)
  assertBranchName(headBranch)
  await withGitAskpass(githubToken, async (env) => {
    await run(["git", "remote", "set-url", "origin", `https://github.com/${repository}.git`], {
      env: gitOperationEnv(),
    })
    if (await remoteBranchExists(headBranch, env)) {
      throw new Error(`refusing to overwrite existing remote branch: ${headBranch}`)
    }
    await run(["git", "-c", "core.hooksPath=/dev/null", "push", "origin", `HEAD:refs/heads/${headBranch}`], {
      label: `git push origin HEAD:refs/heads/${headBranch}`,
      env,
    })
  })
}

function assertBranchName(value: string): void {
  if (!/^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$/.test(value) || value.includes("..") || value.endsWith("/")) {
    throw new Error(`implementation agent checked out an unsafe branch name: ${value}`)
  }
  if (!value.startsWith("helmr/")) {
    throw new Error(`implementation agent must checkout a helmr/... branch, received: ${value}`)
  }
  if (/^helmr\/(?:main|master|develop|dev|trunk|release|hotfix|production|prod|staging)(?:\/|$)/i.test(value)) {
    throw new Error(`implementation agent checked out a protected/shared branch name: ${value}`)
  }
}

function assertRepositoryName(value: string): void {
  if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(value)) {
    throw new Error(`expected GitHub repository in owner/name form, received: ${value}`)
  }
}

async function fetchResolvedSha(sha: string, env: Record<string, string>): Promise<void> {
  assertSha(sha)
  await run(["git", "fetch", "--depth=1", "origin", sha], {
    label: `git fetch --depth=1 origin ${sha}`,
    env,
  })
}

async function checkoutResolvedSha(sha: string, env: Record<string, string>): Promise<void> {
  assertSha(sha)
  await run(["git", "checkout", "--detach", sha], {
    label: `git checkout --detach ${sha}`,
    env,
  })
  const head = (await run(["git", "rev-parse", "HEAD"], { env })).trim()
  if (head !== sha) {
    throw new Error(`git checkout resolved to ${head}, expected ${sha}`)
  }
}

async function reviewIndexEnv(baseSha: string): Promise<{ readonly env: Record<string, string>; readonly path: string }> {
  const gitDir = (await run(["git", "rev-parse", "--git-dir"])).trim()
  const indexPath = `${gitDir}/helmr-review-index-${process.pid}-${Date.now()}`
  const env = {
    ...gitOperationEnv(),
    GIT_INDEX_FILE: indexPath,
  }
  await run(["git", "read-tree", baseSha], {
    label: `git read-tree ${baseSha} for review diff`,
    env,
  })
  return { env, path: indexPath }
}

async function readCurrentBranch(): Promise<string> {
  return (await run(["git", "branch", "--show-current"])).trim() || "detached"
}

async function hasGitWorkspace(): Promise<boolean> {
  try {
    await run(["git", "rev-parse", "--is-inside-work-tree"], { env: gitOperationEnv() })
    return true
  } catch {
    return false
  }
}

function assertSha(value: string): void {
  if (!/^[0-9a-f]{40}$/i.test(value)) {
    throw new Error(`expected a full git SHA, received: ${value}`)
  }
}

async function gitStatusForCommit(): Promise<string> {
  return (await run(["git", "status", "--short", "--", ...gitWorkPathspec])).trim()
}

async function assertNoSecretLikeChanges(phase: string): Promise<void> {
  const paths = await changedPathsForCommit()
  const blocked = paths.filter(isSecretLikePath)
  if (blocked.length > 0) {
    throw new Error(`${phase} blocked secret-like changed files:\n${blocked.join("\n")}`)
  }
}

async function changedPathsForCommit(): Promise<readonly string[]> {
  const output = await run([
    "git",
    "status",
    "--porcelain=v1",
    "-z",
    "--untracked-files=all",
    "--",
    ...gitWorkPathspec,
  ])
  const entries = output.split("\0")
  const paths: string[] = []
  for (let i = 0; i < entries.length; i += 1) {
    const entry = entries[i]
    if (!entry) continue
    const status = entry.slice(0, 2)
    const path = entry.slice(3)
    if (path) paths.push(path)
    if (status.includes("R") || status.includes("C")) {
      const originalPath = entries[++i]
      if (originalPath) paths.push(originalPath)
    }
  }
  return paths
}

function isSecretLikePath(path: string): boolean {
  if (path === ".helmr-workflow-artifacts" || path.startsWith(".helmr-workflow-artifacts/")) {
    return false
  }
  return path.split("/").some((segment) => segment.startsWith(".env") || segment.startsWith(".helmr"))
}

function gitOperationEnv(): Record<string, string> {
  const env: Record<string, string> = {}
  for (const key of ["HOME", "PATH", "TMPDIR", "USER", "LOGNAME", "LANG", "LC_ALL"]) {
    const value = process.env[key]
    if (typeof value === "string") {
      env[key] = value
    }
  }
  return env
}

async function remoteBranchExists(headBranch: string, env: Record<string, string>): Promise<boolean> {
  try {
    await run(["git", "ls-remote", "--exit-code", "--heads", "origin", `refs/heads/${headBranch}`], {
      label: `git ls-remote --heads origin refs/heads/${headBranch}`,
      env,
    })
    return true
  } catch (error: unknown) {
    if (error instanceof Error && error.message.includes("exited 2:")) {
      return false
    }
    throw error
  }
}

async function configureOrigin(repository: string, env: Record<string, string>): Promise<void> {
  assertRepositoryName(repository)
  const remoteUrl = `https://github.com/${repository}.git`
  try {
    await run(["git", "remote", "set-url", "origin", remoteUrl], { env })
  } catch {
    await run(["git", "remote", "add", "origin", remoteUrl], { env })
  }
}

async function withGitAskpass<T>(githubToken: string, operation: (env: Record<string, string>) => Promise<T>): Promise<T> {
  const askpassPath = ".helmr-git-askpass.sh"
  await writeFile(
    askpassPath,
    [
      "#!/bin/sh",
      "case \"$1\" in",
      "*Username*) printf '%s\\n' 'x-access-token' ;;",
      "*) printf '%s\\n' \"$GITHUB_TOKEN\" ;;",
      "esac",
      "",
    ].join("\n"),
  )
  await run(["chmod", "700", askpassPath], { env: gitOperationEnv() })
  try {
    return await operation({
      ...gitOperationEnv(),
      GIT_ASKPASS: `${process.cwd()}/${askpassPath}`,
      GIT_TERMINAL_PROMPT: "0",
      GITHUB_TOKEN: githubToken,
    })
  } finally {
    await run(["rm", "-f", askpassPath], { env: gitOperationEnv() })
  }
}
