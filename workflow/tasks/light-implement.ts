import { cache, image, sandbox, source, task } from "@helmr/sdk"
import { runClaude, runCodexJson, runCursor, triageSchema } from "./implement/agents"
import { artifactPath, writeJson, writeMarkdown } from "./implement/artifacts"
import { createOrFindPullRequest, resolvePullRequestBase } from "./implement/github"
import { renderCursorFixPrompt } from "./implement/prompts"
import {
  assertCleanSnapshot,
  assertCurrentBranch,
  assertHeadContainsBase,
  assertHeadEqualsBase,
  commitChanges,
  currentBranch,
  prepareGitWorkspace,
  pushBranch,
  repoSnapshot,
  workingTreeDiff,
} from "./implement/repo"
import {
  CLAUDE_REVIEW_MAX_TURNS,
  DEFAULT_CLAUDE_MODEL,
  DEFAULT_CODEX_MODEL,
  DEFAULT_CURSOR_MODEL,
} from "./models"
import {
  normalizePayload,
  requireGitHubSource,
  type FeatureDesign,
  type Input,
  type Payload,
  type RepoSnapshot,
  type TriageResult,
} from "./implement/types"

const dependencyInputs = source.directory(".", {
  ignore: ["*", "!package.json", "!bun.lock", "!tsconfig.json"],
})

const base = image("helmr-light-implementation-workflow")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .copy("/workspace", dependencyInputs)
  .run([
    "sh",
    "-ceu",
    [
      "apt-get update",
      "apt-get install -y --no-install-recommends ca-certificates git ripgrep python3 make g++",
      "rm -rf /var/lib/apt/lists/*",
    ].join(" && "),
  ])
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .run(["bun", "install", "--frozen-lockfile"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("light-implementation-workflow-bun") }],
  })

const sbx = sandbox("helmr-light-implementation-workflow")
  .image(base)
  .resources({ cpu: 2, memory: "4Gi" })

type LightPayload = Omit<Payload, "operatorInput" | "operatorInputTimeout" | "maxOperatorQuestionsPerPhase">

interface LightReviewRound {
  readonly round: number
  readonly subagentReview: string
  readonly codexTriage: TriageResult
  readonly cursorFix?: string
}

export const lightImplement = task({
  id: "light-implement",
  sandbox: sbx,
  maxDuration: 7200,
  secrets: {
    ANTHROPIC_API_KEY: { env: "ANTHROPIC_API_KEY" },
    OPENAI_API_KEY: { env: "OPENAI_API_KEY" },
    CURSOR_API_KEY: { env: "CURSOR_API_KEY" },
    GITHUB_TOKEN: { env: "GITHUB_TOKEN" },
  },
  run: async (payload: LightPayload, ctx) => {
    const input = normalizeLightPayload(payload)
    requiredEnv("ANTHROPIC_API_KEY")
    const openaiApiKey = requiredEnv("OPENAI_API_KEY")
    const cursorApiKey = requiredEnv("CURSOR_API_KEY")
    const githubToken = requiredEnv("GITHUB_TOKEN")
    const source = requireGitHubSource(ctx)

    await prepareGitWorkspace(ctx, githubToken)
    const prBaseBranch = resolvePullRequestBase(source, input.prBaseBranch)
    const repo = await repoSnapshot(source.resolvedSha)
    assertCleanSnapshot(repo, "light implementation workflow")

    await writeMarkdown(
      "00-light-brief.md",
      renderLightBrief(input, repo, source, ctx.run.id, prBaseBranch),
    )
    ctx.log.info({ phase: "brief", artifact: artifactPath("00-light-brief.md") })

    const implementation = await runCursor(
      input,
      cursorApiKey,
      renderLightImplementationPrompt(input, repo),
    )
    await writeMarkdown("01-light-implementation.md", implementation)
    ctx.log.info({ phase: "cursor-implementation", artifact: artifactPath("01-light-implementation.md") })

    const headBranch = await currentBranch({ previousBranch: repo.branch })
    await assertHeadEqualsBase(repo.baseSha, "light implementation phase")

    const rounds: LightReviewRound[] = []
    let finalFindingCount = Number.POSITIVE_INFINITY
    for (let round = 1; round <= input.maxReviewRounds; round += 1) {
      await assertCurrentBranch(headBranch, `review round ${round}`)
      await assertHeadEqualsBase(repo.baseSha, `review round ${round}`)
      const diff = await workingTreeDiff(repo.baseSha)
      await writeMarkdown(`02-light-round-${round}-diff.md`, diff)

      ctx.log.info({ phase: "subagent-review-start", round, maxTurns: CLAUDE_REVIEW_MAX_TURNS })
      const subagentReview = await runClaude(
        `light-subagent-review-${round}`,
        renderLightSubagentReviewPrompt(input, round, diff),
        {
          cwd: process.cwd(),
          model: input.claudeModel,
          permissionMode: "dontAsk",
          tools: ["Agent"],
          allowedTools: ["Agent"],
          agents: {
            "light-code-reviewer": {
              description: "Reviews small implementation diffs for correctness, security, and missing validation.",
              prompt: renderLightCodeReviewerSubagentPrompt(input),
              tools: ["Read", "Glob", "Grep", "LS"],
              model: input.claudeModel,
            },
          },
          maxTurns: CLAUDE_REVIEW_MAX_TURNS,
        },
      )
      ctx.log.info({ phase: "subagent-review-complete", round })
      await writeMarkdown(`03-light-round-${round}-review.md`, subagentReview)

      const codexTriage = await runCodexJson<TriageResult>(
        openaiApiKey,
        renderLightTriagePrompt(input, round, subagentReview),
        triageSchema,
        {
          model: input.codexModel,
          sandboxMode: "read-only",
          approvalPolicy: "never",
          workingDirectory: process.cwd(),
          skipGitRepoCheck: true,
          modelReasoningEffort: "medium",
        },
      )

      finalFindingCount = codexTriage.findings.length
      ctx.log.info({ phase: "triage", round, findings: finalFindingCount })

      if (finalFindingCount === 0) {
        rounds.push({ round, subagentReview, codexTriage })
        break
      }

      ctx.log.info({ phase: "cursor-fix-start", round, findings: finalFindingCount })
      const cursorFix = await runCursor(
        input,
        cursorApiKey,
        renderCursorFixPrompt(input, round, codexTriage),
      )
      ctx.log.info({ phase: "cursor-fix-complete", round })
      rounds.push({ round, subagentReview, codexTriage, cursorFix })
      await assertCurrentBranch(headBranch, `fix round ${round}`)
      await assertHeadEqualsBase(repo.baseSha, `fix round ${round}`)
      await writeMarkdown(`04-light-round-${round}-fix.md`, cursorFix)
    }

    await writeMarkdown("05-light-review-loop.md", renderLightReviewLoop(rounds))
    if (finalFindingCount !== 0) {
      const result = {
        status: "blocked",
        reason: "light review loop ended before Codex triage reached zero findings",
        runId: ctx.run.id,
        repository: source.repository,
        headBranch,
        rounds,
        artifacts: lightArtifacts(rounds),
      }
      await writeJson("light-implementation-result.json", result)
      return result
    }

    await assertCurrentBranch(headBranch, "commit phase")
    await assertHeadEqualsBase(repo.baseSha, "commit phase")
    await commitChanges(input)
    await assertCurrentBranch(headBranch, "push phase")
    await assertHeadContainsBase(repo.baseSha, "push phase")
    await pushBranch(source.repository, headBranch, githubToken)
    const pullRequest = await createOrFindPullRequest(githubToken, source, input, headBranch)

    const result = {
      status: "pr-created",
      runId: ctx.run.id,
      repository: source.repository,
      headBranch,
      prUrl: pullRequest.html_url,
      prNumber: pullRequest.number,
      rounds,
      artifacts: lightArtifacts(rounds),
    }
    await writeJson("light-implementation-result.json", result)
    return result
  },
})

const lightPayloadFields = new Set([
  "featureDesign",
  "prBaseBranch",
  "prTitle",
  "prBody",
  "maxReviewRounds",
  "claudeModel",
  "codexModel",
  "cursorModel",
])

export function normalizeLightPayload(payload: LightPayload): Input {
  assertKnownLightPayloadFields(payload)
  const input = normalizePayload(payload)
  return {
    ...input,
    operatorInput: false,
    operatorInputTimeout: 1,
    maxOperatorQuestionsPerPhase: 0,
  }
}

function assertKnownLightPayloadFields(payload: LightPayload): void {
  if (payload === null || typeof payload !== "object" || Array.isArray(payload)) {
    throw new Error("payload must be an object")
  }
  for (const field of Object.keys(payload as Record<string, unknown>)) {
    if (!lightPayloadFields.has(field)) {
      throw new Error(`payload.${field} is not supported by light-implement`)
    }
  }
}

function renderLightBrief(
  input: Input,
  repo: RepoSnapshot,
  source: { readonly repository: string; readonly requestedRef: string; readonly resolvedSha: string },
  runId: string,
  prBaseBranch: string,
): string {
  return [
    "# Light Implementation Brief",
    "",
    `Run: ${runId}`,
    `Repository: ${source.repository}`,
    `Requested ref: ${source.requestedRef}`,
    `Resolved SHA: ${source.resolvedSha}`,
    `PR base branch: ${prBaseBranch}`,
    `PR title: ${input.prTitle}`,
    `Claude review model: ${input.claudeModel}`,
    `Codex model: ${input.codexModel}`,
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

function renderLightImplementationPrompt(input: Input, repo: RepoSnapshot): string {
  return [
    "<role>",
    "Lightweight implementation phase with local workspace access.",
    "This workflow is for small, well-scoped coding tasks that do not need a separate planning or review loop.",
    "</role>",
    "",
    "<constraints>",
    "Do not inspect or expose secrets, .env files, .helmr* files, or API keys.",
    "Respect existing repository patterns, abstractions, naming, formatting, and validation style.",
    "Keep changes scoped to the feature design. Do not do broad refactors or unrelated cleanup.",
    "Prefer small, reversible edits. Add abstractions only when they remove real duplication or match an existing pattern.",
    "Before editing, inspect the relevant files and existing tests. Do not invent conventions when local examples exist.",
    "Before making code changes, checkout a new git branch with a short, descriptive, task-specific name and a unique suffix.",
    "Use a safe branch name that starts with `helmr/` and contains only letters, numbers, dots, underscores, hyphens, and slashes.",
    "Do not commit, push, or create a pull request; the workflow will do that after your response.",
    "Run the most relevant validation command available for the files you changed.",
    "If the task is larger than a light implementation, stop after exploration and explain the blocker instead of making a risky broad change.",
    "Fixes requested after review must be limited to the review findings.",
    "</constraints>",
    "",
    "<repository>",
    `Repository branch: ${repo.branch}`,
    `Repository HEAD: ${repo.head}`,
    "</repository>",
    "",
    "<feature_design>",
    input.featureDesign,
    "</feature_design>",
    "",
    "<task>",
    "Implement the requested change directly.",
    "Final response format:",
    "- Summary: what changed.",
    "- Changed files: bullet list.",
    "- Validation: commands run and outcomes.",
    "- Gaps or blockers: only real remaining issues, or `none`.",
    "</task>",
  ].join("\n")
}

function renderLightSubagentReviewPrompt(input: Input, round: number, diff: string): string {
  return [
    "<role>",
    "Lightweight review coordinator.",
    `This is review round ${round}.`,
    "</role>",
    "",
    "<constraints>",
    "Do not modify files.",
    "Do not inspect or expose secrets, .env files, .helmr* files, or API keys.",
    "You must delegate the actual code review to the `light-code-reviewer` subagent using the Agent tool.",
    "After the subagent returns, synthesize the final review from the subagent result only.",
    "</constraints>",
    "",
    "<feature_design>",
    input.featureDesign,
    "</feature_design>",
    "",
    "<diff>",
    diff,
    "</diff>",
    "",
    "<task>",
    "Ask the `light-code-reviewer` subagent to review this diff.",
    "Return markdown with these sections:",
    "1. Summary: one or two sentences.",
    "2. Findings: each finding must include severity, affected file/function if known, why it matters, and a concrete fix.",
    "3. Validation gaps: tests/checks still needed.",
    "If the subagent reports no actionable findings, write exactly: `No actionable findings.`",
    "</task>",
  ].join("\n")
}

function renderLightCodeReviewerSubagentPrompt(input: Input): string {
  return [
    "You are the `light-code-reviewer` subagent for a lightweight coding workflow.",
    "Review only the supplied feature design and diff plus any repository files needed to validate the diff.",
    "Do not modify files. Do not commit, push, or create a pull request.",
    "Do not inspect or expose secrets, .env files, .helmr* files, or API keys.",
    "",
    "Review priorities:",
    "1. Correctness, data loss, security, auth, secret handling, and permissions.",
    "2. Contract/API compatibility, concurrency, retries, and error handling.",
    "3. Missing or weak validation for behavior touched by the change.",
    "4. Maintainability issues that are likely to cause defects.",
    "Ignore style preferences and speculative improvements unless they affect correctness or operability.",
    "",
    "Feature design:",
    input.featureDesign,
  ].join("\n")
}

function renderLightTriagePrompt(input: Input, round: number, subagentReview: string): string {
  return [
    "<role>",
    "Lightweight review triage phase.",
    `Triage the subagent review from round ${round} into an evidence-based fix list for the Cursor fix phase.`,
    "</role>",
    "",
    "<constraints>",
    "Return only valid JSON matching the provided schema.",
    "Your job is to decide whether review findings are real blockers, not to preserve every reviewer concern.",
    "Include only findings that must be fixed before a PR is created.",
    "A finding must identify a concrete failure mode, affected file or behavior, and a plausible way the current diff can trigger it.",
    "Exclude false positives, speculative risks, missing ideal tests, style preferences, duplicate concerns, and requests without evidence from the diff or repository contract.",
    "If a reviewer asks for validation that has already been run and reported by the fix phase, do not keep that finding unless the reported validation is clearly insufficient.",
    "If a finding is merely about test shape or implementation preference, keep it only when it creates a concrete regression risk for the feature design.",
    "Prefer fewer, higher-confidence findings over broad or defensive issue lists.",
    "If there are no actionable findings, return an empty findings array.",
    "</constraints>",
    "",
    "<feature_design>",
    input.featureDesign,
    "</feature_design>",
    "",
    "<subagent_review>",
    subagentReview,
    "</subagent_review>",
  ].join("\n")
}

function renderLightReviewLoop(rounds: readonly LightReviewRound[]): string {
  const sections = rounds.map((round) => [
    `## Round ${round.round}`,
    "",
    "### Subagent Review",
    "",
    round.subagentReview,
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

  return ["# Light Review Loop", "", ...sections, ""].join("\n")
}

function lightArtifacts(rounds: readonly LightReviewRound[]): string[] {
  const artifacts = [
    artifactPath("00-light-brief.md"),
    artifactPath("01-light-implementation.md"),
    artifactPath("05-light-review-loop.md"),
    artifactPath("light-implementation-result.json"),
  ]
  for (const round of rounds) {
    artifacts.push(
      artifactPath(`02-light-round-${round.round}-diff.md`),
      artifactPath(`03-light-round-${round.round}-review.md`),
    )
    if (round.cursorFix) artifacts.push(artifactPath(`04-light-round-${round.round}-fix.md`))
  }
  return artifacts
}

function requiredEnv(name: string): string {
  const value = process.env[name]
  if (!value) {
    throw new Error(`${name} secret is required`)
  }
  return value
}
