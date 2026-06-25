import { cache, image, logger, sandbox, source, task, tokens } from "@helmr/sdk"
import { z } from "zod"

const base = image("github-pr-review")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .copy("/opt/helmr-task/package.json", source.file("package.json"))
  .workdir("/opt/helmr-task")
  .run(["bun", "install"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("github-pr-review-bun") }],
  })
  .workdir("/workspace")

const sbx = sandbox("github-pr-review")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

const payload = z.object({
  owner: z.string().optional(),
  repo: z.string().optional(),
  prNumber: z.number().int().positive(),
})

type Payload = z.infer<typeof payload>

interface PullRequest {
  readonly title: string
}

interface PullRequestFile {
  readonly filename: string
  readonly additions: number
  readonly deletions: number
}

export const reviewPullRequest = task({
  id: "github-pr-review",
  sandbox: sbx,
  maxDuration: 600,
  secrets: [{ name: "GITHUB_TOKEN", env: "GITHUB_TOKEN" }],
  payload,
  run: async (payload, ctx) => {
    const token = requireEnv("GITHUB_TOKEN")
    const target = resolveTarget(payload)
    const repoPath = `${encodeURIComponent(target.owner)}/${encodeURIComponent(target.repo)}`
    const pull = await github<PullRequest>(
      token,
      `/repos/${repoPath}/pulls/${target.prNumber}`,
    )
    const files = await listPullRequestFiles(token, repoPath, target.prNumber)

    const summary = [
      `PR #${target.prNumber}: ${pull.title}`,
      `Files changed: ${files.length}`,
      ...files.slice(0, 10).map((file) => `- ${file.filename} (+${file.additions}/-${file.deletions})`),
    ].join("\n")

    logger.info({ pullRequest: target.prNumber, filesChanged: files.length })

    const approvalToken = await tokens.create({ timeout: "10m" })
    const decision = await approvalToken.wait({
      schema: z.object({ approved: z.boolean() }),
      metadata: { prompt: "Post this review summary?", summary },
    }).unwrap()
    if (!decision.approved) {
      return { status: "skipped" }
    }

    await github(token, `/repos/${repoPath}/issues/${target.prNumber}/comments`, {
      method: "POST",
      body: JSON.stringify({ body: `Helmr review summary:\n\n${summary}` }),
    })
    return { status: "commented", filesChanged: files.length }
  },
})

function requireEnv(name: string): string {
  const value = process.env[name]
  if (!value) {
    throw new Error(`${name} is required`)
  }
  return value
}

function resolveTarget(payload: Payload): Required<Payload> {
  if (payload.owner && payload.repo) {
    return payload as Required<Payload>
  }
  const [owner, repo] = process.env.GITHUB_REPOSITORY?.split("/") ?? []
  if (!owner || !repo) {
    throw new Error("owner/repo payload fields or GITHUB_REPOSITORY are required")
  }
  return { owner, repo, prNumber: payload.prNumber }
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
): Promise<PullRequestFile[]> {
  const files: PullRequestFile[] = []
  for (let page = 1; ; page++) {
    const batch = await github<PullRequestFile[]>(
      token,
      `/repos/${repoPath}/pulls/${prNumber}/files?per_page=100&page=${page}`,
    )
    files.push(...batch)
    if (batch.length < 100) {
      return files
    }
  }
}
