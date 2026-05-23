import { image, sandbox, task } from "@helmr/sdk"
import { runClaude, runCodex, runCodexJson, runCursor, triageSchema } from "./implement/agents"
import { artifactPath, artifacts, renderFeatureDesign, renderReviewLoop, writeJson, writeMarkdown } from "./implement/artifacts"
import { createOrFindPullRequest } from "./implement/github"
import { readAuthSecrets } from "./implement/payload"
import {
  renderClaudePlanPrompt,
  renderClaudeReviewPrompt,
  renderCodexPlanPrompt,
  renderCodexReviewPrompt,
  renderCodexTriagePrompt,
  renderCursorFixPrompt,
  renderCursorExplorationPrompt,
  renderCursorImplementationPrompt,
} from "./implement/prompts"
import { commitChanges, currentBranch, inferRepository, pushBranch, repoSnapshot, workingTreeDiff } from "./implement/repo"
import { normalizePayload, type Payload } from "./implement/types"
import type { ReviewRound, TriageResult } from "./implement/types"

const base = image("helmr-implementation-workflow")
  .from("debian:trixie-slim")
  .run([
    "sh",
    "-ceu",
    [
      "apt-get update",
      "apt-get install -y --no-install-recommends git ca-certificates",
      "rm -rf /var/lib/apt/lists/*",
    ].join(" && "),
  ])

const sbx = sandbox("helmr-implementation-workflow")
  .image(base)
  .resources({ cpu: 2, memory: "4Gi" })

export const implement = task({
  id: "implement",
  sandbox: sbx,
  maxDuration: 43200,
  secrets: {
    ANTHROPIC_API_KEY: { env: "ANTHROPIC_API_KEY" },
    OPENAI_API_KEY: { env: "OPENAI_API_KEY" },
    CURSOR_API_KEY: { env: "CURSOR_API_KEY" },
    GITHUB_TOKEN: { env: "GITHUB_TOKEN" },
  },
  run: async (payload: Payload, ctx) => {
    const input = normalizePayload(payload)
    const auth = readAuthSecrets()

    const repository = input.repository ?? await inferRepository()
    const repo = await repoSnapshot()
    const rounds: ReviewRound[] = []

    await writeMarkdown("00-feature-design.md", renderFeatureDesign(input, repo, repository, ctx.run.id))
    ctx.log.info({ phase: "brief", artifact: artifactPath("00-feature-design.md") })

    const exploration = await runCursor(
      input,
      auth.cursorApiKey,
      renderCursorExplorationPrompt(input, repo),
    )
    await writeMarkdown("01-exploration.md", exploration)
    ctx.log.info({ phase: "exploration", artifact: artifactPath("01-exploration.md") })

    const claudePlan = await runClaude(
      "claude-plan",
      renderClaudePlanPrompt(input, repo, exploration),
      {
        cwd: process.cwd(),
        model: input.claudeModel,
        permissionMode: "dontAsk",
        allowedTools: ["Read", "Glob", "Grep", "LS"],
        maxTurns: 8,
      },
    )
    await writeMarkdown("02-claude-plan.md", claudePlan)
    ctx.log.info({ phase: "claude-plan", artifact: artifactPath("02-claude-plan.md") })

    const codexPlan = await runCodex(
      auth.openaiApiKey,
      renderCodexPlanPrompt(input, exploration, claudePlan),
      {
        model: input.codexModel,
        sandboxMode: "read-only",
        approvalPolicy: "never",
        workingDirectory: process.cwd(),
        skipGitRepoCheck: true,
        modelReasoningEffort: "high",
      },
    )
    await writeMarkdown("03-codex-plan-review.md", codexPlan)
    ctx.log.info({ phase: "codex-plan-review", artifact: artifactPath("03-codex-plan-review.md") })

    const cursorImplementation = await runCursor(
      input,
      auth.cursorApiKey,
      renderCursorImplementationPrompt(input, exploration, claudePlan, codexPlan),
    )
    await writeMarkdown("04-cursor-implementation.md", cursorImplementation)
    ctx.log.info({ phase: "cursor-implementation", artifact: artifactPath("04-cursor-implementation.md") })
    const headBranch = await currentBranch({ previousBranch: repo.branch })
    ctx.log.info({ phase: "branch", headBranch })

    let finalFindingCount = Number.POSITIVE_INFINITY
    for (let round = 1; round <= input.maxReviewRounds; round += 1) {
      const diff = await workingTreeDiff()
      const codexReview = await runCodex(
        auth.openaiApiKey,
        renderCodexReviewPrompt(input, round, diff),
        {
          model: input.codexModel,
          sandboxMode: "read-only",
          approvalPolicy: "never",
          workingDirectory: process.cwd(),
          skipGitRepoCheck: true,
          modelReasoningEffort: "high",
        },
      )
      const claudeReview = await runClaude(
        `claude-review-${round}`,
        renderClaudeReviewPrompt(input, round, diff),
        {
          cwd: process.cwd(),
          model: input.claudeModel,
          permissionMode: "dontAsk",
          allowedTools: ["Read", "Glob", "Grep", "LS"],
          maxTurns: 8,
        },
      )
      const codexTriage = await runCodexJson<TriageResult>(
        auth.openaiApiKey,
        renderCodexTriagePrompt(input, round, codexReview, claudeReview),
        triageSchema,
        {
          model: input.codexModel,
          sandboxMode: "read-only",
          approvalPolicy: "never",
          workingDirectory: process.cwd(),
          skipGitRepoCheck: true,
          modelReasoningEffort: "high",
        },
      )

      finalFindingCount = codexTriage.findings.length
      ctx.log.info({ phase: "triage", round, findings: finalFindingCount })

      if (finalFindingCount === 0) {
        rounds.push({ round, codexReview, claudeReview, codexTriage })
        break
      }

      const cursorFix = await runCursor(
        input,
        auth.cursorApiKey,
        renderCursorFixPrompt(input, round, codexTriage),
      )
      rounds.push({ round, codexReview, claudeReview, codexTriage, cursorFix })
      await writeMarkdown(`05-round-${round}-fix.md`, cursorFix)
    }

    await writeMarkdown("05-review-loop.md", renderReviewLoop(rounds))
    if (finalFindingCount !== 0) {
      const result = {
        status: "blocked",
        reason: "review loop ended before Codex triage reached zero findings",
        runId: ctx.run.id,
        repository,
        headBranch,
        rounds,
        artifacts: artifacts(false),
      }
      await writeJson("implementation-result.json", result)
      return result
    }

    await commitChanges(input)
    await pushBranch(repository, headBranch, auth.githubToken)
    const pullRequest = await createOrFindPullRequest(auth.githubToken, repository, input, headBranch)

    const result = {
      status: "pr-created",
      runId: ctx.run.id,
      repository,
      headBranch,
      prUrl: pullRequest.html_url,
      prNumber: pullRequest.number,
      rounds,
      artifacts: artifacts(true),
    }
    await writeJson("implementation-result.json", result)
    return result
  },
})
