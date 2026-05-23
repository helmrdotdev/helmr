import { mkdir } from "node:fs/promises"
import type { Input, RepoSnapshot, ReviewRound } from "./types"

const artifactDir = ".helmr-workflow-artifacts"

export function artifactPath(path: string): string {
  return `${artifactDir}/${path}`
}

export function artifacts(includeResult: boolean): string[] {
  return [
    artifactPath("00-feature-design.md"),
    artifactPath("01-exploration.md"),
    artifactPath("02-claude-plan.md"),
    artifactPath("03-codex-plan-review.md"),
    artifactPath("04-cursor-implementation.md"),
    artifactPath("05-review-loop.md"),
    includeResult ? artifactPath("implementation-result.json") : "",
  ].filter(Boolean)
}

export function renderFeatureDesign(input: Input, repo: RepoSnapshot, repository: string, runId: string): string {
  return [
    "# Feature Design",
    "",
    `Run: ${runId}`,
    `Repository: ${repository}`,
    `Base branch: ${input.baseBranch}`,
    `PR title: ${input.prTitle}`,
    `Cursor model: ${input.cursorModel}`,
    `Review rounds: ${input.maxReviewRounds}`,
    "",
    "## Workspace",
    "",
    `Branch: ${repo.branch}`,
    `HEAD: ${repo.head}`,
    "",
    "## Initial Status",
    "",
    "```text",
    repo.status || "clean",
    "```",
    "",
    "## Design Payload",
    "",
    input.featureDesign,
    "",
  ].join("\n")
}

export function renderReviewLoop(rounds: readonly ReviewRound[]): string {
  const sections = rounds.map((round) => [
    `## Round ${round.round}`,
    "",
    "### Codex Review",
    "",
    round.codexReview,
    "",
    "### Claude Review",
    "",
    round.claudeReview,
    "",
    "### Codex Triage",
    "",
    JSON.stringify(round.codexTriage, null, 2),
    "",
    `Actionable findings: ${round.codexTriage.findings.length}`,
    "",
    round.cursorFix ? "### Cursor Fix" : "",
    round.cursorFix ? "" : "",
    round.cursorFix ?? "",
  ].filter((line) => line !== "").join("\n"))

  return ["# Review Loop", "", ...sections, ""].join("\n")
}

export async function writeMarkdown(path: string, value: string): Promise<void> {
  await mkdir(artifactDir, { recursive: true })
  await Bun.write(artifactPath(path), value.endsWith("\n") ? value : `${value}\n`)
}

export async function writeJson(path: string, value: unknown): Promise<void> {
  await mkdir(artifactDir, { recursive: true })
  await Bun.write(artifactPath(path), `${JSON.stringify(value, null, 2)}\n`)
}
