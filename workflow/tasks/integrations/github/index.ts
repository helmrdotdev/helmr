import type { GitHubTaskSource } from "@helmr/sdk"
import type { Input, PullRequest } from "../types"

interface GitHubPullRequest {
  readonly html_url: string
  readonly number: number
}

export async function createOrFindPullRequest(
  token: string,
  source: GitHubTaskSource,
  input: Input,
  headBranch: string,
): Promise<PullRequest> {
  const repository = source.repository
  const baseBranch = resolvePullRequestBase(source, input.prBaseBranch)
  const [owner] = repository.split("/")
  if (!owner) throw new Error(`Invalid repository: ${repository}`)

  const search = new URLSearchParams({
    state: "open",
    head: `${owner}:${headBranch}`,
    base: baseBranch,
    per_page: "1",
  })
  const existing = await github<GitHubPullRequest[]>(
    token,
    `/repos/${repository}/pulls?${search.toString()}`,
  )
  if (existing[0]) {
    return { html_url: existing[0].html_url, number: existing[0].number }
  }

  const created = await github<GitHubPullRequest>(token, `/repos/${repository}/pulls`, {
    method: "POST",
    body: JSON.stringify({
      title: input.prTitle,
      head: headBranch,
      base: baseBranch,
      body: input.prBody,
      draft: true,
    }),
  })
  return { html_url: created.html_url, number: created.number }
}

export function resolvePullRequestBase(source: GitHubTaskSource, prBaseBranch?: string): string {
  if (source.pullRequest?.baseRef) {
    return source.pullRequest.baseRef
  }
  if (source.refKind === "branch" && source.refName) {
    return source.refName
  }
  const trimmed = prBaseBranch?.trim()
  if (trimmed) {
    return trimmed
  }
  const kind = source.refKind ?? "unknown"
  throw new Error(`payload.prBaseBranch is required when run source ref kind is ${kind}`)
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
