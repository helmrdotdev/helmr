import { compactEnv } from "./env"
import { run } from "./shell"
import type { Input, RepoSnapshot } from "./types"

export async function ensureBranch(targetBranch: string): Promise<void> {
  await run(["git", "checkout", "-B", targetBranch], { label: `git checkout -B ${targetBranch}` })
}

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
  const [stat, files, diff] = await Promise.all([
    run(["git", "diff", "--stat"]),
    run(["git", "diff", "--name-only"]),
    run(["git", "diff", "--", "."]),
  ])
  return [
    "## Diff Stat",
    "```text",
    stat.trim() || "no unstaged diff",
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
}

export async function commitChanges(input: Input): Promise<void> {
  const status = (await run(["git", "status", "--short"])).trim()
  if (!status) {
    throw new Error("Cursor completed but the working tree has no changes to commit")
  }
  await run(["git", "config", "user.name", "helmr-workflow"])
  await run(["git", "config", "user.email", "workflow@helmr.dev"])
  await run(["git", "add", "-A", "--", ".", ":(exclude).helmr-workflow-artifacts"])
  await run(["git", "commit", "-m", input.prTitle, "-m", input.prBody])
}

export async function pushBranch(repository: string, targetBranch: string, githubToken: string): Promise<void> {
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
    await run(["git", "push", "--force-with-lease", "origin", `HEAD:refs/heads/${targetBranch}`], {
      label: `git push --force-with-lease origin HEAD:refs/heads/${targetBranch}`,
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

function truncate(value: string, max: number): string {
  if (value.length <= max) return value
  return `${value.slice(0, max)}\n... truncated ${value.length - max} characters ...`
}
