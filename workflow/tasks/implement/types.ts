export type FeatureDesign = string | Record<string, unknown>

export interface Payload {
  readonly featureDesign?: FeatureDesign
  readonly repository?: string
  readonly targetBranch?: string
  readonly baseBranch?: string
  readonly prTitle?: string
  readonly prBody?: string
  readonly maxReviewRounds?: number
  readonly claudeModel?: string
  readonly codexModel?: string
  readonly cursorModel?: string
}

export interface Input {
  readonly featureDesign: string
  readonly repository?: string
  readonly targetBranch: string
  readonly baseBranch: string
  readonly prTitle: string
  readonly prBody: string
  readonly maxReviewRounds: number
  readonly claudeModel?: string
  readonly codexModel?: string
  readonly cursorModel: string
}

export interface AuthSecrets {
  readonly openaiApiKey: string
  readonly cursorApiKey: string
  readonly githubToken: string
}

export interface RepoSnapshot {
  readonly head: string
  readonly branch: string
  readonly status: string
}

export interface ReviewRound {
  readonly round: number
  readonly codexReview: string
  readonly claudeReview: string
  readonly codexTriage: TriageResult
  readonly cursorFix?: string
}

export interface TriageFinding {
  readonly title: string
  readonly severity: "critical" | "high" | "medium" | "low"
  readonly details: string
  readonly files: readonly string[]
  readonly remediation: string
}

export interface TriageResult {
  readonly summary: string
  readonly findings: readonly TriageFinding[]
}

export interface PullRequest {
  readonly html_url: string
  readonly number: number
}

export function normalizePayload(payload: Payload): Input {
  if (!payload.featureDesign) {
    throw new Error("payload.featureDesign is required")
  }
  const featureDesign = formatFeatureDesign(payload.featureDesign)
  const title = firstLine(featureDesign)
  const targetBranch = payload.targetBranch?.trim() || `helmr/${slugify(title)}`
  assertBranchName(targetBranch)

  return {
    featureDesign,
    repository: payload.repository?.trim() || undefined,
    targetBranch,
    baseBranch: payload.baseBranch?.trim() || "main",
    prTitle: payload.prTitle?.trim() || title,
    prBody: payload.prBody?.trim() || [
      "Created by the Helmr dogfooding implementation workflow.",
      "",
      "Feature design:",
      "",
      featureDesign,
    ].join("\n"),
    maxReviewRounds: clampInteger(payload.maxReviewRounds ?? 3, 1, 10),
    claudeModel: payload.claudeModel?.trim() || undefined,
    codexModel: payload.codexModel?.trim() || undefined,
    cursorModel: payload.cursorModel?.trim() || "composer-2.5",
  }
}

function formatFeatureDesign(value: FeatureDesign): string {
  if (typeof value === "string") {
    const trimmed = value.trim()
    if (!trimmed) throw new Error("payload.featureDesign must not be empty")
    return trimmed
  }
  return JSON.stringify(value, null, 2)
}

function firstLine(value: string): string {
  return value.split("\n").map((line) => line.trim()).find(Boolean) ?? "Implement feature"
}

function slugify(value: string): string {
  const slug = value
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 64)
  return slug || "feature"
}

function assertBranchName(value: string): void {
  if (!/^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$/.test(value) || value.includes("..") || value.endsWith("/")) {
    throw new Error("payload.targetBranch contains an unsafe branch name")
  }
}

function clampInteger(value: number, min: number, max: number): number {
  if (!Number.isInteger(value)) {
    throw new Error("payload.maxReviewRounds must be an integer")
  }
  return Math.min(Math.max(value, min), max)
}
