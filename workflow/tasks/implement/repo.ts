import { compactEnv } from "./env"
import { run } from "./shell"
import type { Input, RepoSnapshot } from "./types"

export async function repoSnapshot(): Promise<RepoSnapshot> {
  const [head, branch, status] = await Promise.all([
    run(["git", "rev-parse", "--short", "HEAD"]),
    run(["git", "branch", "--show-current"]),
    run(["git", "status", "--short"]),
  ])
  return {
    head: head.trim(),
    branch: branch.trim() || "detached",
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
  const branch = (await run(["git", "branch", "--show-current"])).trim()
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
  const branch = await currentBranch()
  if (branch !== expectedBranch) {
    throw new Error(`${phase} changed branch from ${expectedBranch} to ${branch}`)
  }
}

export async function inferRepository(): Promise<string> {
  const envRepository = process.env.GITHUB_REPOSITORY?.trim()
  if (envRepository) return envRepository

  const remote = (await run(["git", "config", "--get", "remote.origin.url"])).trim()
  const match = remote.match(/github\.com[:/]([^/]+\/[^/.]+)(?:\.git)?$/)
  if (!match?.[1]) {
    throw new Error("payload.repository or GITHUB_REPOSITORY is required when remote.origin.url is not a GitHub repository")
  }
  return match[1]
}

export async function workingTreeDiff(): Promise<string> {
  const status = await gitStatusForCommit()
  const reviewIndex = await reviewIndexEnv()
  try {
    await run(["git", "add", "--intent-to-add", "--", ".", ":(exclude).helmr-workflow-artifacts"], {
      label: "git add --intent-to-add for review diff",
      env: reviewIndex.env,
    })
    const [stat, files, diff] = await Promise.all([
      run(["git", "diff", "--stat", "HEAD", "--", ".", ":(exclude).helmr-workflow-artifacts"], {
        env: reviewIndex.env,
      }),
      run(["git", "diff", "--name-only", "HEAD", "--", ".", ":(exclude).helmr-workflow-artifacts"], {
        env: reviewIndex.env,
      }),
      run(["git", "diff", "HEAD", "--", ".", ":(exclude).helmr-workflow-artifacts"], {
        env: reviewIndex.env,
      }),
    ])
    return [
      "## Git Status",
      "```text",
      status.trim() || "clean",
      "```",
      "",
      "## Diff Stat",
      "```text",
      stat.trim() || "no working tree diff",
      "```",
      "",
      "## Changed Files",
      "```text",
      files.trim() || "no changed files",
      "```",
      "",
      "## Diff",
      "```diff",
      truncate(diff, 60000),
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
  await run(["git", "config", "user.name", "helmr-workflow"])
  await run(["git", "config", "user.email", "workflow@helmr.dev"])
  await run(["git", "add", "-A", "--", ".", ":(exclude).helmr-workflow-artifacts"])
  await run(["git", "commit", "-m", input.prTitle, "-m", input.prBody])
}

export async function pushBranch(repository: string, headBranch: string, githubToken: string): Promise<void> {
  const askpassPath = ".helmr-git-askpass.sh"
  await Bun.write(
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
  await run(["chmod", "700", askpassPath])
  try {
    await run(["git", "remote", "set-url", "origin", `https://github.com/${repository}.git`])
    await run(["git", "push", "--force-with-lease", "origin", `HEAD:refs/heads/${headBranch}`], {
      label: `git push --force-with-lease origin HEAD:refs/heads/${headBranch}`,
      env: {
        ...compactEnv(process.env),
        GIT_ASKPASS: `${process.cwd()}/${askpassPath}`,
        GIT_TERMINAL_PROMPT: "0",
        GITHUB_TOKEN: githubToken,
      },
    })
  } finally {
    await run(["rm", "-f", askpassPath])
  }
}

function assertBranchName(value: string): void {
  if (!/^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$/.test(value) || value.includes("..") || value.endsWith("/")) {
    throw new Error(`implementation agent checked out an unsafe branch name: ${value}`)
  }
}

async function reviewIndexEnv(): Promise<{ readonly env: Record<string, string>; readonly path: string }> {
  const gitDir = (await run(["git", "rev-parse", "--git-dir"])).trim()
  const indexPath = `${gitDir}/helmr-review-index-${process.pid}-${Date.now()}`
  const env = {
    ...compactEnv(process.env),
    GIT_INDEX_FILE: indexPath,
  }
  await run(["git", "read-tree", "HEAD"], {
    label: "git read-tree HEAD for review diff",
    env,
  })
  return { env, path: indexPath }
}

async function gitStatusForCommit(): Promise<string> {
  return (await run(["git", "status", "--short", "--", ".", ":(exclude).helmr-workflow-artifacts"])).trim()
}

function truncate(value: string, max: number): string {
  if (value.length <= max) return value
  return `${value.slice(0, max)}\n... truncated ${value.length - max} characters ...`
}
