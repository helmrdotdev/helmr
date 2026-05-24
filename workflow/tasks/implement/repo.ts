import { writeFile } from "node:fs/promises"
import { run } from "./shell"
import type { Input, RepoSnapshot } from "./types"

const gitWorkPathspec = [".", ":(exclude).helmr-workflow-artifacts", ":(exclude).helmr/task-source"] as const

export async function repoSnapshot(): Promise<RepoSnapshot> {
  const [head, baseSha, branch, status] = await Promise.all([
    run(["git", "rev-parse", "--short", "HEAD"]),
    run(["git", "rev-parse", "HEAD"]),
    readCurrentBranch(),
    gitStatusForCommit(),
  ])
  return {
    head: head.trim(),
    baseSha: baseSha.trim(),
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

export async function inferRepository(): Promise<string> {
  const envRepository = process.env.GITHUB_REPOSITORY?.trim()
  if (envRepository) {
    assertRepositoryName(envRepository)
    return envRepository
  }

  const remote = (await run(["git", "config", "--get", "remote.origin.url"])).trim()
  const match = remote.match(/github\.com[:/]([^/]+\/[^/.]+)(?:\.git)?$/)
  if (!match?.[1]) {
    throw new Error("payload.repository or GITHUB_REPOSITORY is required when remote.origin.url is not a GitHub repository")
  }
  assertRepositoryName(match[1])
  return match[1]
}

export async function prepareGitWorkspace(input: Input, githubToken: string): Promise<string> {
  const explicitRepository = input.repository?.trim() || process.env.GITHUB_REPOSITORY?.trim()
  const ref = input.ref?.trim() || input.baseBranch
  assertGitRef(ref)

  if (await hasGitWorkspace()) {
    const repository = explicitRepository ?? await inferRepository()
    assertRepositoryName(repository)
    await withGitAskpass(githubToken, async (env) => {
      await configureOrigin(repository, env)
      await checkoutRef(ref, env)
    })
    return repository
  }

  if (!explicitRepository) {
    throw new Error("payload.repository or GITHUB_REPOSITORY is required when the workspace does not include Git metadata")
  }
  assertRepositoryName(explicitRepository)

  const checkoutPath = `${process.cwd()}/.helmr-workflow-checkout`
  try {
    await run(["test", "!", "-e", checkoutPath], {
      label: `test ! -e ${checkoutPath}`,
      env: gitOperationEnv(),
    })
  } catch {
    throw new Error(`refusing to overwrite existing checkout directory: ${checkoutPath}`)
  }

  await withGitAskpass(githubToken, async (env) => {
    await run(["git", "clone", "--no-checkout", `https://github.com/${explicitRepository}.git`, checkoutPath], {
      label: `git clone --no-checkout https://github.com/${explicitRepository}.git ${checkoutPath}`,
      env,
    })
    await checkoutRef(ref, env, checkoutPath)
  })
  process.chdir(checkoutPath)
  return explicitRepository
}

export async function workingTreeDiff(baseSha: string): Promise<string> {
  assertSha(baseSha)
  await assertNoSecretLikeChanges("review")
  const status = await gitStatusForCommit()
  const reviewIndex = await reviewIndexEnv(baseSha)
  const maxDiffChars = 60000
  try {
    await run(["git", "add", "--intent-to-add", "--", ...gitWorkPathspec], {
      label: "git add --intent-to-add for review diff",
      env: reviewIndex.env,
    })
    const [stat, files, diff] = await Promise.all([
      run(["git", "diff", "--no-ext-diff", "--stat", baseSha, "--", ...gitWorkPathspec], {
        env: reviewIndex.env,
      }),
      run(["git", "diff", "--no-ext-diff", "--name-only", baseSha, "--", ...gitWorkPathspec], {
        env: reviewIndex.env,
      }),
      run(["git", "diff", "--no-ext-diff", baseSha, "--", ...gitWorkPathspec], {
        env: reviewIndex.env,
      }),
    ])
    if (diff.length > maxDiffChars) {
      throw new Error(`working tree diff is ${diff.length} characters, exceeding review limit ${maxDiffChars}`)
    }
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
      "```diff",
      diff,
      "```",
    ].join("\n")
  } finally {
    await run(["rm", "-f", reviewIndex.path, `${reviewIndex.path}.lock`])
  }
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
  await run(["git", "add", "-A", "--", ...gitWorkPathspec], { env })
  await run(["git", "-c", "core.hooksPath=/dev/null", "commit", "-m", input.prTitle, "-m", input.prBody], {
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

function assertGitRef(value: string): void {
  if (!value || value.includes("\0") || value.startsWith("-")) {
    throw new Error(`expected a safe git ref, received: ${value}`)
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
  const remoteUrl = `https://github.com/${repository}.git`
  try {
    await run(["git", "remote", "set-url", "origin", remoteUrl], { env })
  } catch {
    await run(["git", "remote", "add", "origin", remoteUrl], { env })
  }
}

async function checkoutRef(ref: string, env: Record<string, string>, cwd?: string): Promise<void> {
  const git = cwd === undefined ? ["git"] : ["git", "-C", cwd]
  await run([...git, "fetch", "--depth=1", "origin", ref], {
    label: `git fetch --depth=1 origin ${ref}`,
    env,
  })
  await run([...git, "checkout", "--detach", "FETCH_HEAD"], {
    label: "git checkout --detach FETCH_HEAD",
    env,
  })
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
