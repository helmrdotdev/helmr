import { agent, computer, image, source } from "@helmr/sdk"
import { z } from "zod"

const base = image("github-pr-review")
  .from("node:24-bookworm-slim")
  .workdir("/sandbox")
  .run(["npm", "install", "-g", "bun@1.3.13"])
  .copy(source.file("package.json"), "/opt/helmr-task/package.json")
  .workdir("/opt/helmr-task")
  .run(["bun", "install"])
  .workdir("/sandbox")

export const githubPRReviewComputer = computer({
  id: "github-pr-review",
  image: base,
  resources: { cpu: 1, memory: "1GiB" },
  // Replace this example UUID with the Environment Secret ID before deployment.
  secrets: [{ secretId: "01900000-0000-7000-8000-000000000001", env: { name: "GITHUB_TOKEN", mode: "raw" } }],
})

const payload = z.object({
  owner: z.string().min(1),
  repo: z.string().min(1),
  prNumber: z.number().int().positive(),
})

interface PullRequest {
  readonly title: string
}

interface PullRequestFile {
  readonly filename: string
  readonly additions: number
  readonly deletions: number
}

export const reviewPullRequest = agent({
  computer: githubPRReviewComputer,
  id: "github-pr-review",
  maxTurnDuration: "10m",
  async turn(turn) {
    const input = payload.parse(JSON.parse(turn.input.map(part => part.text).join("")))
    const token = requireEnv("GITHUB_TOKEN")
    const target = input
    const repoPath = `${encodeURIComponent(target.owner)}/${encodeURIComponent(target.repo)}`
    const pull = await github<PullRequest>(
      token,
      `/repos/${repoPath}/pulls/${target.prNumber}`,
      { signal: turn.signal },
    )
    const files = await listPullRequestFiles(token, repoPath, target.prNumber, turn.signal)

    const summary = [
      `PR #${target.prNumber}: ${pull.title}`,
      `Files changed: ${files.length}`,
      ...files.slice(0, 10).map((file) => `- ${file.filename} (+${file.additions}/-${file.deletions})`),
    ].join("\n")

    console.info({ pullRequest: target.prNumber, filesChanged: files.length })

    return { summary, filesChanged: files.length }
  },
})

function requireEnv(name: string): string {
  const value = process.env[name]
  if (!value) {
    throw new Error(`${name} is required`)
  }
  return value
}

async function github<T>(token: string, path: string, init: RequestInit = {}): Promise<T> {
  const response = await fetch(`https://api.github.com${path}`, {
    ...init,
    headers: {
      accept: "application/vnd.github+json",
      authorization: `Bearer ${token}`,
      "content-type": "application/json",
      "x-github-api-version": "2022-11-28",
      ...init.headers,
    },
  })
  if (!response.ok) {
    throw new Error(`GitHub API ${response.status}: ${await response.text()}`)
  }
  return (await response.json()) as T
}

async function listPullRequestFiles(
  token: string,
  repoPath: string,
  prNumber: number,
  signal: AbortSignal,
): Promise<PullRequestFile[]> {
  const files: PullRequestFile[] = []
  for (let page = 1; ; page++) {
    const batch = await github<PullRequestFile[]>(
      token,
      `/repos/${repoPath}/pulls/${prNumber}/files?per_page=100&page=${page}`,
      { signal },
    )
    files.push(...batch)
    if (batch.length < 100) {
      return files
    }
  }
}
