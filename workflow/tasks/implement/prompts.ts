import type { Input, RepoSnapshot, TriageResult } from "./types"

const secretInstruction = "Do not inspect or expose secrets, .env files, .helmr* files, or API keys."

export function renderClaudePlanPrompt(input: Input, repo: RepoSnapshot): string {
  return [
    "You are Claude in a Helmr dogfooding coding workflow.",
    "Create an implementation plan from the feature design payload. Do not modify files.",
    secretInstruction,
    "",
    "Plan requirements:",
    "- name likely files and modules to inspect",
    "- describe implementation steps",
    "- describe validation commands",
    "- identify risks and open questions",
    "",
    `Repository branch: ${repo.branch}`,
    `Repository HEAD: ${repo.head}`,
    "",
    "Feature design:",
    input.featureDesign,
  ].join("\n")
}

export function renderCodexPlanPrompt(input: Input, claudePlan: string): string {
  return [
    "You are Codex reviewing Claude's implementation plan before Cursor implements it.",
    "Do not modify files. Return concrete corrections, missing checks, and any constraints Cursor must follow.",
    secretInstruction,
    "",
    "Feature design:",
    input.featureDesign,
    "",
    "Claude plan:",
    claudePlan,
  ].join("\n")
}

export function renderCursorImplementationPrompt(input: Input, claudePlan: string, codexPlan: string): string {
  return [
    "Implement the feature in this repository.",
    "Use the feature design as the source of truth, follow Claude's plan, and apply Codex's corrections.",
    secretInstruction,
    "After editing, run the relevant validation commands and summarize changed files and validation results.",
    "",
    "Feature design:",
    input.featureDesign,
    "",
    "Claude plan:",
    claudePlan,
    "",
    "Codex plan review:",
    codexPlan,
  ].join("\n")
}

export function renderCodexReviewPrompt(input: Input, round: number, diff: string): string {
  return renderReviewPrompt("Codex", input, round, diff)
}

export function renderClaudeReviewPrompt(input: Input, round: number, diff: string): string {
  return renderReviewPrompt("Claude", input, round, diff)
}

export function renderCodexTriagePrompt(
  input: Input,
  round: number,
  codexReview: string,
  claudeReview: string,
): string {
  return [
    `Triage review round ${round}.`,
    "Merge Codex and Claude review output into the JSON schema.",
    "Include only actionable findings that Cursor must fix before a PR is created.",
    "If there are no actionable findings, return an empty findings array.",
    "",
    "Feature design:",
    input.featureDesign,
    "",
    "Codex review:",
    codexReview,
    "",
    "Claude review:",
    claudeReview,
  ].join("\n")
}

export function renderCursorFixPrompt(input: Input, round: number, triage: TriageResult): string {
  return [
    `Fix review round ${round} findings.`,
    "Use Cursor Composer 2.5 behavior through Cursor Agent CLI.",
    secretInstruction,
    "After editing, run relevant validation and summarize changed files and validation results.",
    "",
    "Feature design:",
    input.featureDesign,
    "",
    "Findings to fix:",
    JSON.stringify(triage.findings, null, 2),
  ].join("\n")
}

function renderReviewPrompt(agent: "Claude" | "Codex", input: Input, round: number, diff: string): string {
  return [
    `Review round ${round}.`,
    `You are ${agent} reviewing the current implementation diff.`,
    "Find only actionable correctness, reliability, security, maintainability, or test coverage issues.",
    `Do not modify files. ${secretInstruction}`,
    "",
    "Feature design:",
    input.featureDesign,
    "",
    diff,
  ].join("\n")
}
